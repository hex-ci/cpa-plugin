// desensitize.go implements the blocked-term obfuscation ("屏蔽词") surface,
// mirroring the WorkBuddy plugin: configured literal terms are matched
// case-insensitively and get a U+200B inserted after their first rune, which
// breaks exact-string matching upstream while staying invisible to the model.
//
// Scope is the same as WorkBuddy's:
//   - system / developer text, always;
//   - user text only when it carries an injected-instruction marker
//     (AGENTS.md blob, <system-reminder>, ...), so a user's own prose is left
//     alone;
//   - tool title / description metadata.
//
// Everything else in the request (assistant and tool turns, parameters) is
// untouched: rewriting those would corrupt the conversation or the generation
// knobs.
package main

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const zeroWidthSpace = "\u200b"

// defaultDesensitizeTerms mirrors the WorkBuddy list verbatim so both plugins
// behave the same for an operator who switches between them.
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

// desensitizeUserMarkers are the injected-instruction headers that mark user
// content as harness scaffolding rather than the user's own words.
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

// desensitizeConfig is the compiled runtime view of the config.
type desensitizeConfig struct {
	enabled bool
	terms   []string
	source  string
	matcher *desensitizeMatcher
}

var desensitizeRuntime atomic.Pointer[desensitizeConfig]

func init() {
	cfg, err := buildDesensitizeConfig(false, nil)
	if err != nil {
		panic(err)
	}
	desensitizeRuntime.Store(cfg)
}

func currentDesensitize() *desensitizeConfig {
	return desensitizeRuntime.Load()
}

// buildDesensitizeConfig normalises the term list and compiles the matcher.
// A nil configured slice means "use the built-in list"; an explicitly empty
// slice means an empty custom list.
func buildDesensitizeConfig(enabled bool, configured *[]string) (*desensitizeConfig, error) {
	terms, source, err := normalizeDesensitizeTerms(configured)
	if err != nil {
		return nil, err
	}
	matcher, err := compileDesensitizeMatcher(terms)
	if err != nil {
		return nil, err
	}
	return &desensitizeConfig{enabled: enabled, terms: terms, source: source, matcher: matcher}, nil
}

func normalizeDesensitizeTerms(configured *[]string) ([]string, string, error) {
	source := "custom"
	input := []string(nil)
	if configured == nil {
		source = "default"
		input = defaultDesensitizeTerms
	} else {
		input = *configured
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

// compileDesensitizeMatcher builds one case-insensitive alternation with the
// longest terms first, so a longer term wins over its own prefix.
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

// desensitizeText applies the block list to one string.
func desensitizeText(cfg *desensitizeConfig, s string) string {
	if cfg == nil || !cfg.enabled || cfg.matcher == nil {
		return s
	}
	return cfg.matcher.replace(s)
}

func hasDesensitizeUserMarker(text string) bool {
	for _, marker := range desensitizeUserMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// applyDesensitizeToBody rewrites the allowed fields of a built upstream body
// in place and reports whether anything changed.
//
// The body is a map built from the request, so message content and tools can
// still be json.RawMessage values; those are decoded, rewritten and encoded
// back so the rest of the pipeline keeps working on raw JSON.
func applyDesensitizeToBody(body map[string]any, cfg *desensitizeConfig) bool {
	if cfg == nil || !cfg.enabled {
		return false
	}
	changed := false

	if raw, ok := body["messages"]; ok {
		if next, touched := desensitizeNode(raw, cfg, true); touched {
			body["messages"] = next
			changed = true
		}
	}
	if raw, ok := body["tools"]; ok {
		if next, touched := desensitizeToolMetadata(raw, cfg); touched {
			body["tools"] = next
			changed = true
		}
	}
	// chat_prompt and chat_context mirror the latest user turn; they follow the
	// same marker rule as the user message itself.
	for _, key := range []string{"chat_prompt"} {
		if s, ok := body[key].(string); ok && hasDesensitizeUserMarker(s) {
			if next := desensitizeText(cfg, s); next != s {
				body[key] = next
				changed = true
			}
		}
	}
	if cc, ok := body["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			if s, ok := txt["text"].(string); ok && hasDesensitizeUserMarker(s) {
				if next := desensitizeText(cfg, s); next != s {
					txt["text"] = next
					changed = true
				}
			}
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				if s, ok := oc["text"].(string); ok && hasDesensitizeUserMarker(s) {
					if next := desensitizeText(cfg, s); next != s {
						oc["text"] = next
						changed = true
					}
				}
			}
		}
	}
	return changed
}

