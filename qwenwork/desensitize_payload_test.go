package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildQwenBodyDesensitizesAllowedFields(t *testing.T) {
	old := featureRuntime.Load()
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [attack, exploit]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(old) })

	var payload map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"attack"},
			{"role":"developer","content":[{"type":"text","text":"exploit"}]},
			{"role":"user","content":[{"type":"text","text":"# AGENTS.md instructions"},{"type":"text","text":"attack"}]}
		],
		"tools":[{"function":{"name":"attack","description":"attack tool","parameters":{"title":"attack schema","description":"exploit field"}}}]
	}`), &payload); err != nil {
		t.Fatal(err)
	}
	out, err := buildQwenBody(payload, "pro")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}

	// system text lands in the top-level "system" field of the qwenwork body.
	if got := body["system"]; got != "a"+zeroWidthSpace+"ttack" {
		t.Errorf("system field = %q", got)
	}
	// chat_context.text / originalContent mirror the last user text. The
	// AGENTS.md marker makes the user content eligible, so the folded copy
	// is desensitized as well.
	chatContext := body["chat_context"].(map[string]any)
	if got := chatContext["text"].(string); !strings.Contains(got, "a"+zeroWidthSpace+"ttack") {
		t.Errorf("chat_context.text = %q, want desensitized attack", got)
	}
	messages := body["messages"].([]any)
	userText := messages[len(messages)-1].(map[string]any)["content"].([]any)[1].(map[string]any)["text"]
	if got := userText; got != "a"+zeroWidthSpace+"ttack" {
		t.Errorf("marked user text = %q", got)
	}
	tools := body["tools"].([]any)
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if got := function["description"]; got != "a"+zeroWidthSpace+"ttack tool" {
		t.Errorf("tool description = %q", got)
	}
	parameters := function["parameters"].(map[string]any)
	if got := parameters["title"]; got != "a"+zeroWidthSpace+"ttack schema" {
		t.Errorf("tool title = %q", got)
	}
	if got := parameters["description"]; got != "e"+zeroWidthSpace+"xploit field" {
		t.Errorf("tool nested description = %q", got)
	}
}

func TestBuildQwenBodyLeavesDisallowedFieldsUnchanged(t *testing.T) {
	old := featureRuntime.Load()
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [attack]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(old) })

	var payload map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"m",
		"messages":[
			{"role":"developer","content":[{"type":"image_url","text":"attack","image_url":{"url":"attack"}}]},
			{"role":"user","content":"ordinary attack"},
			{"role":"user","content":[{"type":"text","text":"<System-Reminder> attack"}]},
			{"role":"assistant","content":"attack"},
			{"role":"tool","content":"attack"}
		],
		"tools":[{"function":{"name":"attack","parameters":{"enum":["attack"],"default":"attack","example":"attack"}}}]
	}`), &payload); err != nil {
		t.Fatal(err)
	}
	out, err := buildQwenBody(payload, "pro")
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(out)
	for _, want := range []string{
		`"text":"attack","type":"image_url"`,
		`"url":"attack"`,
		`"name":"attack"`,
		`"enum":["attack"]`,
		`"default":"attack"`,
		`"example":"attack"`,
	} {
		if !strings.Contains(encoded, want) {
			t.Errorf("disallowed field changed or missing %q in %s", want, encoded)
		}
	}
	// Ordinary user text without markers must never be rewritten.
	if strings.Contains(encoded, "ordinary a"+zeroWidthSpace+"ttack") {
		t.Errorf("unmarked user text was desensitized")
	}
}

func TestDesensitizeUserMarkersAreExactAndCaseSensitive(t *testing.T) {
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [attack]\n"))
	if err != nil {
		t.Fatal(err)
	}
	exact := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "<system-reminder> attack"}}}
	wrong := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "<System-Reminder> attack"}}}
	applyDesensitizeInPlace(exact, cfg)
	applyDesensitizeInPlace(wrong, cfg)
	if got := exact["messages"].([]any)[0].(map[string]any)["content"]; got != "<system-reminder> a"+zeroWidthSpace+"ttack" {
		t.Errorf("exact marker content = %q", got)
	}
	if got := wrong["messages"].([]any)[0].(map[string]any)["content"]; got != "<System-Reminder> attack" {
		t.Errorf("case-variant marker content = %q", got)
	}
}

func TestBuildQwenBodyDoesNotDesensitizeWhenDisabled(t *testing.T) {
	old := featureRuntime.Load()
	cfg, err := parseFeatureRuntime([]byte("desensitize: false\ndesensitize_terms: [attack]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(old) })

	var payload map[string]any
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"system","content":"attack"}]}`), &payload); err != nil {
		t.Fatal(err)
	}
	out, err := buildQwenBody(payload, "pro")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "a"+zeroWidthSpace+"ttack") {
		t.Fatalf("disabled desensitize changed payload: %s", out)
	}
}

func TestBuildQwenBodyDesensitizesNestedToolMetadataBelowStructuredMetadata(t *testing.T) {
	old := featureRuntime.Load()
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [attack]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(old) })

	var payload map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"m",
		"tools":[{"function":{
			"description":{"field":{"description":"attack"}},
			"title":[{"title":"attack"}]
		}}]
	}`), &payload); err != nil {
		t.Fatal(err)
	}
	out, err := buildQwenBody(payload, "pro")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	function := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	description := function["description"].(map[string]any)["field"].(map[string]any)["description"]
	if description != "a"+zeroWidthSpace+"ttack" {
		t.Errorf("nested description = %q", description)
	}
	title := function["title"].([]any)[0].(map[string]any)["title"]
	if title != "a"+zeroWidthSpace+"ttack" {
		t.Errorf("nested title = %q", title)
	}
}
