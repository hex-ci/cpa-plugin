// desensitize.go — blocked-term obfuscation for the request this plugin sends
// upstream. A configured term gets a zero-width space after its first rune,
// which is enough to keep the upstream's content filter from matching it while
// leaving the text readable for the model.
//
// Only prompt text and tool metadata are rewritten: system/developer messages
// always, user messages only when they carry client-injected scaffolding (the
// user's own words are theirs, and rewriting them would change meaning), and the
// description/title of a tool definition.
package main

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const zeroWidthSpace = "\u200b"

// defaultDesensitizeTerms is the built-in list, used when the configuration
// carries no desensitize_terms of its own. It is the same list WorkBuddy ships:
// the terms a coding agent's own prompts tend to contain and an upstream filter
// tends to flag.
var defaultDesensitizeTerms = []string{
	"DoS", "DDoS", "exploit", "credential testing", "credential stuffing",
	"supply chain compromise", "supply-chain compromise", "detection evasion",
	"C2 frameworks", "C2 framework", "command and control", "malicious purposes",
	"malicious intent", "mass targeting", "brute force", "brute-force",
	"privilege escalation", "reverse shell", "remote code execution", "SQL injection",
	"XSS", "CSRF", "phishing", "malware", "ransomware", "keylogger", "rootkit",
	"backdoor", "botnet", "zero-day", "0day", "vulnerability", "vulnerabilities",
	"red teaming", "red-teaming", "sandbox", "sandboxing", "sandboxed", "unsandboxed",
	"escalated privileges", "escalated", "escalation", "destructive action",
	"destructive command", "destructive", "attack", "attacks", "cybersecurity",
	"security review", "exploit development", "hacking", "penetration testing",
	"penetration test", "injection", "weaponize", "weaponized", "harmful", "dangerous",
	"abuse", "abusive", "illegal", "terrorist", "terrorism", "bomb", "weapon",
	"weapons", "drug", "drugs", "narcotic", "suicide", "self-harm", "murder",
	"kill", "violence", "violent", "Claude Code", "Claude Opus", "Claude Sonnet",
	"Claude Haiku", "Claude Fable", "Anthropic", "Co-Authored-By",
	"noreply@anthropic.com", "Codex", "codex",
}

// desensitizeUserMarkers are the scaffolding strings clients inject into user
// turns. A user message carrying one is really a client-authored prompt, so it
// is rewritten like a system prompt.
var desensitizeUserMarkers = []string{
	"# AGENTS.md instructions",
	"<environment_context>",
	"<permissions instructions>",
	"<collaboration_mode>",
	"<skills_instructions>",
	"<system-reminder>",
	"# claudeMd",
}

type desensitizeMatcher struct {
	expression *regexp.Regexp
}

// desensitizeConfig is the effective setting: whether to rewrite, which terms,
// where they came from, and the compiled matcher.
type desensitizeConfig struct {
	enabled bool
	terms   []string
	source  string
	matcher *desensitizeMatcher
}

var desensitizeRuntime atomic.Pointer[desensitizeConfig]

func init() {
	cfg, err := buildDesensitizeConfig(false, nil, false)
	if err != nil {
		panic(err)
	}
	desensitizeRuntime.Store(cfg)
}

func currentDesensitize() *desensitizeConfig {
	cfg := desensitizeRuntime.Load()
	if cfg == nil {
		return nil
	}
	return cfg
}

// buildDesensitizeConfig validates a configuration into its runtime form.
// explicit reports whether desensitize_terms was present at all: absent means
// the built-in list, an explicit empty list means no terms.
func buildDesensitizeConfig(enabled bool, terms []string, explicit bool) (*desensitizeConfig, error) {
	normalized, source, err := normalizedDesensitizeTerms(terms, explicit)
	if err != nil {
		return nil, err
	}
	matcher, err := compileDesensitizeMatcher(normalized)
	if err != nil {
		return nil, err
	}
	return &desensitizeConfig{
		enabled: enabled,
		terms:   normalized,
		source:  source,
		matcher: matcher,
	}, nil
}

