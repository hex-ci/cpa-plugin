package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const modelsDevMetadataURL = "https://models.dev/models.json"

type modelsDevLimitWire struct {
	Context *int64 `json:"context"`
	Output  *int64 `json:"output"`
}

type modelsDevModalitiesWire struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type modelsDevModelWire struct {
	ID         string                   `json:"id"`
	Name       string                   `json:"name"`
	Limit      *modelsDevLimitWire      `json:"limit"`
	Modalities *modelsDevModalitiesWire `json:"modalities"`
}

type modelsDevFetchResult struct {
	NotModified bool
	ETag        string
	Records     map[string]modelFacts
}

func parseModelsDevMetadata(raw []byte) (map[string]modelFacts, error) {
	var wire map[string]modelsDevModelWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("decode models.dev metadata: %w", err)
	}
	if len(wire) == 0 {
		return nil, fmt.Errorf("models.dev metadata is empty")
	}

	records := make(map[string]modelFacts, len(wire))
	for canonicalID, model := range wire {
		if !validModelsDevCanonicalID(canonicalID) {
			return nil, fmt.Errorf("models.dev canonical ID is invalid")
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" || len(modelID) > maxDiscoveredModelIDBytes {
			return nil, fmt.Errorf("models.dev model ID is invalid")
		}

		facts := modelFacts{ID: canonicalID, Name: model.Name}
		if model.Limit != nil {
			facts.ContextLength = cloneInt64(model.Limit.Context)
			facts.MaxCompletionTokens = cloneInt64(model.Limit.Output)
		}
		if model.Modalities != nil {
			facts.SupportedInputModalities = model.Modalities.Input
			facts.SupportedOutputModalities = model.Modalities.Output
		}
		facts, err := validateModelsDevCanonicalRecord(canonicalID, facts)
		if err != nil {
			return nil, err
		}
		records[canonicalID] = facts
	}
	return records, nil
}

func validateModelsDevCanonicalRecord(canonicalID string, facts modelFacts) (modelFacts, error) {
	if !validModelsDevCanonicalID(canonicalID) || facts.ID != canonicalID {
		return modelFacts{}, fmt.Errorf("models.dev canonical record identity is invalid")
	}
	if facts.Description != "" {
		return modelFacts{}, fmt.Errorf("models.dev canonical description is unsupported")
	}
	facts.Name = strings.TrimSpace(facts.Name)
	if facts.ContextLength != nil && *facts.ContextLength < 0 {
		return modelFacts{}, fmt.Errorf("models.dev context limit is negative")
	}
	if facts.MaxCompletionTokens != nil && *facts.MaxCompletionTokens < 0 {
		return modelFacts{}, fmt.Errorf("models.dev output limit is negative")
	}
	var err error
	facts.SupportedInputModalities, err = validateModelsDevModalities(facts.SupportedInputModalities)
	if err != nil {
		return modelFacts{}, err
	}
	facts.SupportedOutputModalities, err = validateModelsDevModalities(facts.SupportedOutputModalities)
	if err != nil {
		return modelFacts{}, err
	}
	return facts, nil
}

func validModelsDevCanonicalID(id string) bool {
	if id == "" || len(id) > maxDiscoveredModelIDBytes || id != strings.TrimSpace(id) {
		return false
	}
	segments := strings.Split(id, "/")
	if len(segments) < 2 {
		return false
	}
	for _, segment := range segments {
		if strings.TrimSpace(segment) == "" {
			return false
		}
	}
	return true
}

func validateModelsDevModalities(values []string) ([]string, error) {
	if values == nil {
		return nil, nil
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("models.dev modality list is empty")
	}
	validated := make([]string, len(values))
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("models.dev modality is empty")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("models.dev modality is duplicated")
		}
		seen[value] = struct{}{}
		validated[i] = value
	}
	return validated, nil
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func matchModelsDevRecord(rawID string, records map[string]modelFacts) *modelFacts {
	if record, ok := records[rawID]; ok {
		return &record
	}
	var match *modelFacts
	for canonicalID, record := range records {
		if canonicalID[strings.LastIndex(canonicalID, "/")+1:] != rawID {
			continue
		}
		if match != nil {
			return nil
		}
		copy := record
		match = &copy
	}
	return match
}

func fetchModelsDevMetadata(etag, callbackID string, do modelHTTPDo) (modelsDevFetchResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), modelSourceRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevMetadataURL, nil)
	if err != nil {
		return modelsDevFetchResult{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
	}
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := do(req, callbackID)
	if err != nil {
		return modelsDevFetchResult{}, &modelSourceError{Kind: modelSourceTransportFailure, err: err}
	}
	if resp == nil {
		return modelsDevFetchResult{}, &modelSourceError{Kind: modelSourceTransportFailure, err: fmt.Errorf("empty HTTP response")}
	}
	responseETag := resp.Headers.Get("ETag")
	if responseETag == "" {
		for key, values := range resp.Headers {
			if strings.EqualFold(key, "ETag") && len(values) > 0 {
				responseETag = values[0]
				break
			}
		}
	}
	if resp.StatusCode == http.StatusNotModified {
		return modelsDevFetchResult{NotModified: true, ETag: responseETag}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return modelsDevFetchResult{}, &modelSourceError{Kind: modelSourceHTTPFailure, StatusCode: resp.StatusCode}
	}
	records, err := parseModelsDevMetadata(resp.Body)
	if err != nil {
		return modelsDevFetchResult{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
	}
	return modelsDevFetchResult{ETag: responseETag, Records: records}, nil
}

func cloneModelFacts(facts modelFacts) modelFacts {
	facts.ContextLength = cloneInt64(facts.ContextLength)
	facts.MaxCompletionTokens = cloneInt64(facts.MaxCompletionTokens)
	facts.SupportedInputModalities = append([]string(nil), facts.SupportedInputModalities...)
	facts.SupportedOutputModalities = append([]string(nil), facts.SupportedOutputModalities...)
	return facts
}

func fillMissingModelFacts(dst *modelFacts, src modelFacts) {
	if dst.Name == "" {
		dst.Name = src.Name
	}
	if dst.Description == "" {
		dst.Description = src.Description
	}
	if dst.ContextLength == nil {
		dst.ContextLength = cloneInt64(src.ContextLength)
	}
	if dst.MaxCompletionTokens == nil {
		dst.MaxCompletionTokens = cloneInt64(src.MaxCompletionTokens)
	}
	if len(dst.SupportedInputModalities) == 0 {
		dst.SupportedInputModalities = append([]string(nil), src.SupportedInputModalities...)
	}
	if len(dst.SupportedOutputModalities) == 0 {
		dst.SupportedOutputModalities = append([]string(nil), src.SupportedOutputModalities...)
	}
}

func modelInfoFromSources(serving modelFacts, canonical *modelFacts) pluginapi.ModelInfo {
	merged := cloneModelFacts(serving)
	if canonical != nil {
		fillMissingModelFacts(&merged, *canonical)
	}
	info := defaultModelInfo(serving.ID, merged.Name)
	info.Description = merged.Description
	if merged.ContextLength != nil {
		info.ContextLength = *merged.ContextLength
	}
	if merged.MaxCompletionTokens != nil {
		info.MaxCompletionTokens = *merged.MaxCompletionTokens
	}
	info.SupportedInputModalities = append([]string(nil), merged.SupportedInputModalities...)
	info.SupportedOutputModalities = append([]string(nil), merged.SupportedOutputModalities...)
	return info
}
