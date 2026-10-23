package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func setDesensitizeForTest(t *testing.T, enabled bool, terms *[]string) *desensitizeConfig {
	t.Helper()
	old := desensitizeRuntime.Load()
	cfg, err := buildDesensitizeConfig(enabled, terms)
	if err != nil {
		t.Fatal(err)
	}
	desensitizeRuntime.Store(cfg)
	t.Cleanup(func() { desensitizeRuntime.Store(old) })
	return cfg
}

func termsPtr(terms ...string) *[]string { return &terms }

// Parity with WorkBuddy: same built-in list, off by default.
func TestDesensitizeDefaultsMatchWorkBuddy(t *testing.T) {
	cfg, err := buildDesensitizeConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.enabled {
		t.Fatal("desensitize must default off")
	}
	if cfg.source != "default" {
		t.Fatalf("source = %q", cfg.source)
	}
	if len(cfg.terms) != len(defaultDesensitizeTerms) || len(cfg.terms) != 85 {
		t.Fatalf("terms = %d, want 85", len(cfg.terms))
	}
	for _, want := range []string{"DoS", "exploit", "Claude Code", "Anthropic", "Codex", "noreply@anthropic.com"} {
		found := false
		for _, term := range cfg.terms {
			if term == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing default term %q", want)
		}
	}
}

// nil means built-in, [] means an empty custom list, a list stays custom.
func TestDesensitizeTermsDefaultCustomEmpty(t *testing.T) {
	cfg, err := buildDesensitizeConfig(true, termsPtr("  exploit  ", "exploit", "Codex", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.source != "custom" || len(cfg.terms) != 3 {
		t.Fatalf("custom = %#v", cfg.terms)
	}
	if cfg.terms[0] != "exploit" || cfg.terms[1] != "Codex" || cfg.terms[2] != "codex" {
		t.Fatalf("custom order/trim = %#v", cfg.terms)
	}

	empty, err := buildDesensitizeConfig(true, termsPtr())
	if err != nil {
		t.Fatal(err)
	}
	if empty.source != "custom" || len(empty.terms) != 0 || empty.matcher.expression != nil {
		t.Fatalf("empty list = %#v", empty)
	}

	if _, err := buildDesensitizeConfig(true, termsPtr("x")); err == nil {
		t.Fatal("single-rune term must be rejected")
	}
	if _, err := buildDesensitizeConfig(true, termsPtr("a\u200bb")); err == nil {
		t.Fatal("term containing U+200B must be rejected")
	}
}

func TestDesensitizeInsertion(t *testing.T) {
	cfg, err := buildDesensitizeConfig(true, termsPtr("attack", "attacks", "Claude Code"))
	if err != nil {
		t.Fatal(err)
	}
	// Longest alternative wins so "attacks" is not split into "attack"+"s".
	if got, want := cfg.matcher.replace("attacks"), "a"+zeroWidthSpace+"ttacks"; got != want {
		t.Fatalf("longest-first = %q, want %q", got, want)
	}
	if got := cfg.matcher.replace("ATTACK"); !strings.HasPrefix(got, "A"+zeroWidthSpace) {
		t.Fatalf("case-insensitive = %q", got)
	}
	if got := cfg.matcher.replace("claude code"); !strings.HasPrefix(got, "c"+zeroWidthSpace) {
		t.Fatalf("multi-word = %q", got)
	}
	// Idempotent: a second pass must not insert another zero-width space.
	once := cfg.matcher.replace("attack")
	if twice := cfg.matcher.replace(once); twice != once {
		t.Fatalf("not idempotent: %q vs %q", once, twice)
	}
}

// The body rewrite must follow the same scope as WorkBuddy: system/developer
// always, user only behind an injected-instruction marker, tool metadata yes,
// assistant and tool turns never.
func TestApplyDesensitizeToBodyScope(t *testing.T) {
	setDesensitizeForTest(t, true, termsPtr("attack", "exploit"))

	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "you may attack things"},
			map[string]any{"role": "developer", "content": []any{
				map[string]any{"type": "text", "text": "exploit this"},
			}},
			map[string]any{"role": "user", "content": json.RawMessage(`"please attack it"`)},
			map[string]any{"role": "assistant", "content": "attack noted"},
			map[string]any{"role": "user", "content": json.RawMessage(`[{"type":"text","text":"# AGENTS.md instructions"},{"type":"text","text":"attack here"}]`)},
			map[string]any{"role": "tool", "content": json.RawMessage(`"attack result"`)},
		},
		"tools": json.RawMessage(`[{"type":"function","function":{"name":"attack","description":"attack tool","parameters":{"title":"attack schema","description":"exploit field"}}}]`),
	}
	if !applyDesensitizeToBody(body, currentDesensitize()) {
		t.Fatal("expected changes")
	}

	msgs := body["messages"].([]any)
	system := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, "a"+zeroWidthSpace+"ttack") {
		t.Fatalf("system content untouched: %q", system)
	}
	dev := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(dev, "e"+zeroWidthSpace+"xploit") {
		t.Fatalf("developer text untouched: %q", dev)
	}
	// A user message without an injected marker is the user's own prose.
	plain := string(msgs[2].(map[string]any)["content"].(json.RawMessage))
	if strings.Contains(plain, zeroWidthSpace) {
		t.Fatalf("plain user text must be left alone: %s", plain)
	}
	// Assistant and tool turns must never be rewritten.
	if got := msgs[3].(map[string]any)["content"]; got != "attack noted" {
		t.Fatalf("assistant content rewritten: %v", got)
	}
	if got := string(msgs[5].(map[string]any)["content"].(json.RawMessage)); strings.Contains(got, zeroWidthSpace) {
		t.Fatalf("tool content rewritten: %s", got)
	}
	// The marked user message is rewritten.
	marked := string(msgs[4].(map[string]any)["content"].(json.RawMessage))
	if !strings.Contains(marked, "a"+zeroWidthSpace+"ttack here") {
		t.Fatalf("marked user text untouched: %s", marked)
	}

	tools := string(body["tools"].(json.RawMessage))
	if !strings.Contains(tools, "a"+zeroWidthSpace+"ttack tool") || !strings.Contains(tools, "e"+zeroWidthSpace+"xploit field") {
		t.Fatalf("tool metadata untouched: %s", tools)
	}
	// The tool name is routing identity, not prose.
	if strings.Contains(tools, `"name":"a\u200b`) {
		t.Fatalf("tool name must not be rewritten: %s", tools)
	}
}