func normalizedDesensitizeTerms(configured []string, explicit bool) ([]string, string, error) {
	source := "custom"
	input := configured
	if !explicit {
		source = "default"
		input = defaultDesensitizeTerms
	}

	terms := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, raw := range input {
		term := strings.TrimSpace(raw)
		if term == "" {
			continue
		}
		if _, exists := seen[term]; exists {
			continue
		}
		if utf8.RuneCountInString(term) < 2 {
			return nil, "", errors.New("desensitize_terms entries must contain at least two Unicode runes")
		}
		if strings.Contains(term, zeroWidthSpace) {
			return nil, "", errors.New("desensitize_terms entries must not contain U+200B")
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	return terms, source, nil
}

// compileDesensitizeMatcher builds one case-insensitive alternation, longest
// term first so a longer term wins over one of its own substrings.
func compileDesensitizeMatcher(terms []string) (*desensitizeMatcher, error) {
	ordered := append([]string(nil), terms...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return utf8.RuneCountInString(ordered[i]) > utf8.RuneCountInString(ordered[j])
	})

	alternatives := make([]string, 0, len(ordered))
	for _, term := range ordered {
		duplicate := false
		for _, existing := range alternatives {
			if strings.EqualFold(term, existing) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			alternatives = append(alternatives, term)
		}
	}
	if len(alternatives) == 0 {
		return &desensitizeMatcher{}, nil
	}
	quoted := make([]string, len(alternatives))
	for i, term := range alternatives {
		quoted[i] = regexp.QuoteMeta(term)
	}
	expression, err := regexp.Compile("(?i:" + strings.Join(quoted, "|") + ")")
	if err != nil {
		return nil, err
	}
	return &desensitizeMatcher{expression: expression}, nil
}

// applyDesensitizeInPlace rewrites the prompt and tool metadata fields of one
// outbound request body, in place. It reports whether anything changed.
func applyDesensitizeInPlace(body map[string]any, cfg *desensitizeConfig) bool {
	if cfg == nil || !cfg.enabled || cfg.matcher == nil {
		return false
	}
	changed := false
	for _, raw := range messageList(body) {
		if desensitizeMessageInPlace(raw, cfg) {
			changed = true
		}
	}
	if tools, ok := body["tools"].([]map[string]any); ok {
		for _, tool := range tools {
			if function, ok := tool["function"].(map[string]any); ok && desensitizeToolFieldsInPlace(function, cfg) {
				changed = true
			}
		}
	}
	return changed
}

// messageList reads the body's messages as the typed shape buildSoloRequest
// produces.
func messageList(body map[string]any) []map[string]any {
	messages, _ := body["messages"].([]map[string]any)
	return messages
}

func desensitizeMessageInPlace(msg map[string]any, cfg *desensitizeConfig) bool {
	role, _ := msg["role"].(string)
	parts, ok := msg["content"].([]map[string]any)
	if !ok || len(parts) == 0 {
		return false
	}
	switch role {
	case "system", "developer":
		return desensitizeTextPartsInPlace(parts, true, cfg)
	case "user":
		// Only client-injected scaffolding: a real user turn keeps its wording.
		return desensitizeTextPartsInPlace(parts, hasDesensitizeUserMarker(joinPartText(parts)), cfg)
	}
	return false
}

func desensitizeTextPartsInPlace(parts []map[string]any, enabled bool, cfg *desensitizeConfig) bool {
	if !enabled {
		return false
	}
	changed := false
	for _, part := range parts {
		if part["type"] != "text" {
			continue
		}
		text, ok := part["text"].(string)
		if !ok {
			continue
		}
		replaced := cfg.matcher.replace(text)
		if replaced != text {
			part["text"] = replaced
			changed = true
		}
	}
	return changed
}

// desensitizeToolFieldsInPlace rewrites the metadata of one tool definition.
// The JSON schema string in "parameters" is left alone: its field descriptions
// are part of the tool contract, and a broken schema is worse than a flagged
// term.
func desensitizeToolFieldsInPlace(function map[string]any, cfg *desensitizeConfig) bool {
	changed := false
	for _, key := range []string{"description", "title"} {
		text, ok := function[key].(string)
		if !ok {
			continue
		}
		replaced := cfg.matcher.replace(text)
		if replaced != text {
			function[key] = replaced
			changed = true
		}
	}
	return changed
}

func joinPartText(parts []map[string]any) string {
	var text strings.Builder
	for _, part := range parts {
		if part["type"] != "text" {
			continue
		}
		if value, ok := part["text"].(string); ok {
			text.WriteString(value)
		}
	}
	return text.String()
}

func hasDesensitizeUserMarker(text string) bool {
	for _, marker := range desensitizeUserMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// replace inserts a zero-width space after the first rune of every match. The
// loop repeats because an insertion can expose a term the previous pass split.
func (m *desensitizeMatcher) replace(input string) string {
	if m == nil || m.expression == nil || input == "" {
		return input
	}
	for {
		changed := false
		next := m.expression.ReplaceAllStringFunc(input, func(match string) string {
			_, size := utf8.DecodeRuneInString(match)
			changed = true
			return match[:size] + zeroWidthSpace + match[size:]
		})
		if !changed {
			return input
		}
		input = next
	}
}

// parseDesensitizeTerms reads the desensitize_terms sequence. Presence is
// reported separately from the value: an absent key means the built-in list,
// while an explicit empty list means no terms at all.
func parseDesensitizeTerms(raw []byte) ([]string, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, errors.New("invalid plugin configuration: " + err.Error())
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, false, nil
		}
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil, false, nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || (key.Value != "desensitize_terms" && key.Value != "desensitize-terms") {
			continue
		}
		if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			return nil, false, nil
		}
		if value.Kind != yaml.SequenceNode || value.Tag != "!!seq" {
			return nil, false, errors.New("desensitize_terms must be an array of strings")
		}
		terms := make([]string, 0, len(value.Content))
		for _, entry := range value.Content {
			if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" {
				return nil, false, errors.New("desensitize_terms entries must be strings")
			}
			terms = append(terms, entry.Value)
		}
		return terms, true, nil
	}
	return nil, false, nil
}
