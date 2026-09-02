package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxDiscoveredModelIDBytes = 512
const modelSourceRequestTimeout = 15 * time.Second

type modelFacts struct {
	ID                        string   `json:"id"`
	Name                      string   `json:"name,omitempty"`
	Description               string   `json:"description,omitempty"`
	ContextLength             *int64   `json:"context_length,omitempty"`
	MaxCompletionTokens       *int64   `json:"max_completion_tokens,omitempty"`
	SupportedInputModalities  []string `json:"supported_input_modalities,omitempty"`
	SupportedOutputModalities []string `json:"supported_output_modalities,omitempty"`
}

type modelHTTPDo func(*http.Request, string) (*hostHTTPResponse, error)

type modelSourceFailureKind string

const (
	modelSourceTransportFailure modelSourceFailureKind = "transport"
	modelSourceHTTPFailure      modelSourceFailureKind = "http"
	modelSourceSchemaFailure    modelSourceFailureKind = "schema"
)

type modelSourceError struct {
	Kind       modelSourceFailureKind
	StatusCode int
	err        error
}

func (e *modelSourceError) Error() string {
	switch e.Kind {
	case modelSourceTransportFailure:
		return "model source transport failure"
	case modelSourceHTTPFailure:
		return fmt.Sprintf("model source HTTP %d", e.StatusCode)
	default:
		return "model source schema failure"
	}
}

func (e *modelSourceError) Unwrap() error {
	return e.err
}

type workBuddyRealm string

const (
	workBuddyRealmCN     workBuddyRealm = "cn"
	workBuddyRealmGlobal workBuddyRealm = "global"
)

type workBuddyEndpointKind string

const (
	workBuddyEndpointV3Config             workBuddyEndpointKind = "v3_config"
	workBuddyEndpointLegacyPersonalModels workBuddyEndpointKind = "legacy_personal_models"
)

type workBuddyCatalog struct {
	Realm    workBuddyRealm        `json:"realm"`
	Endpoint workBuddyEndpointKind `json:"endpoint"`
	Models   []modelFacts          `json:"models"`
}

type workBuddyAgentWire struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

type workBuddyLegacyModelWire struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Disabled      bool   `json:"disabled"`
	ContextWindow *int64 `json:"contextWindow"`
	MaxTokens     *int64 `json:"maxTokens"`
}

func parseWorkBuddyV3Config(raw []byte) ([]modelFacts, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			Agents []workBuddyAgentWire `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode v3 config: %w", err)
	}
	if response.Code == nil || *response.Code != 0 {
		return nil, fmt.Errorf("v3 config business code is not successful")
	}
	if response.Data == nil {
		return nil, fmt.Errorf("v3 config data is missing")
	}

	var modelIDs []string
	foundCLI := false
	for _, agent := range response.Data.Agents {
		if agent.Name != "cli" {
			continue
		}
		if foundCLI {
			return nil, fmt.Errorf("v3 config has multiple cli agents")
		}
		foundCLI = true
		modelIDs = agent.Models
	}
	if !foundCLI {
		return nil, fmt.Errorf("v3 config cli agent is missing")
	}

	models := make([]modelFacts, len(modelIDs))
	for i, id := range modelIDs {
		models[i].ID = id
	}
	return validateModelFacts(models)
}

func parseWorkBuddyLegacyModels(raw []byte) ([]modelFacts, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			Models []workBuddyLegacyModelWire `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode legacy models: %w", err)
	}
	if response.Code == nil || *response.Code != 0 {
		return nil, fmt.Errorf("legacy models business code is not successful")
	}
	if response.Data == nil {
		return nil, fmt.Errorf("legacy models data is missing")
	}

	models := make([]modelFacts, len(response.Data.Models))
	for i, model := range response.Data.Models {
		models[i] = modelFacts{
			ID:                  model.ID,
			Name:                model.Name,
			Description:         model.Description,
			ContextLength:       model.ContextWindow,
			MaxCompletionTokens: model.MaxTokens,
		}
	}
	models, err := validateModelFacts(models)
	if err != nil {
		return nil, err
	}
	enabled := make([]modelFacts, 0, len(models))
	for i, model := range models {
		if !response.Data.Models[i].Disabled {
			enabled = append(enabled, model)
		}
	}
	return validateModelFacts(enabled)
}

