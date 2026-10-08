package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func collectFrames(t *testing.T, events []traeSSEEvent) [][]byte {
	t.Helper()
	translator := newTraeTranslator("glm-5.2")
	var frames [][]byte
	for _, event := range events {
		produced, err := translator.handle(event)
		if err != nil {
			t.Fatalf("handle(%s): %v", event.Name, err)
		}
		frames = append(frames, produced...)
	}
	frames = append(frames, translator.finishFrames()...)
	return frames
}

func event(name, data string) traeSSEEvent {
	return traeSSEEvent{Name: name, Data: []byte(data)}
}

func TestTranslatorEmitsRoleOnceAndContentDeltas(t *testing.T) {
	frames := collectFrames(t, []traeSSEEvent{
		event("metadata", `{"session_id":"abc"}`),
		event("output", `{"response":"你好"}`),
		event("output", `{"response":"，世界"}`),
		event("token_usage", `{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"reasoning_tokens":1}`),
		event("done", `{"finish_reason":"stop"}`),
	})
	if len(frames) != 4 {
		t.Fatalf("frames = %d, want 2 deltas + finish + usage", len(frames))
	}
	first := decodeFrame(t, frames[0])
	if first.Delta["role"] != "assistant" || first.Delta["content"] != "你好" {
		t.Errorf("first frame = %+v, want role plus the first content delta", first.Delta)
	}
	second := decodeFrame(t, frames[1])
	if _, hasRole := second.Delta["role"]; hasRole {
		t.Error("role must be sent once, on the first frame")
	}
	finish := decodeFrame(t, frames[2])
	if finish.FinishReason != "stop" || len(finish.Delta) != 0 {
		t.Errorf("finish frame = %+v / %q", finish.Delta, finish.FinishReason)
	}
	usage := decodeFrame(t, frames[3])
	if usage.Usage["prompt_tokens"] != float64(10) {
		t.Errorf("usage frame = %+v", usage.Usage)
	}
	if details, ok := usage.Usage["completion_tokens_details"].(map[string]any); !ok || details["reasoning_tokens"] != float64(1) {
		t.Errorf("reasoning tokens must be reported: %+v", usage.Usage)
	}
}

type decodedFrame struct {
	Delta        map[string]any
	FinishReason string
	Usage        map[string]any
}

