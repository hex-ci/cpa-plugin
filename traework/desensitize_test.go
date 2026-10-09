package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleDesensitize(t *testing.T, enabled bool, terms []string, explicit bool) *desensitizeConfig {
	t.Helper()
	cfg, err := buildDesensitizeConfig(enabled, terms, explicit)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDesensitizeTermNormalization(t *testing.T) {
	terms, source, err := normalizedDesensitizeTerms(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if source != "default" || len(terms) != len(defaultDesensitizeTerms) {
		t.Fatalf("default list = %d terms (%s), want the built-in list", len(terms), source)
	}

	// An explicit empty list means no terms, not the default list.
	terms, source, err = normalizedDesensitizeTerms([]string{}, true)
	if err != nil || source != "custom" || len(terms) != 0 {
		t.Fatalf("empty custom list = %v (%s, err %v)", terms, source, err)
	}

	// Blanks dropped, duplicates removed, surrounding space trimmed.
	terms, _, err = normalizedDesensitizeTerms([]string{" kill ", "", "kill", "bomb"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(terms) != 2 || terms[0] != "kill" || terms[1] != "bomb" {
		t.Fatalf("terms = %v, want [kill bomb]", terms)
	}

	if _, _, err := normalizedDesensitizeTerms([]string{"x"}, true); err == nil {
		t.Error("a single-rune term must be rejected")
	}
	if _, _, err := normalizedDesensitizeTerms([]string{"kill" + zeroWidthSpace}, true); err == nil {
		t.Error("a term already carrying U+200B must be rejected")
	}
}

func TestDesensitizeMatcherInsertsOneZeroWidthSpace(t *testing.T) {
	cfg := sampleDesensitize(t, true, []string{"malware", "SQL injection"}, true)
	if got := cfg.matcher.replace("Malware and SQL Injection"); got != "M\u200balware and S\u200bQL Injection" {
		t.Fatalf("replace = %q, want the break after each match's first rune", got)
	}
	// The insertion is not repeated on a second pass: the term no longer matches.
	if got := cfg.matcher.replace("SQL injection"); got != "S\u200bQL injection" {
		t.Fatalf("replace = %q, want a single insertion", got)
	}
	if got := cfg.matcher.replace("nothing to do here"); got != "nothing to do here" {
		t.Errorf("unmatched text changed: %q", got)
	}
}

// Only prompt text and tool metadata are rewritten.
func TestApplyDesensitizeInPlaceScope(t *testing.T) {
	cfg := sampleDesensitize(t, true, []string{"malware"}, true)
	body := map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": []map[string]any{{"type": "text", "text": "scan for malware"}}},
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "I saw malware"}}},
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "<system-reminder>\nmalware report"}}},
			{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "malware"}}},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{
				"name":        "scan",
				"description": "detect malware",
				"parameters":  `{"type":"object","properties":{"q":{"description":"malware query"}}}`,
			}},
		},
	}
	if !applyDesensitizeInPlace(body, cfg) {
		t.Fatal("the rewrite must report a change")
	}

	messages := body["messages"].([]map[string]any)
	text := func(i int) string {
		parts := messages[i]["content"].([]map[string]any)
		return parts[0]["text"].(string)
	}
	if text(0) != "scan for m\u200balware" {
		t.Errorf("system prompt = %q", text(0))
	}
	// A real user turn is the user's own wording: untouched.
	if text(1) != "I saw malware" {
		t.Errorf("user turn = %q, want it untouched", text(1))
	}
	// Client-injected scaffolding in a user turn is a prompt: rewritten.
	if !strings.Contains(text(2), "m\u200balware") {
		t.Errorf("scaffolded user turn = %q, want the rewrite", text(2))
	}
	if text(3) != "malware" {
		t.Errorf("assistant turn = %q, want it untouched", text(3))
	}

	fn := body["tools"].([]map[string]any)[0]["function"].(map[string]any)
	if fn["description"] != "detect m\u200balware" {
		t.Errorf("tool description = %q", fn["description"])
	}
	// The JSON schema is the tool contract: left alone.
	if !strings.Contains(fn["parameters"].(string), "malware query") {
		t.Errorf("parameters = %q, want the schema untouched", fn["parameters"])
	}
}

func TestApplyDesensitizeInPlaceDisabled(t *testing.T) {
	cfg := sampleDesensitize(t, false, nil, false)
	body := map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": []map[string]any{{"type": "text", "text": "malware"}}},
		},
	}
	if applyDesensitizeInPlace(body, cfg) {
		t.Error("a disabled setting must not rewrite anything")
	}
	if got := body["messages"].([]map[string]any)[0]["content"].([]map[string]any)[0]["text"]; got != "malware" {
		t.Errorf("text = %q, want it untouched", got)
	}
}

// The rewrite runs inside the body builder, so the marshalled request is what
// upstream sees.
func TestBuildSoloRequestAppliesDesensitize(t *testing.T) {
	cfg := sampleDesensitize(t, true, []string{"malware"}, true)
	previous := desensitizeRuntime.Load()
	desensitizeRuntime.Store(cfg)
	t.Cleanup(func() { desensitizeRuntime.Store(previous) })

	payload, err := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": "scan for malware"},
			{"role": "user", "content": "hello"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := buildSoloRequest(payload, "lite")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "m\u200balware") {
		t.Errorf("upstream body = %s, want the rewritten term", body)
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("upstream body = %s, want the user turn intact", body)
	}
}

func TestConfigureAppliesDesensitize(t *testing.T) {
	t.Cleanup(func() {
		if err := configureYAML(t, "management_key: k\n"); err != nil {
			t.Fatal(err)
		}
	})

	if err := configureYAML(t, "desensitize: true\ndesensitize_terms:\n  - kill\n  - malware\n"); err != nil {
		t.Fatal(err)
	}
	cfg := currentDesensitize()
	if !cfg.enabled || cfg.source != "custom" || len(cfg.terms) != 2 {
		t.Fatalf("cfg = %+v, want the custom list enabled", cfg)
	}

	// Terms absent: back to the built-in list.
	if err := configureYAML(t, "desensitize: true\n"); err != nil {
		t.Fatal(err)
	}
	if cfg := currentDesensitize(); cfg.source != "default" || len(cfg.terms) != len(defaultDesensitizeTerms) {
		t.Fatalf("cfg = %+v, want the built-in list", cfg)
	}

	// A term list the matcher cannot honour is a configuration error.
	if err := configureYAML(t, "desensitize: true\ndesensitize_terms:\n  - x\n"); err == nil {
		t.Error("a single-rune term must fail configuration")
	}
	// The scalar type is checked too.
	if err := configureYAML(t, "desensitize: \"yes\"\n"); err == nil {
		t.Error("a quoted string is not a boolean")
	}
	if err := configureYAML(t, "desensitize_terms: kill\n"); err == nil {
		t.Error("a scalar term list must be rejected")
	}
}
