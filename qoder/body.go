// body.go constructs the agent_chat_generation request body from an
// OpenAI-style chat completion request.
//
// The base template (baseprompt.json) is the official CLI's own body snapshot:
// chat_task/version/source/session_type and the system prompt all come from it.
// Per request we overwrite the ids, the model block (from the live catalog),
// the conversation, and the generation parameters.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// cpaToUpstreamKey maps CPA-facing model names to upstream catalog keys.
// Unknown names pass through unchanged — upstream routes unknown keys to its
// default model, and the catalog lookup then falls back to defaults.
func cpaToUpstreamKey(cpaModel string) string {
	switch strings.ToLower(strings.TrimSpace(cpaModel)) {
	case "qoder-auto", "auto":
		return "auto"
	case "qwen3.8-max", "qwen3.8-max-preview", "qmodel_preview", "qmodel_38max":
		return "qmodel_38max"
	case "qwen3.8-flash", "qfmodel":
		return "qfmodel"
	case "qwen3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qmodel":
		return "qmodel"
	case "qwen3.7-flash", "q37fmodel":
		return "q37fmodel"
	case "qwen3.6-flash", "q36fmodel":
		return "q36fmodel"
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-v4-flash", "deepseek-flash", "dfmodel":
		return "dfmodel"
	case "glm-5.3", "gmodel":
		return "gmodel"
	case "glm-5.3-flash", "gfmodel":
		return "gfmodel"
	case "glm-5.2", "gm51model":
		return "gm51model"
	case "kimi-k3", "kmodel_latest":
		return "kmodel_latest"
	case "kimi-k2.8-preview", "kimi-k2.7-code", "kmodel":
		return "kmodel"
	case "minimax-m3", "minimax-m2.7", "mmodel":
		return "mmodel"
	}
	return strings.TrimSpace(cpaModel)
}

