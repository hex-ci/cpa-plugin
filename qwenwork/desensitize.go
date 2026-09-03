package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"gopkg.in/yaml.v3"
)

const zeroWidthSpace = "​"

// defaultDesensitizeTerms mirrors the WorkBuddy plugin's built-in list: CLI
// client fingerprints (Claude Code / Codex identities) plus security-violence
// vocabulary that upstream moderation may flag. Ported 1:1 so both plugins
// obfuscate the same surface.
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

type desensitizeMatcher struct {
	expression *regexp.Regexp
}

type featureRuntimeConfig struct {
	desensitizeEnabled bool
	desensitizeTerms   []string
	desensitizeSource  string
	matcher            *desensitizeMatcher
}

var featureRuntime atomic.Pointer[featureRuntimeConfig]

func init() {
	cfg, err := parseFeatureRuntime(nil)
	if err != nil {
		panic(err)
	}
	featureRuntime.Store(cfg)
}

func currentFeatureRuntime() *featureRuntimeConfig {
	cfg := featureRuntime.Load()
	if cfg == nil {
		return nil
	}
	snapshot := *cfg
	snapshot.desensitizeTerms = append([]string(nil), cfg.desensitizeTerms...)
	return &snapshot
}

// commitFeatureRuntime publishes a fully parsed feature config. Called at the
// end of configure() after every other setting applied successfully.
func commitFeatureRuntime(cfg *featureRuntimeConfig) {
	featureRuntime.Store(cfg)
}

type featureConfigYAML struct {
	Desensitize      *bool     `yaml:"desensitize"`
	DesensitizeTerms *[]string `yaml:"desensitize_terms"`
}

func parseFeatureRuntime(raw []byte) (*featureRuntimeConfig, error) {
	var doc featureConfigYAML
	if strings.TrimSpace(string(raw)) != "" {
		if _, err := parseValidatedConfigRoot(raw); err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, errors.New("invalid config_yaml")
		}
	}
	terms, source, err := normalizedDesensitizeTerms(doc.DesensitizeTerms)
	if err != nil {
		return nil, err
	}
	matcher, err := compileDesensitizeMatcher(terms)
	if err != nil {
		return nil, err
	}
	return &featureRuntimeConfig{
		desensitizeEnabled: doc.Desensitize != nil && *doc.Desensitize,
		desensitizeTerms:   terms,
		desensitizeSource:  source,
		matcher:            matcher,
	}, nil
}

func normalizedDesensitizeTerms(configured *[]string) ([]string, string, error) {
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

var desensitizeUserMarkers = []string{
	"# AGENTS.md instructions",
	"<environment_context>",
	"<permissions instructions>",
	"<collaboration_mode>",
	"<skills_instructions>",
	"<system-reminder>",
	"# claudeMd",
}

// applyDesensitizeInPlace changes only the prompt and tool metadata fields
// allowed by the configured desensitization scope. Call it on the OpenAI-style
// payload before buildQwenBody folds it into the qwenwork request.
//
// When a mutation happens, it emits one host.log line carrying the number of
// zero-width-space insertions (never the matched terms or payload text) so
// operators can verify the filter actually runs on live traffic.
func applyDesensitizeInPlace(obj map[string]any, cfg *featureRuntimeConfig) bool {
	if cfg == nil || !cfg.desensitizeEnabled || cfg.matcher == nil {
		return false
	}
	beforeHits := 0
	if before, err := json.Marshal(obj); err == nil {
		beforeHits = strings.Count(string(before), zeroWidthSpace)
	}
	changed := false
	if messages, ok := obj["messages"].([]any); ok {
		for _, raw := range messages {
			if msg, ok := raw.(map[string]any); ok && desensitizeMessageInPlace(msg, cfg) {
				changed = true
			}
		}
	}
	if tools, ok := obj["tools"]; ok && desensitizeToolMetadataInPlace(tools, cfg) {
		changed = true
	}
	if changed {
		afterHits := 0
		if after, err := json.Marshal(obj); err == nil {
			afterHits = strings.Count(string(after), zeroWidthSpace)
		}
		added := afterHits - beforeHits
		if added < 0 {
			added = 0
		}
		logDesensitizeHits(added)
	}
	return changed
}

// logDesensitizeHits reports filter activity via host.log (best effort:
// never fails a request because logging is unavailable). The log line carries
// only the insertion count — no matched terms, no payload text.
func logDesensitizeHits(hits int) {
	req, _ := json.Marshal(map[string]any{
		"level":   "info",
		"message": fmt.Sprintf("desensitize: filter applied (hits=%d)", hits),
	})
	_, _ = hostCall(pluginabi.MethodHostLog, req)
}

func desensitizeMessageInPlace(msg map[string]any, cfg *featureRuntimeConfig) bool {
	role, _ := msg["role"].(string)
	content, ok := msg["content"]
	if !ok {
		return false
	}
	switch role {
	case "system", "developer":
		switch value := content.(type) {
		case string:
			return desensitizeStringField(msg, "content", value, cfg)
		case []any:
			return desensitizeTextBlocksInPlace(value, true, cfg)
		}
	case "user":
		switch value := content.(type) {
		case string:
			if hasDesensitizeUserMarker(value) {
				return desensitizeStringField(msg, "content", value, cfg)
			}
		case []any:
			var text strings.Builder
			for _, raw := range value {
				part, ok := raw.(map[string]any)
				if !ok || part["type"] != "text" {
					continue
				}
				if value, ok := part["text"].(string); ok {
					text.WriteString(value)
				}
			}
			return desensitizeTextBlocksInPlace(value, hasDesensitizeUserMarker(text.String()), cfg)
		}
	}
	return false
}

func desensitizeStringField(obj map[string]any, key, value string, cfg *featureRuntimeConfig) bool {
	replaced := cfg.matcher.replace(value)
	if replaced == value {
		return false
	}
	obj[key] = replaced
	return true
}

func desensitizeTextBlocksInPlace(content []any, enabled bool, cfg *featureRuntimeConfig) bool {
	if !enabled {
		return false
	}
	changed := false
	for _, raw := range content {
		part, ok := raw.(map[string]any)
		if !ok || part["type"] != "text" {
			continue
		}
		if text, ok := part["text"].(string); ok && desensitizeStringField(part, "text", text, cfg) {
			changed = true
		}
	}
	return changed
}

func desensitizeToolMetadataInPlace(value any, cfg *featureRuntimeConfig) bool {
	switch node := value.(type) {
	case map[string]any:
		changed := false
		for key, value := range node {
			if key == "description" || key == "title" {
				if text, ok := value.(string); ok {
					if desensitizeStringField(node, key, text, cfg) {
						changed = true
					}
					continue
				}
			}
			if desensitizeToolMetadataInPlace(value, cfg) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, item := range node {
			if desensitizeToolMetadataInPlace(item, cfg) {
				changed = true
			}
		}
		return changed
	}
	return false
}

func hasDesensitizeUserMarker(text string) bool {
	for _, marker := range desensitizeUserMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
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