func decodeFrame(t *testing.T, raw []byte) decodedFrame {
	t.Helper()
	var frame struct {
		Choices []struct {
			Delta        map[string]any `json:"delta"`
			FinishReason any            `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("frame is not JSON: %v (%s)", err, raw)
	}
	out := decodedFrame{Usage: frame.Usage}
	if len(frame.Choices) > 0 {
		out.Delta = frame.Choices[0].Delta
		if reason, ok := frame.Choices[0].FinishReason.(string); ok {
			out.FinishReason = reason
		}
	}
	return out
}

// The upstream names the field function_call, streams arguments in pieces, and
// reports finish_reason "stop" even for a tool call: all three must be fixed on
// the way out.
func TestTranslatorMapsStreamingToolCalls(t *testing.T) {
	frames := collectFrames(t, []traeSSEEvent{
		event("output", `{"tool_calls":[{"index":0,"id":"call_up","type":"function","function_call":{"name":"get_weather","arguments":"{\"city\": ","partial_arguments":null,"namespace":null}}]}`),
		event("output", `{"tool_calls":[{"index":0,"id":"","type":"function","function_call":{"name":"","arguments":"\"北京\"}","partial_arguments":null,"namespace":null}}]}`),
		event("done", `{"finish_reason":"stop"}`),
	})
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want two tool deltas + finish", len(frames))
	}
	first := decodeFrame(t, frames[0])
	calls, ok := first.Delta["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("first frame tool_calls = %+v", first.Delta["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_up" || call["type"] != "function" {
		t.Errorf("call identity = %+v", call)
	}
	function := call["function"].(map[string]any)
	if function["name"] != "get_weather" || function["arguments"] != `{"city": ` {
		t.Errorf("first fragment function = %+v", function)
	}
	if _, leaked := function["partial_arguments"]; leaked {
		t.Error("upstream-private fields must not reach the client")
	}

	second := decodeFrame(t, frames[1])
	secondCalls := second.Delta["tool_calls"].([]any)
	secondCall := secondCalls[0].(map[string]any)
	if _, hasID := secondCall["id"]; hasID {
		t.Error("a continuation fragment must not repeat the call id")
	}
	secondFunction := secondCall["function"].(map[string]any)
	if _, hasName := secondFunction["name"]; hasName {
		t.Errorf("a continuation fragment must omit the name entirely, got %+v", secondFunction)
	}
	if secondFunction["arguments"] != `"北京"}` {
		t.Errorf("arguments fragment = %v", secondFunction["arguments"])
	}

	finish := decodeFrame(t, frames[2])
	if finish.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls despite the upstream's stop", finish.FinishReason)
	}
}

func TestTranslatorFoldsToolCallsForNonStreamingClients(t *testing.T) {
	translator := newTraeTranslator("glm-5.2")
	for _, ev := range []traeSSEEvent{
		event("output", `{"tool_calls":[{"index":0,"id":"call_1","function_call":{"name":"get_weather","arguments":"{\"city\":"}}]}`),
		event("output", `{"tool_calls":[{"index":0,"function_call":{"arguments":"\"北京\"}"}}]}`),
		event("done", `{"finish_reason":"stop"}`),
	} {
		if _, err := translator.handle(ev); err != nil {
			t.Fatal(err)
		}
	}
	var completion struct {
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(translator.completion(), &completion); err != nil {
		t.Fatal(err)
	}
	if completion.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
	calls := completion.Choices[0].Message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	call := calls[0].(map[string]any)
	if _, hasIndex := call["index"]; hasIndex {
		t.Error("a non-streaming tool call must not carry the streaming index field")
	}
	function := call["function"].(map[string]any)
	if function["name"] != "get_weather" || function["arguments"] != `{"city":"北京"}` {
		t.Errorf("folded call = %+v", function)
	}
}

// A half-written call must not reach the client as unparseable arguments.
func TestTranslatorDropsTruncatedToolCalls(t *testing.T) {
	translator := newTraeTranslator("glm-5.2")
	for _, ev := range []traeSSEEvent{
		event("output", `{"tool_calls":[{"index":0,"id":"call_1","function_call":{"name":"get_weather","arguments":"{\"city\":"}}]}`),
		event("done", `{"finish_reason":"length"}`),
	} {
		if _, err := translator.handle(ev); err != nil {
			t.Fatal(err)
		}
	}
	var completion struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(translator.completion(), &completion); err != nil {
		t.Fatal(err)
	}
	if _, present := completion.Choices[0].Message["tool_calls"]; present {
		t.Error("a truncated call must be dropped, not sent half-written")
	}
}

func TestTranslatorKeepsReasoningSeparate(t *testing.T) {
	frames := collectFrames(t, []traeSSEEvent{
		event("output", `{"reasoning_content":"先想想"}`),
		event("output", `{"response":"答案"}`),
		event("done", `{"finish_reason":"stop"}`),
	})
	first := decodeFrame(t, frames[0])
	if first.Delta["reasoning_content"] != "先想想" {
		t.Errorf("reasoning delta = %+v", first.Delta)
	}
	if _, isContent := first.Delta["content"]; isContent {
		t.Error("reasoning must not be folded into content")
	}
}

// A long queue keeps the connection alive with a comment frame instead of
// letting the client time out.
func TestTranslatorKeepsTheStreamAliveWhileQueued(t *testing.T) {
	frames := collectFrames(t, []traeSSEEvent{
		event("request_wait_in_queue", `{"position":733,"message":"Too many current requests."}`),
		event("request_wait_in_queue", `{"position":120,"message":"Too many current requests."}`),
		event("output", `{"response":"done"}`),
		event("done", `{"finish_reason":"stop"}`),
	})
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want one comment + content + finish", len(frames))
	}
	if !strings.HasPrefix(string(frames[0]), ": queued") {
		t.Errorf("first frame = %q, want an SSE comment", frames[0])
	}
	if !strings.Contains(string(frames[0]), "733") {
		t.Errorf("the comment should carry the queue position, got %q", frames[0])
	}
	if strings.Contains(string(frames[1]), ": queued") {
		t.Error("queue notices must not repeat on every upstream event")
	}
}

func TestTranslatorSurfacesUpstreamErrors(t *testing.T) {
	translator := newTraeTranslator("glm-5.2")
	_, err := translator.handle(event("error", `{"code":"10101","message":"无效参数"}`))
	if err == nil || !strings.Contains(err.Error(), "无效参数") {
		t.Fatalf("err = %v, want the upstream message", err)
	}
}

func TestScannerParsesUpstreamFraming(t *testing.T) {
	raw := "event:output\ndata:{\"response\":\"a\"}\n\nevent:output\ndata:{\"response\":\"b\"}\n--\nevent:done\ndata:{\"finish_reason\":\"stop\"}\n"
	var names []string
	var payloads []string
	if err := scanTraeSSE(strings.NewReader(raw), func(ev traeSSEEvent) error {
		names = append(names, ev.Name)
		payloads = append(payloads, string(ev.Data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "output" || names[2] != "done" {
		t.Fatalf("names = %v", names)
	}
	if payloads[1] != `{"response":"b"}` {
		t.Errorf("payload = %q", payloads[1])
	}
}

func TestBuildSoloRequestMapsMessagesAndTools(t *testing.T) {
	payload := []byte(`{
		"messages":[
			{"role":"system","content":"你是助手"},
			{"role":"user","content":[{"type":"text","text":"你好"}]},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":{"city":"北京"}}}]},
			{"role":"tool","tool_call_id":"call_1","content":"晴"}
		],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"查询天气","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"temperature":0.3,"max_tokens":256}`)
	raw, err := buildSoloRequest(payload, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Function    string           `json:"function"`
		Stream      bool             `json:"stream"`
		ConfigName  string           `json:"config_name"`
		Model       string           `json:"model"`
		Messages    []map[string]any `json:"messages"`
		Tools       []map[string]any `json:"tools"`
		Temperature float64          `json:"temperature"`
		MaxTokens   int              `json:"max_tokens"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Function != soloFunction || !body.Stream || body.ConfigName != "glm-5.2" || body.Model != "glm-5.2" {
		t.Fatalf("request envelope = %+v", body)
	}
	if body.Temperature != 0.3 || body.MaxTokens != 256 {
		t.Errorf("generation settings dropped: %+v", body)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("messages = %d", len(body.Messages))
	}
	userParts, ok := body.Messages[1]["content"].([]any)
	if !ok || len(userParts) != 1 {
		t.Fatalf("user content = %+v, want typed parts", body.Messages[1]["content"])
	}
	part := userParts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "你好" {
		t.Errorf("user part = %+v", part)
	}
	assistantCalls := body.Messages[2]["tool_calls"].([]any)
	call := assistantCalls[0].(map[string]any)
	if call["id"] != "call_1" {
		t.Errorf("assistant tool call = %+v", call)
	}
	// The channel expects its own field name on the request side too.
	function, ok := call["function_call"].(map[string]any)
	if !ok {
		t.Fatalf("assistant tool call must carry function_call, got %+v", call)
	}
	if function["name"] != "get_weather" {
		t.Errorf("function_call name = %v", function["name"])
	}
	if _, isString := function["arguments"].(string); !isString {
		t.Errorf("tool call arguments must be a JSON string, got %T", function["arguments"])
	}
	if body.Messages[3]["tool_call_id"] != "call_1" {
		t.Errorf("tool result = %+v", body.Messages[3])
	}
	params := body.Tools[0]["function"].(map[string]any)["parameters"]
	if _, isString := params.(string); !isString {
		t.Errorf("tool parameters must be a JSON string on this channel, got %T", params)
	}
	if !strings.Contains(params.(string), `"city"`) {
		t.Errorf("tool parameters = %v", params)
	}
}

func TestBuildSoloRequestRejectsEmptyPayload(t *testing.T) {
	if _, err := buildSoloRequest(nil, "glm-5.2"); err == nil {
		t.Error("an empty payload must be refused")
	}
	if _, err := buildSoloRequest([]byte(`{"messages":[]}`), "glm-5.2"); err == nil {
		t.Error("a request without messages must be refused")
	}
}

func TestSoloPayloadAcceptsStringAndPartContent(t *testing.T) {
	parts := soloContentParts("纯文本")
	if len(parts) != 1 || parts[0]["text"] != "纯文本" {
		t.Errorf("string content = %+v", parts)
	}
	parts = soloContentParts([]any{map[string]any{"type": "text", "text": "片段"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x"}}})
	if len(parts) != 1 || parts[0]["text"] != "片段" {
		t.Errorf("part content = %+v, want only the text part", parts)
	}
	if got := soloContentParts(""); len(got) != 1 || got[0]["text"] != "" {
		t.Errorf("empty content must still produce one empty part, got %+v", got)
	}
}