// openAIMessage is one message in the OpenAI chat completion format.
//
// Content is json.RawMessage so multi-modal messages whose content is a part
// array decode instead of failing the whole request. ToolCalls / ToolCallID
// are kept so assistant tool-call turns and their matching tool results
// survive the round trip.
type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	Stream              bool            `json:"stream"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	MaxTokens           int64           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int64           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
}

// contentText renders a message content for use as a plain prompt string.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// extractLatestUserPrompt returns the text of the last user message.
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			if text := contentText(messages[i].Content); text != "" {
				return text
			}
		}
	}
	return ""
}

// effortRank orders effort levels from weakest to strongest.
var effortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// offEffortValues are the spellings clients use to disable thinking.
var offEffortValues = map[string]bool{
	"none": true, "off": true, "disabled": true, "disable": true,
	"false": true, "0": true, "no": true,
}

// resolveReasoningEffort maps a requested effort onto what the model actually
// accepts.
//
// Upstream silently falls back to the model's default level when handed a
// level it does not know, so a request that looks honoured may have been
// ignored. Returns ("", false) when nothing should be sent.
func resolveReasoningEffort(requested string, facts catalogEntry) (string, bool) {
	level := strings.ToLower(strings.TrimSpace(requested))
	if level == "" {
		return "", false
	}
	if offEffortValues[level] {
		if facts.thinkingToggleable() {
			return "none", true
		}
		if len(facts.ThinkingConfigLevels()) == 0 {
			// Model has no thinking control at all: nothing to send.
			return "", false
		}
	}
	levels, def := facts.thinkingLevels()
	if len(levels) == 0 {
		if facts.thinkingToggleable() {
			// Only on/off is supported: anything but "off" means "leave default".
			return "", false
		}
		return "", false
	}
	if rank, ok := effortRank[level]; ok {
		best := ""
		bestDist := 1 << 30
		for _, lv := range levels {
			lvRank, ok := effortRank[lv]
			if !ok {
				continue
			}
			dist := lvRank - rank
			if dist < 0 {
				dist = -dist
			}
			if dist < bestDist || (dist == bestDist && lv == def) {
				best, bestDist = lv, dist
			}
		}
		if best != "" {
			return best, true
		}
	}
	return def, def != ""
}

// ThinkingConfigLevels returns the effort levels of a catalog entry (helper so
// callers do not have to nil-check the nested config).
func (e catalogEntry) ThinkingConfigLevels() []string {
	levels, _ := e.thinkingLevels()
	return levels
}

// deepCopyMap copies a JSON object so later mutations of the original (or of
// the copy) cannot leak into the other.
func deepCopyMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// buildQoderBody renders the upstream agent_chat_generation body for one
// request. modelKey is the upstream catalog key; sa supplies the realm catalog
// that drives model_config and the thinking levels.
func buildQoderBody(req *openAIRequest, sa *storedAuth, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	facts, _ := modelFacts(sa, modelKey)

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"
	// custom_model must be absent, not empty: an empty object tells upstream
	// this is a BYOK request with no endpoint, and the model resolution then
	// fails with "Unsupported model format" on every model.

	// model_config is catalog-driven: display name, modality flags and the
	// input limit all come from upstream, not from constants.
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
		if facts.Key != "" {
			mc["display_name"] = firstNonEmpty(facts.DisplayName, modelKey)
			mc["format"] = firstNonEmpty(facts.Format, "openai")
			mc["source"] = firstNonEmpty(facts.Source, "system")
			mc["is_vl"] = facts.IsVL
			mc["is_reasoning"] = facts.IsReasoning
			mc["max_input_tokens"] = firstNonZero64(facts.MaxInputTokens, facts.contextWindow())
		} else {
			mc["display_name"] = modelKey
		}
	}

	// chat_context mirrors the latest user turn.
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			// chat_context carries a full copy of model_config: upstream reads
			// model identity from here, and a partial object leaves it at the
			// template's model ("lite") so the gateway picks the wrong node.
			if top, ok := base["model_config"].(map[string]any); ok {
				extra["modelConfig"] = deepCopyMap(top)
			}
		}
	}

	// messages: keep the template's system prompt, then the conversation.
	var msgs []any
	if baseMsgs, ok := base["messages"].([]any); ok {
		for _, m := range baseMsgs {
			if mm, ok := m.(map[string]any); ok {
				if role, _ := mm["role"].(string); role == "system" {
					msgs = append(msgs, m)
				}
			}
		}
	}
	for _, m := range req.Messages {
		out := map[string]any{"role": m.Role}
		// A tool message whose content is null makes upstream drop the paired
		// assistant turn ("Messages with role 'tool' must be a response to a
		// preceding message with 'tool_calls'"), so normalize it to "".
		if len(m.Content) == 0 || string(m.Content) == "null" {
			out["content"] = ""
		} else {
			out["content"] = m.Content
		}
		if m.Name != "" {
			out["name"] = m.Name
		}
		if len(m.ToolCalls) > 0 {
			out["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			out["tool_call_id"] = m.ToolCallID
		}
		msgs = append(msgs, out)
	}
	base["messages"] = msgs

	if len(req.Tools) > 0 {
		base["tools"] = req.Tools
	}

	// parameters: generation knobs plus the normalized thinking level.
	params, ok := base["parameters"].(map[string]any)
	if !ok {
		params = map[string]any{}
		base["parameters"] = params
	}
	switch {
	case req.MaxTokens > 0:
		params["max_tokens"] = req.MaxTokens
	case req.MaxCompletionTokens > 0:
		params["max_tokens"] = req.MaxCompletionTokens
	}
	if req.Temperature != nil {
		params["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		params["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		params["stop"] = req.Stop
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		params["tool_choice"] = req.ToolChoice
	}
	if level, send := resolveReasoningEffort(req.ReasoningEffort, facts); send {
		params["reasoning_effort"] = level
		if level == "none" {
			params["enable_thinking"] = false
			delete(params, "reasoning_budget_tokens")
		} else {
			params["enable_thinking"] = true
		}
	}

	// business: one entry per request; the version field tracks the CLI the
	// plugin mirrors.
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		// business.version stays at the template's value (0.1.43). It is the
		// business-protocol version, not the client version: setting it to
		// cosy-version makes upstream route the call to a node that answers
		// "Unsupported model format" for every model.
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	// Blocked-term obfuscation runs last, over the finished body, so the
	// mirrors (chat_prompt, chat_context text, business.name) all derive from
	// the same rewritten text. It only touches prompt and tool metadata.
	if applyDesensitizeToBody(base, currentDesensitize()) {
		// business.name is a 30-rune prefix of the prompt; keep it consistent
		// with the rewritten text rather than leaking the original spelling.
		if biz, ok := base["business"].(map[string]any); ok {
			if name, ok := biz["name"].(string); ok {
				biz["name"] = desensitizeText(currentDesensitize(), name)
			}
		}
	}

	if dumpPath := strings.TrimSpace(os.Getenv("QODER_DUMP_BODY")); dumpPath != "" {
		if out, err := json.MarshalIndent(base, "", " "); err == nil {
			_ = os.WriteFile(dumpPath, out, 0o600)
		}
	}
	return json.Marshal(base)
}
