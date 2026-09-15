package main

import (
	"encoding/json"
	"testing"
)

func TestContentText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain string", raw: `"hello"`, want: "hello"},
		{name: "missing", raw: ``, want: ""},
		{name: "null", raw: `null`, want: ""},
		{
			name: "part array",
			raw:  `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"https://example.invalid/x.png"}},{"type":"text","text":"b"}]`,
			want: "a\nb",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contentText(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("contentText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestBuildQoderBodyForwardsToolCallFields(t *testing.T) {
	req := &openAIRequest{
		Model: "dfmodel",
		Messages: []openAIMessage{
			{Role: "user", Content: json.RawMessage(`"查询北京天气"`)},
			{
				Role:      "assistant",
				Content:   json.RawMessage(`null`),
				ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]`),
			},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"晴"`)},
		},
		Tools: json.RawMessage(`[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]`),
	}

	body, err := buildQoderBody(req, "dfmodel", "personal")
	if err != nil {
		t.Fatalf("buildQoderBody() error = %v", err)
	}

	var parsed struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCalls  json.RawMessage `json:"tool_calls"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("Unmarshal(body) error = %v", err)
	}

	var assistantContent string
	foundAssistantToolCalls := false
	foundToolResult := false
	for _, m := range parsed.Messages {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				foundAssistantToolCalls = true
				if err := json.Unmarshal(m.Content, &assistantContent); err != nil {
					t.Fatalf("assistant content decode: %v", err)
				}
			}
		case "tool":
			if m.ToolCallID == "call_1" {
				foundToolResult = true
				if string(m.Content) != `"晴"` {
					t.Fatalf("tool content = %s, want \"晴\"", string(m.Content))
				}
			}
		}
	}
	if !foundAssistantToolCalls {
		t.Fatal("assistant tool_calls turn was dropped from forwarded messages")
	}
	if assistantContent != "" {
		t.Fatalf("assistant null content = %q, want empty string", assistantContent)
	}
	if !foundToolResult {
		t.Fatal("tool message with tool_call_id was dropped from forwarded messages")
	}
	if len(parsed.Tools) == 0 {
		t.Fatal("tools definition was not forwarded")
	}
}