// desensitizeNode handles a value that may be a raw JSON blob, a slice or a
// map. tree applies the message rules when the node is the message list.
func desensitizeNode(value any, cfg *desensitizeConfig, tree bool) (any, bool) {
	switch node := value.(type) {
	case json.RawMessage:
		if len(node) == 0 {
			return value, false
		}
		var decoded any
		if err := json.Unmarshal(node, &decoded); err != nil {
			return value, false
		}
		next, changed := desensitizeNode(decoded, cfg, tree)
		if !changed {
			return value, false
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return value, false
		}
		return json.RawMessage(encoded), true
	case []byte:
		return desensitizeNode(json.RawMessage(node), cfg, tree)
	case []any:
		changed := false
		for i, item := range node {
			var next any
			var touched bool
			if tree {
				next, touched = desensitizeMessage(item, cfg)
			} else {
				next, touched = desensitizeNode(item, cfg, false)
			}
			if touched {
				node[i] = next
				changed = true
			}
		}
		return node, changed
	case map[string]any:
		changed := false
		for key, item := range node {
			next, touched := desensitizeNode(item, cfg, false)
			if touched {
				node[key] = next
				changed = true
			}
		}
		return node, changed
	case string:
		next := desensitizeText(cfg, node)
		return next, next != node
	}
	return value, false
}

// desensitizeMessage applies the role rules to one message object.
func desensitizeMessage(value any, cfg *desensitizeConfig) (any, bool) {
	msg, ok := value.(map[string]any)
	if !ok {
		return value, false
	}
	role, _ := msg["role"].(string)
	content, ok := msg["content"]
	if !ok {
		return value, false
	}

	switch role {
	case "system", "developer":
		return msg, desensitizeMessageContent(msg, content, cfg, true)
	case "user":
		return msg, desensitizeMessageContent(msg, content, cfg, hasUserMarkerInContent(content))
	}
	return value, false
}

// desensitizeMessageContent rewrites the text parts of one message when
// enabled; content may be a plain string or an array of parts.
func desensitizeMessageContent(msg map[string]any, content any, cfg *desensitizeConfig, enabled bool) bool {
	if !enabled {
		return false
	}
	switch value := content.(type) {
	case string:
		next := desensitizeText(cfg, value)
		if next == value {
			return false
		}
		msg["content"] = next
		return true
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return false
		}
		next, changed := desensitizeNode(decoded, cfg, false)
		if !changed {
			return false
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return false
		}
		msg["content"] = json.RawMessage(encoded)
		return true
	case []any:
		changed := false
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				continue
			}
			if text, ok := part["text"].(string); ok {
				if next := desensitizeText(cfg, text); next != text {
					part["text"] = next
					changed = true
				}
			}
		}
		return changed
	}
	return false
}

// hasUserMarkerInContent looks for an injected-instruction marker inside a
// user message, whether its content is a string or a list of parts.
func hasUserMarkerInContent(content any) bool {
	switch value := content.(type) {
	case string:
		return hasDesensitizeUserMarker(value)
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return false
		}
		return hasUserMarkerInContent(decoded)
	case []any:
		var text strings.Builder
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				continue
			}
			if s, ok := part["text"].(string); ok {
				text.WriteString(s)
			}
		}
		return hasDesensitizeUserMarker(text.String())
	}
	return false
}

// desensitizeToolMetadata rewrites tool title/description fields anywhere in
// the tools tree.
func desensitizeToolMetadata(value any, cfg *desensitizeConfig) (any, bool) {
	switch node := value.(type) {
	case json.RawMessage:
		if len(node) == 0 {
			return value, false
		}
		var decoded any
		if err := json.Unmarshal(node, &decoded); err != nil {
			return value, false
		}
		next, changed := desensitizeToolMetadata(decoded, cfg)
		if !changed {
			return value, false
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return value, false
		}
		return json.RawMessage(encoded), true
	case []any:
		changed := false
		for _, item := range node {
			if _, touched := desensitizeToolMetadata(item, cfg); touched {
				changed = true
			}
		}
		return node, changed
	case map[string]any:
		changed := false
		for key, item := range node {
			if key == "description" || key == "title" {
				if s, ok := item.(string); ok {
					if next := desensitizeText(cfg, s); next != s {
						node[key] = next
						changed = true
					}
					continue
				}
			}
			if next, touched := desensitizeToolMetadata(item, cfg); touched {
				node[key] = next
				changed = true
			}
		}
		return node, changed
	}
	return value, false
}

// decodeDesensitizeTerms parses the desensitize_terms node. A null node keeps
// the built-in list (dst stays nil); an explicit empty list is preserved.
func decodeDesensitizeTerms(val *yaml.Node, dst **[]string) error {
	if val.Kind == yaml.ScalarNode && val.Tag == "!!null" {
		*dst = nil
		return nil
	}
	if val.Kind != yaml.SequenceNode {
		return errors.New("config_yaml: desensitize_terms must be a list")
	}
	terms := make([]string, 0, len(val.Content))
	for _, item := range val.Content {
		if item.Kind != yaml.ScalarNode || item.Tag == "!!bool" || item.Tag == "!!int" || item.Tag == "!!float" {
			return errors.New("config_yaml: desensitize_terms entries must be strings")
		}
		terms = append(terms, strings.TrimSpace(item.Value))
	}
	*dst = &terms
	return nil
}

// desensitizeSettings is the read model the management API and panel use.
func desensitizeSettings() map[string]any {
	cfg := currentDesensitize()
	if cfg == nil {
		return map[string]any{"enabled": false, "terms": []string{}, "source": "default"}
	}
	return map[string]any{
		"enabled": cfg.enabled,
		"terms":   append([]string(nil), cfg.terms...),
		"source":  cfg.source,
	}
}