// Mirage fields mirror the latest user turn, so they follow the same rule.
func TestApplyDesensitizeMirrorsFollowMarkerRule(t *testing.T) {
	setDesensitizeForTest(t, true, termsPtr("attack"))

	body := map[string]any{
		"chat_prompt": "attack now",
		"chat_context": map[string]any{
			"text": map[string]any{"text": "attack now"},
			"extra": map[string]any{
				"originalContent": map[string]any{"text": "attack now"},
			},
		},
		"business": map[string]any{"name": "attack now"},
	}
	// Unmarked text is the user's own prose: nothing should change.
	if applyDesensitizeToBody(body, currentDesensitize()) {
		t.Fatal("unmarked text must not change")
	}
	if body["chat_prompt"] != "attack now" {
		t.Fatalf("unmarked prompt must stay: %q", body["chat_prompt"])
	}

	body["chat_prompt"] = "<system-reminder>attack now"
	body["chat_context"].(map[string]any)["text"].(map[string]any)["text"] = "<system-reminder>attack now"
	body["chat_context"].(map[string]any)["extra"].(map[string]any)["originalContent"].(map[string]any)["text"] = "<system-reminder>attack now"
	if !applyDesensitizeToBody(body, currentDesensitize()) {
		t.Fatal("expected changes for marked text")
	}
	if !strings.Contains(body["chat_prompt"].(string), "a"+zeroWidthSpace+"ttack") {
		t.Fatalf("chat_prompt = %q", body["chat_prompt"])
	}
	txt := body["chat_context"].(map[string]any)["text"].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "a"+zeroWidthSpace+"ttack") {
		t.Fatalf("chat_context text = %q", txt)
	}
	oc := body["chat_context"].(map[string]any)["extra"].(map[string]any)["originalContent"].(map[string]any)["text"].(string)
	if !strings.Contains(oc, "a"+zeroWidthSpace+"ttack") {
		t.Fatalf("originalContent = %q", oc)
	}
}

// Disabled config means the body is byte-identical.
func TestDesensitizeDisabledIsNoOp(t *testing.T) {
	setDesensitizeForTest(t, false, nil)
	body := map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": "attack"}},
	}
	if applyDesensitizeToBody(body, currentDesensitize()) {
		t.Fatal("disabled config must not change the body")
	}
	if body["messages"].([]any)[0].(map[string]any)["content"] != "attack" {
		t.Fatal("content was rewritten while disabled")
	}
}

// Config parsing: the two keys survive the strict loader, and null restores the
// built-in list.
func TestConfigParsesDesensitizeKeys(t *testing.T) {
	cfg, err := loadPluginConfig([]byte("desensitize: true\ndesensitize_terms:\n  - ' attack '\n  - exploit\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Desensitize || cfg.DesensitizeTerms == nil {
		t.Fatalf("config = %#v", cfg)
	}
	if got := *cfg.DesensitizeTerms; len(got) != 2 || got[0] != "attack" {
		t.Fatalf("terms = %#v", got)
	}

	nullCfg, err := loadPluginConfig([]byte("desensitize: true\ndesensitize_terms: null\n"))
	if err != nil {
		t.Fatal(err)
	}
	if nullCfg.DesensitizeTerms != nil {
		t.Fatalf("null must keep the built-in list, got %#v", *nullCfg.DesensitizeTerms)
	}

	emptyCfg, err := loadPluginConfig([]byte("desensitize_terms: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if emptyCfg.DesensitizeTerms == nil || len(*emptyCfg.DesensitizeTerms) != 0 {
		t.Fatalf("[] must stay an empty custom list, got %#v", emptyCfg.DesensitizeTerms)
	}

	if _, err := loadPluginConfig([]byte("desensitize_terms: attack\n")); err == nil {
		t.Fatal("a scalar term list must be rejected")
	}
}