func validateModelFacts(models []modelFacts) ([]modelFacts, error) {
	if len(models) == 0 {
		return nil, fmt.Errorf("model snapshot is empty")
	}

	validated := make([]modelFacts, len(models))
	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		model.Name = strings.TrimSpace(model.Name)
		if model.ID == "" {
			return nil, fmt.Errorf("model ID is empty")
		}
		if len(model.ID) > maxDiscoveredModelIDBytes {
			return nil, fmt.Errorf("model ID exceeds %d bytes", maxDiscoveredModelIDBytes)
		}
		if _, exists := seen[model.ID]; exists {
			return nil, fmt.Errorf("model ID is duplicated")
		}
		if model.ContextLength != nil && *model.ContextLength < 0 {
			return nil, fmt.Errorf("model context length is negative")
		}
		if model.MaxCompletionTokens != nil && *model.MaxCompletionTokens < 0 {
			return nil, fmt.Errorf("model max completion tokens is negative")
		}
		if model.SupportedInputModalities != nil {
			model.SupportedInputModalities = append([]string{}, model.SupportedInputModalities...)
		}
		if model.SupportedOutputModalities != nil {
			model.SupportedOutputModalities = append([]string{}, model.SupportedOutputModalities...)
		}
		seen[model.ID] = struct{}{}
		validated[i] = model
	}
	return validated, nil
}

func fetchWorkBuddyCatalog(sa *storedAuth, callbackID string, do modelHTTPDo) (workBuddyCatalog, error) {
	if sa == nil {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: fmt.Errorf("stored auth is nil")}
	}
	realm, err := workBuddyRealmFromAccessToken(sa.Auth.AccessToken)
	if err != nil {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
	}

	base := upstreamBaseCN
	origin := originReferer
	if realm == workBuddyRealmGlobal {
		base = upstreamBaseGlobal
		origin = originRefererGlobal
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelSourceRequestTimeout)
	defer cancel()

	request := func(path string) (*hostHTTPResponse, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
		}
		backendHeaders(req, sa)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		resp, err := do(req, callbackID)
		if err != nil {
			return nil, &modelSourceError{Kind: modelSourceTransportFailure, err: err}
		}
		if resp == nil {
			return nil, &modelSourceError{Kind: modelSourceTransportFailure, err: fmt.Errorf("empty HTTP response")}
		}
		return resp, nil
	}

	resp, err := request("/v3/config")
	if err != nil {
		return workBuddyCatalog{}, err
	}
	if resp.StatusCode == http.StatusOK {
		models, err := parseWorkBuddyV3Config(resp.Body)
		if err != nil {
			return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
		}
		return workBuddyCatalog{Realm: realm, Endpoint: workBuddyEndpointV3Config, Models: models}, nil
	}
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceHTTPFailure, StatusCode: resp.StatusCode}
	}

	resp, err = request("/console/enterprises/personal/models")
	if err != nil {
		return workBuddyCatalog{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceHTTPFailure, StatusCode: resp.StatusCode}
	}
	models, err := parseWorkBuddyLegacyModels(resp.Body)
	if err != nil {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
	}
	return workBuddyCatalog{Realm: realm, Endpoint: workBuddyEndpointLegacyPersonalModels, Models: models}, nil
}

// workBuddyRealmFromAccessToken decodes unverified JWT routing facts only.
func workBuddyRealmFromAccessToken(accessToken string) (workBuddyRealm, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("decode JWT claims: %w", err)
	}
	issuer, err := url.Parse(claims.Issuer)
	if err != nil || !issuer.IsAbs() || issuer.Hostname() == "" {
		return "", fmt.Errorf("JWT issuer is not an absolute URL")
	}
	switch strings.ToLower(issuer.Hostname()) {
	case "codebuddy.cn", "www.codebuddy.cn", "copilot.tencent.com":
		return workBuddyRealmCN, nil
	case "workbuddy.ai":
		return workBuddyRealmGlobal, nil
	default:
		return "", fmt.Errorf("JWT issuer host is unsupported")
	}
}
