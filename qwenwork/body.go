// body.go constructs the qwenwork agent_chat_generation request body from an
// OpenAI-style chat completion payload. qwenwork (千问办公) 走明文 JSON —— 没有
// qoderwork 的 QoderEncoding/base64 与 baseprompt 模板；结构对齐官方 0.1.8
// asar 与 Buddy2api 的 build_body。
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// cpaToUpstreamKey maps CPA-facing model names to upstream keys. qwenwork's
// real model keys (from /algo/api/v2/model/list, qwork scene) are "pro"
// (default), "flash", "qwen3.8-max-preview". Unknown names pass through.
func cpaToUpstreamKey(cpaModel string) string {
	switch cpaModel {
	case "auto", "advanced", "pro":
		return "pro"
	case "lite", "flash":
		return "flash"
	case "qwen3.8-max", "max", "qwen3.8-max-preview":
		return "qwen3.8-max-preview"
	}
	return cpaModel
}

// messageContentText joins an OpenAI message content (string or array of
// {text} parts) into a single string.
func messageContentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	default:
		if c == nil {
			return ""
		}
		b, _ := json.Marshal(c)
		return string(b)
	}
}

// splitMessages separates system messages (joined) from the conversation, and
// returns the last user text for chat_context.text / originalContent.
func splitMessages(payload map[string]any) (system string, messages []any, lastUser string) {
	raw, _ := payload["messages"].([]any)
	var sysParts []string
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" {
			if s := messageContentText(m["content"]); s != "" {
				sysParts = append(sysParts, s)
			}
			continue
		}
		messages = append(messages, m)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if m, ok := messages[i].(map[string]any); ok {
			if role, _ := m["role"].(string); role == "user" {
				if s := messageContentText(m["content"]); s != "" {
					lastUser = s
				}
				break
			}
		}
	}
	return strings.Join(sysParts, "\n\n"), messages, lastUser
}

// buildQwenBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey). The body
// is plain JSON and is signed as-is by COSY (no QoderEncoding).
func buildQwenBody(payload map[string]any, modelKey string) ([]byte, error) {
	if modelKey == "" {
		modelKey = "pro"
	}
	// Desensitize the configured prompt and tool metadata fields before the
	// OpenAI payload is folded into the qwenwork body (same scope as the
	// WorkBuddy plugin: system/developer text, marker-flagged user text,
	// tool title/description only).
	applyDesensitizeInPlace(payload, currentFeatureRuntime())
	requestID := uuid.NewString()
	system, messages, lastUser := splitMessages(payload)
	if lastUser == "" {
		// Fall back to a single user message if none was parsed cleanly.
		for _, m := range messages {
			if mm, ok := m.(map[string]any); ok {
				if role, _ := mm["role"].(string); role == "user" {
					lastUser = messageContentText(mm["content"])
					if lastUser != "" {
						break
					}
				}
			}
		}
	}
	if lastUser == "" {
		lastUser = "ping"
	}

	isReasoning := false
	parameters := map[string]any{}
	for _, k := range []string{"temperature", "top_p", "max_tokens", "presence_penalty", "frequency_penalty"} {
		if v, ok := payload[k]; ok && v != nil {
			parameters[k] = v
		}
	}
	if _, ok := parameters["max_tokens"]; !ok {
		parameters["max_tokens"] = 32000
	}

	tools, _ := payload["tools"].([]any)
	if tools == nil {
		tools = []any{}
	}

	body := map[string]any{
		"request_id":     requestID,
		"request_set_id": requestID,
		"chat_record_id": requestID,
		"session_id":     uuid.NewString(),
		"stream":         true,
		"chat_task":      "FREE_INPUT",
		"chat_context": map[string]any{
			"text":     lastUser,
			"features": []any{},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": modelKey, "is_reasoning": isReasoning},
				"originalContent": lastUser,
			},
			"chatPrompt": "",
			"imageUrls":  nil,
		},
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"session_type":     "qoder_work",
		"aliyun_user_type": "",
		"model_config": map[string]any{
			"key":              modelKey,
			"display_name":     modelKey,
			"model":            "",
			"format":           "openai",
			"is_vl":            true,
			"is_reasoning":     isReasoning,
			"api_key":          "",
			"url":              "",
			"source":           "system",
			"max_input_tokens": 180000,
		},
		"system":     system,
		"messages":   messages,
		"tools":      tools,
		"parameters": parameters,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("body encode: %w", err)
	}
	return raw, nil
}
