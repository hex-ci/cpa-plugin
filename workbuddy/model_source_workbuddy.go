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

// workBuddyModelEntryWire is one entry of the upstream model catalog. It covers
// BOTH catalog shapes: /v3/config's data.models[] and the fuller
// /console/enterprises/{personal|<id>}/models payload. Field names follow the
// upstream's actual keys (maxOutputTokens / maxInputTokens) — an earlier
// revision used maxTokens / contextWindow, which never matched anything and
// left every limit nil, silently falling back to models.dev.
type workBuddyModelEntryWire struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	DescriptionEn string `json:"descriptionEn"`
	DescriptionZh string `json:"descriptionZh"`
	Disabled      bool   `json:"disabled"`
	MaxOutput     *int64 `json:"maxOutputTokens"`
	MaxInput      *int64 `json:"maxInputTokens"`
}

// fact converts a catalog entry into modelFacts. The upstream sends the display
// name plus a localized description under descriptionEn/descriptionZh (the flat
// "description" field only exists in some older shapes), so each is resolved
// from whichever field the catalog actually populated.
func (m workBuddyModelEntryWire) fact() modelFacts {
	f := modelFacts{
		ID:   m.ID,
		Name: firstNonEmpty(m.Name, m.DescriptionEn),
		// maxInputTokens is the model's input window; the client uses it as the
		// context length (maxAllowedSize is a separate, larger allowance).
		Description:         firstNonEmpty(m.Description, m.DescriptionEn, m.DescriptionZh),
		ContextLength:       m.MaxInput,
		MaxCompletionTokens: m.MaxOutput,
	}
	return f
}

// workBuddyAgentWire is the cli agent entry; its models[] is a plain ID list.
type workBuddyAgentWire struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

// parseWorkBuddyV3Config reads the /v3/config payload. The cli agent's models[]
// is the authoritative ORDERED id list; data.models[] carries the per-model
// limits, so the two are joined by id (limits are evidence-based, ids are the
// roster — the roster wins on membership, the detail map only enriches).
func parseWorkBuddyV3Config(raw []byte) ([]modelFacts, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			Agents []workBuddyAgentWire      `json:"agents"`
			Models []workBuddyModelEntryWire `json:"models"`
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

	details := make(map[string]workBuddyModelEntryWire, len(response.Data.Models))
	for _, entry := range response.Data.Models {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		details[id] = entry
	}

	models := make([]modelFacts, 0, len(modelIDs))
	for _, id := range modelIDs {
		entry, ok := details[strings.TrimSpace(id)]
		if !ok {
			// Roster id with no detail row: keep it, limits stay nil.
			models = append(models, modelFacts{ID: id})
			continue
		}
		models = append(models, entry.fact())
	}
	return validateModelFacts(models)
}

// parseWorkBuddyLegacyModels reads the /console/enterprises/.../models payload,
// whose data.models[] carries the same entry shape (the fuller variant also
// includes iconUrl/isDefault/top_k, which this parser deliberately ignores).
func parseWorkBuddyLegacyModels(raw []byte) ([]modelFacts, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			Models []workBuddyModelEntryWire `json:"models"`
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
		models[i] = model.fact()
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
	if len(enabled) == 0 {
		return nil, fmt.Errorf("model snapshot is empty")
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

	// requestAsDesktop 与 request 同源，只把客户端身份换成官方桌面端。
	// 仅在 desktop_model_discovery 开启时才会被调用。
	requestAsDesktop := func(path string) (*hostHTTPResponse, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
		}
		backendHeaders(req, sa)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", desktopClientUA)
		req.Header.Set("X-IDE-Type", "WorkBuddy")
		req.Header.Set("X-IDE-Name", "WorkBuddy")
		req.Header.Set("X-IDE-Version", desktopIDEVersion)
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
		models = mergeDesktopModels(requestAsDesktop, "/v3/config", models)
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
	models = mergeDesktopModels(requestAsDesktop, "/console/enterprises/personal/models", models)
	return workBuddyCatalog{Realm: realm, Endpoint: workBuddyEndpointLegacyPersonalModels, Models: models}, nil
}

// mergeDesktopModels 在 desktop_model_discovery 开启时，以官方桌面端身份再抓一次
// 同一端点并合并名单；开关关闭（默认）时原样返回，不发出任何额外请求。
//
// 上游目录接口按请求身份返回不同名单，两个身份各有对方看不到的条目，
// 因此需要并集。补充请求是尽力而为的：任何失败都只保留主名单，
// 绝不会让整轮目录抓取失败。
func mergeDesktopModels(
	requestAsDesktop func(string) (*hostHTTPResponse, error), path string, primary []modelFacts,
) []modelFacts {
	cfg := currentFeatureRuntime()
	if cfg == nil || !cfg.desktopModelDiscovery {
		return primary
	}
	resp, err := requestAsDesktop(path)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		return primary
	}
	desktop, err := parseWorkBuddyV3Config(resp.Body)
	if err != nil || len(desktop) == 0 {
		// 同一 URL 在不同身份下可能改走 legacy 形状，再试一次。
		desktop, err = parseWorkBuddyLegacyModels(resp.Body)
		if err != nil || len(desktop) == 0 {
			return primary
		}
	}
	return mergeModelFacts(primary, desktop)
}

// mergeModelFacts 按 ID 合并两份名单：主名单在前（保留其顺序与已解析限额），
// 补充名单中新的 ID 追加在后；重复 ID 保留先出现者，避免丢失已有详情。
func mergeModelFacts(primary, extra []modelFacts) []modelFacts {
	if len(extra) == 0 {
		return primary
	}
	merged := make([]modelFacts, 0, len(primary)+len(extra))
	seen := make(map[string]struct{}, len(primary)+len(extra))
	for _, model := range primary {
		key := strings.TrimSpace(model.ID)
		if key == "" {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, model)
	}
	for _, model := range extra {
		key := strings.TrimSpace(model.ID)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, model)
	}
	if len(merged) == 0 {
		return primary
	}
	return merged
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
	switch {
	case isGlobalDomain(issuer.Hostname()):
		return workBuddyRealmGlobal, nil
	}
	switch strings.ToLower(issuer.Hostname()) {
	case "codebuddy.cn", "www.codebuddy.cn", "copilot.tencent.com":
		return workBuddyRealmCN, nil
	default:
		return "", fmt.Errorf("JWT issuer host is unsupported")
	}
}
