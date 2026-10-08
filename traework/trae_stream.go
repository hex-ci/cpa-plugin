// trae_stream.go translates the solo channel's event stream into OpenAI
// chat-completion frames. The upstream is not OpenAI-shaped: it emits named SSE
// events carrying text, reasoning and tool-call fragments, a usage record, and
// queue notices while a request waits for capacity.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// traeSSEEvent is one named event from the solo channel.
type traeSSEEvent struct {
	Name string
	Data []byte
}

// scanTraeSSE reads the upstream frames — "event:<name>" plus "data:{json}",
// separated by a blank line or a "--" marker — and hands each to onEvent.
func scanTraeSSE(r io.Reader, onEvent func(traeSSEEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var name string
	var data strings.Builder
	flush := func() error {
		if name == "" && data.Len() == 0 {
			return nil
		}
		event := traeSSEEvent{Name: name, Data: []byte(data.String())}
		name, data = "", strings.Builder{}
		return onEvent(event)
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		switch {
		case line == "" || line == "--":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// traeOutputEvent is the payload of an "output" event.
type traeOutputEvent struct {
	Response         string            `json:"response"`
	ReasoningContent string            `json:"reasoning_content"`
	ToolCalls        []traeToolCallRaw `json:"tool_calls"`
}

// traeToolCallRaw is one upstream tool-call fragment. The field is named
// function_call (not function) and carries upstream-private extras that must not
// reach the client.
type traeToolCallRaw struct {
	Index        *int   `json:"index"`
	ID           string `json:"id"`
	Type         string `json:"type"`
	FunctionCall *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function_call"`
}

// traeTranslator folds upstream events into OpenAI frames.
type traeTranslator struct {
	model   string
	id      string
	created int64

	roleSent     bool
	sawToolCalls bool
	finishReason string

	queuePosition int
	queueNotified bool

	toolIDs   map[int]string
	toolOrder []int
	toolCalls map[int]map[string]any

	text      strings.Builder
	reasoning strings.Builder
	usage     map[string]any
}

func newTraeTranslator(model string) *traeTranslator {
	return &traeTranslator{
		model:     model,
		id:        "chatcmpl-" + randomHex(12),
		created:   time.Now().Unix(),
		toolIDs:   map[int]string{},
		toolCalls: map[int]map[string]any{},
	}
}

// handle converts one upstream event into zero or more client frames.
func (t *traeTranslator) handle(event traeSSEEvent) ([][]byte, error) {
	switch strings.ToLower(strings.TrimSpace(event.Name)) {
	case "output":
		var payload traeOutputEvent
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, nil
		}
		var frames [][]byte
		if payload.ReasoningContent != "" {
			t.reasoning.WriteString(payload.ReasoningContent)
			frames = append(frames, t.deltaFrame(map[string]any{"reasoning_content": payload.ReasoningContent}))
		}
		if payload.Response != "" {
			t.text.WriteString(payload.Response)
			frames = append(frames, t.deltaFrame(map[string]any{"content": payload.Response}))
		}
		if len(payload.ToolCalls) > 0 {
			if frame := t.toolCallFrame(payload.ToolCalls); frame != nil {
				frames = append(frames, frame)
			}
		}
		return frames, nil
	case "token_usage":
		t.recordUsage(event.Data)
		return nil, nil
	case "done":
		var payload struct {
			FinishReason string `json:"finish_reason"`
		}
		if err := json.Unmarshal(event.Data, &payload); err == nil {
			t.finishReason = strings.TrimSpace(payload.FinishReason)
		}
		return nil, nil
	case "request_wait_in_queue":
		var payload struct {
			Position int    `json:"position"`
			Message  string `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &payload); err == nil {
			t.queuePosition = payload.Position
		}
		// A comment frame is a legal SSE keep-alive: it stops client-side idle
		// timeouts during a long queue without inventing content.
		if !t.queueNotified {
			t.queueNotified = true
			return [][]byte{[]byte(": queued" + t.queueSuffix())}, nil
		}
		return nil, nil
	case "error":
		var payload struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(event.Data, &payload)
		message := strings.TrimSpace(payload.Message)
		if message == "" {
			message = strings.TrimSpace(string(event.Data))
		}
		if message == "" {
			message = "upstream error"
		}
		if payload.Code != "" {
			message = payload.Code + ": " + message
		}
		return nil, fmt.Errorf("%s", redactSecrets(message))
	default:
		// metadata / timing_cost / extra_info / queue_begin / queue_end /
		// progress_notice carry no client-visible content.
		return nil, nil
	}
}

func (t *traeTranslator) queueSuffix() string {
	if t.queuePosition > 0 {
		return fmt.Sprintf(" position %d", t.queuePosition)
	}
	return ""
}

// recordUsage keeps the upstream accounting for the final frame.
func (t *traeTranslator) recordUsage(raw []byte) {
	var payload struct {
		PromptTokens           int64 `json:"prompt_tokens"`
		CompletionTokens       int64 `json:"completion_tokens"`
		TotalTokens            int64 `json:"total_tokens"`
		CacheReadInputTokens   int64 `json:"cache_read_input_tokens"`
		CacheCreationInputToks int64 `json:"cache_creation_input_tokens"`
		ReasoningTokens        int64 `json:"reasoning_tokens"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	usage := map[string]any{
		"prompt_tokens":     payload.PromptTokens,
		"completion_tokens": payload.CompletionTokens,
		"total_tokens":      payload.TotalTokens,
	}
	if payload.CacheReadInputTokens > 0 || payload.CacheCreationInputToks > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": payload.CacheReadInputTokens}
	}
	if payload.ReasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": payload.ReasoningTokens}
	}
	t.usage = usage
}

// toolCallFrame maps upstream tool-call fragments onto an OpenAI delta. The
// upstream names the field function_call, streams the arguments in pieces and
// reports finish_reason "stop" even when a call was made, so the translator
// tracks that a call happened and reports tool_calls itself.
func (t *traeTranslator) toolCallFrame(calls []traeToolCallRaw) []byte {
	deltas := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		index := 0
		if call.Index != nil {
			index = *call.Index
		}
		t.sawToolCalls = true
		id, seen := t.toolIDs[index]
		if !seen {
			id = strings.TrimSpace(call.ID)
			if id == "" {
				id = "call_" + randomHex(12)
			}
			t.toolIDs[index] = id
			t.toolOrder = append(t.toolOrder, index)
		}
		entry := map[string]any{"index": index}
		if !seen {
			entry["id"] = id
			entry["type"] = "function"
		}
		function := map[string]any{}
		if call.FunctionCall != nil {
			if name := strings.TrimSpace(call.FunctionCall.Name); name != "" {
				function["name"] = name
			}
			if args := call.FunctionCall.Arguments; args != "" {
				function["arguments"] = args
			}
			merged, ok := t.toolCalls[index]
			if !ok {
				merged = map[string]any{"id": id, "type": "function", "function": map[string]any{}}
				t.toolCalls[index] = merged
			}
			mergedFunction := merged["function"].(map[string]any)
			if name, ok := function["name"]; ok {
				mergedFunction["name"] = name
			}
			if args, ok := function["arguments"]; ok {
				current, _ := mergedFunction["arguments"].(string)
				mergedFunction["arguments"] = current + args.(string)
			}
		}
		if len(function) > 0 {
			entry["function"] = function
		}
		deltas = append(deltas, entry)
	}
	if len(deltas) == 0 {
		return nil
	}
	return t.deltaFrame(map[string]any{"tool_calls": deltas})
}

// deltaFrame wraps a delta in a chat.completion.chunk, adding the assistant role
// to the first frame the client receives.
func (t *traeTranslator) deltaFrame(delta map[string]any) []byte {
	if !t.roleSent {
		t.roleSent = true
		if _, ok := delta["role"]; !ok {
			delta["role"] = "assistant"
		}
	}
	return t.frame(delta, nil)
}

func (t *traeTranslator) frame(delta map[string]any, finish any) []byte {
	frame := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil
	}
	return raw
}

// finishFrames closes the stream: the terminal choice carrying the effective
// finish_reason, then the usage frame.
func (t *traeTranslator) finishFrames() [][]byte {
	if !t.roleSent {
		// A stream that produced nothing still needs a well-formed first frame.
		_ = t.deltaFrame(map[string]any{"content": ""})
	}
	reason := strings.TrimSpace(t.finishReason)
	if t.sawToolCalls {
		reason = "tool_calls"
	}
	if reason == "" {
		reason = "stop"
	}
	frames := [][]byte{t.frame(map[string]any{}, reason)}
	if t.usage != nil {
		usageFrame := map[string]any{
			"id":      t.id,
			"object":  "chat.completion.chunk",
			"created": t.created,
			"model":   t.model,
			"choices": []map[string]any{},
			"usage":   t.usage,
		}
		if raw, err := json.Marshal(usageFrame); err == nil {
			frames = append(frames, raw)
		}
	}
	return frames
}

// completion folds everything seen into a non-streaming chat.completion.
func (t *traeTranslator) completion() []byte {
	reason := strings.TrimSpace(t.finishReason)
	if t.sawToolCalls {
		reason = "tool_calls"
	}
	if reason == "" {
		reason = "stop"
	}
	message := map[string]any{"role": "assistant", "content": t.text.String()}
	if t.reasoning.Len() > 0 {
		message["reasoning_content"] = t.reasoning.String()
	}
	if t.sawToolCalls && len(t.toolOrder) > 0 {
		sorted := append([]int(nil), t.toolOrder...)
		sortInts(sorted)
		calls := make([]map[string]any, 0, len(sorted))
		for _, index := range sorted {
			call := t.toolCalls[index]
			if call == nil {
				continue
			}
			function, _ := call["function"].(map[string]any)
			args, _ := function["arguments"].(string)
			if strings.TrimSpace(args) != "" && !json.Valid([]byte(args)) {
				// A half-written call cannot be executed: drop it rather than
				// hand the client unparseable arguments.
				continue
			}
			folded := map[string]any{}
			for key, value := range call {
				if key == "index" {
					// index is a streaming-only field; a non-streaming message
					// must not carry it.
					continue
				}
				folded[key] = value
			}
			calls = append(calls, folded)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	result := map[string]any{
		"id":      t.id,
		"object":  "chat.completion",
		"created": t.created,
		"model":   t.model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": reason,
		}},
	}
	if t.usage != nil {
		result["usage"] = t.usage
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil
	}
	return raw
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " framing: CPA's chat-completions writer adds the prefix itself, while
// the cross-format translators consume already-framed payloads.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}
