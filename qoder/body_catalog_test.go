package main

import (
	"encoding/json"
	"testing"
)

func thinkingFacts(levels ...string) catalogEntry {
	enabled := &thinkingEnabled{Efforts: make(map[string]thinkingEffort, len(levels))}
	for i, lv := range levels {
		enabled.Efforts[lv] = thinkingEffort{IsDefault: i == len(levels)/2}
	}
	return catalogEntry{
		Key:            "m",
		IsReasoning:    true,
		ThinkingConfig: &thinkingConfig{Enabled: enabled, Disabled: &struct{}{}},
	}
}

// Upstream silently falls back to the model's default level for an effort it
// does not know, so the plugin has to normalize before sending — otherwise a
// request that looks honoured was quietly ignored.
func TestResolveReasoningEffort(t *testing.T) {
	facts := thinkingFacts("low", "medium", "xhigh")
	cases := []struct {
		in       string
		wantLvl  string
		wantSend bool
	}{
		{"", "", false},
		{"low", "low", true},
		{"xhigh", "xhigh", true},
		{"max", "xhigh", true},   // above the top level → nearest
		{"minimal", "low", true}, // below the bottom level → nearest
		{"high", "medium", true},
		{"none", "none", true}, // toggleable model: none disables thinking
		{"bogus", "medium", true},
	}
	for _, tc := range cases {
		lvl, send := resolveReasoningEffort(tc.in, facts)
		if lvl != tc.wantLvl || send != tc.wantSend {
			t.Fatalf("resolveReasoningEffort(%q) = (%q,%t), want (%q,%t)", tc.in, lvl, send, tc.wantLvl, tc.wantSend)
		}
	}

	// A model with no thinking config at all gets no effort parameter.
	if _, send := resolveReasoningEffort("high", catalogEntry{Key: "plain"}); send {
		t.Fatal("effort sent for a model without thinking support")
	}
}

// The request body's model_config and thinking level come from the catalog,
// not from constants: price/limits/levels drift upstream.
func TestBuildQoderBodyUsesCatalogFacts(t *testing.T) {
	entry := catalogEntry{
		Key:            "qmodel_38max",
		DisplayName:    "Qwen3.8-Max",
		Format:         "openai",
		Source:         "system",
		Enable:         true,
		IsVL:           true,
		IsReasoning:    true,
		MaxInputTokens: 180000,
		ThinkingConfig: thinkingFacts("low", "medium", "xhigh").ThinkingConfig,
	}
	catalogCache.Store(realmCN, catalogSnapshot{entries: []catalogEntry{entry}})
	t.Cleanup(func() { catalogCache.Delete(realmCN) })

	req := &openAIRequest{
		Model:           "qmodel_38max",
		ReasoningEffort: "max",
		MaxTokens:       4096,
		Messages:        []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, err := buildQoderBody(req, nil, "qmodel_38max", "personal_professional")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	var out struct {
		ModelConfig map[string]any   `json:"model_config"`
		Parameters  map[string]any   `json:"parameters"`
		CustomModel map[string]any   `json:"custom_model"`
		Business    map[string]any   `json:"business"`
		SessionType string           `json:"session_type"`
		ChatTask    string           `json:"chat_task"`
		Version     string           `json:"version"`
		Messages    []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if out.ModelConfig["key"] != "qmodel_38max" {
		t.Fatalf("model_config.key = %v", out.ModelConfig["key"])
	}
	if out.ModelConfig["display_name"] != "Qwen3.8-Max" {
		t.Fatalf("display_name = %v, want the catalog value", out.ModelConfig["display_name"])
	}
	if out.ModelConfig["is_reasoning"] != true || out.ModelConfig["is_vl"] != true {
		t.Fatalf("modality flags = %#v", out.ModelConfig)
	}
	if out.ModelConfig["max_input_tokens"] != float64(180000) {
		t.Fatalf("max_input_tokens = %v", out.ModelConfig["max_input_tokens"])
	}
	if out.Parameters["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want the nearest supported level", out.Parameters["reasoning_effort"])
	}
	if out.Parameters["enable_thinking"] != true {
		t.Fatalf("enable_thinking = %v", out.Parameters["enable_thinking"])
	}
	if out.Parameters["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %v", out.Parameters["max_tokens"])
	}
	// Verified against a working client: an empty custom_model object makes the
	// gateway treat the call as BYOK and reject model resolution, so it must be
	// omitted rather than sent empty.
	if out.CustomModel != nil {
		t.Fatalf("custom_model must be omitted, got %#v", out.CustomModel)
	}
	if out.Business["version"] != "0.1.43" {
		t.Fatalf("business.version = %v, want the template's 0.1.43 (protocol version, not client version)", out.Business["version"])
	}
	if out.Business["product"] != "cli" || out.ChatTask != "FREE_INPUT" || out.SessionType != "qodercli" {
		t.Fatalf("template identity drifted: %#v / %q / %q", out.Business, out.ChatTask, out.SessionType)
	}
	if out.Version != "3" {
		t.Fatalf("body version = %q, want the template's 3", out.Version)
	}
	if len(out.Messages) < 2 || out.Messages[0]["role"] != "system" {
		t.Fatalf("messages = %#v, want the template system prompt first", out.Messages)
	}
}

// "none" must actually disable thinking, not just be forwarded: the server
// reads enable_thinking, and a stale budget would keep reasoning on.
func TestBuildQoderBodyDisablesThinkingOnNone(t *testing.T) {
	entry := catalogEntry{
		Key: "qmodel_38max", DisplayName: "Qwen3.8-Max", Enable: true, IsReasoning: true,
		ThinkingConfig: thinkingFacts("low", "medium", "xhigh").ThinkingConfig,
	}
	catalogCache.Store(realmCN, catalogSnapshot{entries: []catalogEntry{entry}})
	t.Cleanup(func() { catalogCache.Delete(realmCN) })

	req := &openAIRequest{
		Model:           "qmodel_38max",
		ReasoningEffort: "none",
		Messages:        []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, err := buildQoderBody(req, nil, "qmodel_38max", "personal")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	var out struct {
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if out.Parameters["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %v", out.Parameters["reasoning_effort"])
	}
	if out.Parameters["enable_thinking"] != false {
		t.Fatalf("enable_thinking = %v, want false", out.Parameters["enable_thinking"])
	}
	if _, ok := out.Parameters["reasoning_budget_tokens"]; ok {
		t.Fatal("reasoning_budget_tokens must be dropped when thinking is off")
	}
}

// custom_model must be absent from the body. An empty object marks the call as
// a BYOK request with no endpoint, and upstream then answers 400 "Unsupported
// model format" for every model — the failure looks like a model problem, not
// like a body-shape problem, which is why this needs its own guard.
func TestBodyOmitsEmptyCustomModel(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "dt-test", Domain: "https://qoder.com"},
		Account: storedAccount{UID: "u1", UserType: "teams"},
	}
	req := &openAIRequest{
		Model:     "efficient",
		MaxTokens: 16,
		Messages:  []openAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	raw, err := buildQoderBody(req, sa, "efficient", "teams")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := doc["custom_model"]; present {
		t.Fatalf("custom_model must be omitted, got %s", doc["custom_model"])
	}
	// business.version is the business protocol version, not the client one.
	var biz struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(doc["business"], &biz); err != nil {
		t.Fatalf("business decode: %v", err)
	}
	if biz.Version != "0.1.43" {
		t.Fatalf("business.version = %q, want the template value", biz.Version)
	}
}
