// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// (pumpUpstreamStream), collecting it synchronously (collectUpstreamStream),
// and the SSE-frame helpers that re-frame, filter, and aggregate chunks.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	// A-37: never emit raw upstream bodies that may contain Bearer/JWT.
	errJSON, _ := json.Marshal(map[string]any{"error": map[string]any{"message": redactSecrets(message)}})
	_ = streamEmit(streamID, errJSON)
}

var streamCloseOnce sync.Map // streamID -> sync.Once

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	actual, _ := streamCloseOnce.LoadOrStore(streamID, &sync.Once{})
	once := actual.(*sync.Once)
	once.Do(func() {
		body, _ := json.Marshal(map[string]any{"stream_id": streamID})
		_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
	})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream reads the upstream SSE response in the background and
// emits each cleaned chunk to the host stream. It closes the stream when done.
// An emit failure (client disconnected → host closed the stream) aborts the
// pump so we stop reading a dead upstream. cancel is invoked on every exit so
// the underlying http request context is released promptly.
//
// v0.7.0: requests now route via host.http.do_stream so request-log captures
// the outbound call and host transport policy applies. The host bridge emits
// arbitrary 32KB chunks, so we adapt to io.Reader and keep the bufio.Scanner
// SSE line framing unchanged.
// rawDumpReader tees the upstream byte stream to QODER_RAW_DUMP when set.
// Field work on a new realm/product occasionally shows an SSE shape the parser
// has never seen; a raw capture beats guessing at the envelope.
func rawDumpReader(r io.Reader) io.Reader {
	path := strings.TrimSpace(os.Getenv("QODER_RAW_DUMP"))
	if path == "" {
		return r
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return r
	}
	return io.TeeReader(r, f)
}

func pumpUpstreamStream(buildReq func() (*http.Request, error), cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID string, sa *storedAuth) {
	// Always close the host stream exactly once on every exit path.
	closed := false
	closeOnce := func() {
		if closed {
			return
		}
		closed = true
		streamClose(streamID)
	}
	defer closeOnce()
	if cancel != nil {
		defer cancel()
	}

	// Capacity queue: a 10605 answer means the model is busy, not that the
	// request failed. Wait (bounded by config) and retry before surfacing it.
	var stream *hostHTTPStream
	var statusCode int
	queueCfg := currentConfig()
	for attempt := 0; ; attempt++ {
		httpReq, reqErr := buildReq()
		if reqErr != nil {
			streamEmitError(streamID, fmt.Sprintf("cosy: %v", reqErr))
			return
		}
		var err error
		stream, statusCode, _, err = hostHTTPDoStream(httpReq)
		if err != nil {
			publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
			streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
			return
		}
		if statusCode < 400 {
			break
		}
		errPayload, _ := io.ReadAll(newHostStreamReader(stream))
		stream.Close()
		signal, isQueue := detectQueueSignal(statusCode, errPayload)
		if !isQueue || !queueCfg.QueueWait || attempt >= maxQueueRetries {
			publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(errPayload))
			if authUID != "" {
				go reconcileByUID(authUID, statusCode, string(errPayload))
			}
			if isQueue {
				streamEmitError(streamID, queuedError(signal.Detail, time.Duration(signal.RetryAfter)*time.Second).Error())
				return
			}
			streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(string(errPayload), 200)))
			return
		}
		hostLogf("info", fmt.Sprintf("model queue: %s busy, waiting %ds (attempt %d)", upstreamModel, signal.RetryAfter, attempt+1))
		if waitErr := awaitQueueTurn(context.Background(), sa, newRequestSetID(), upstreamModel,
			time.Duration(queueCfg.QueueMaxWait)*time.Second,
			time.Duration(signal.RetryAfter)*time.Second, nil); waitErr != nil {
			publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, waitErr.Error())
			streamEmitError(streamID, queuedError(signal.Detail, time.Duration(queueCfg.QueueMaxWait)*time.Second).Error())
			return
		}
	}
	defer stream.Close()
	collector := &sseUsageCollector{}
	scanner := bufio.NewScanner(rawDumpReader(newHostStreamReader(stream)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(line, "data:")
		var outer map[string]any
		if json.Unmarshal([]byte(payload), &outer) != nil {
			continue
		}
		bodyStr, ok := outer["body"].(string)
		if !ok {
			continue
		}
		if bodyStr == "[DONE]" {
			break
		}
		if code, msg := sseErrorEnvelope(outer, bodyStr); code != 0 {
			publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, code, msg)
			streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", code, truncateRedacted(msg, 200)))
			return
		}
		collector.feed(bodyStr)
		cleaned := cleanChunkJSON(bodyStr)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := streamEmit(streamID, []byte(cleaned)); err != nil {
			// Client disconnected / host closed stream — abort; do not report success.
			publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, "stream_emit: "+err.Error())
			return
		}
	}
	// A mid-stream read failure means the client received a truncated stream:
	// surface it as an error frame and record the attempt as failed.
	if err := scanner.Err(); err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, err.Error())
		streamEmitError(streamID, fmt.Sprintf("upstream stream read error: %v", err))
		return
	}
	publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), false, 0, "")
	invalidateAccountCredits(authID, authUID)
}

// collectUpstreamStreamQoder is the Qoder-flavoured synchronous fallback
// (no async stream id): drain the upstream nested SSE, unwrap the inner
// OpenAI chunks, return them as a slice. The collector, when non-nil,
// observes the unwrapped inner chunks for usage extraction.
func collectUpstreamStreamQoder(encodedBody string, sa *storedAuth, modelKey string, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	chatURL := realmOf(sa).chatURL()
	var stream *hostHTTPStream
	var statusCode int
	var err error
	queueCfg := currentConfig()
	for attempt := 0; ; attempt++ {
		httpReq, reqErr := newChatRequest(context.Background(), sa, encodedBody, chatURL, modelKey)
		if reqErr != nil {
			return nil, 0, fmt.Errorf("cosy: %w", reqErr)
		}
		stream, statusCode, _, err = hostHTTPDoStream(httpReq)
		if err != nil {
			return nil, 0, fmt.Errorf("http_error: %w", err)
		}
		if statusCode < 400 {
			break
		}
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		stream.Close()
		signal, isQueue := detectQueueSignal(statusCode, payload)
		if !isQueue || !queueCfg.QueueWait || attempt >= maxQueueRetries {
			if isQueue {
				return nil, statusCode, queuedError(signal.Detail, time.Duration(signal.RetryAfter)*time.Second)
			}
			return nil, statusCode, fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(string(payload), 200))
		}
		hostLogf("info", fmt.Sprintf("model queue: %s busy, waiting %ds (attempt %d)", modelKey, signal.RetryAfter, attempt+1))
		if waitErr := awaitQueueTurn(context.Background(), sa, newRequestSetID(), modelKey,
			time.Duration(queueCfg.QueueMaxWait)*time.Second,
			time.Duration(signal.RetryAfter)*time.Second, nil); waitErr != nil {
			return nil, statusCode, queuedError(signal.Detail, time.Duration(queueCfg.QueueMaxWait)*time.Second)
		}
	}
	defer stream.Close()
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	scanner := bufio.NewScanner(rawDumpReader(newHostStreamReader(stream)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(line, "data:")
		var outer map[string]any
		if json.Unmarshal([]byte(payload), &outer) != nil {
			continue
		}
		bodyStr, ok := outer["body"].(string)
		if !ok || bodyStr == "[DONE]" {
			continue
		}
		if collector != nil {
			collector.feed(bodyStr)
		}
		cleaned := cleanChunkJSON(bodyStr)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: json.RawMessage(cleaned)})
	}
	if err := scanner.Err(); err != nil {
		return chunks, 0, fmt.Errorf("upstream stream read error: %w", err)
	}
	return chunks, 0, nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// cleanChunkJSON strips only the known-problematic empty tool-call shells
// from choice deltas: a null/empty function_call and an empty tool_calls array
// (Qoder emits these on the terminal chunk, and strict clients interpret
// them as a truncated tool call). Other empty-but-legal values are preserved:
// content:"" is a valid delta (pure tool-call chunk) and the role-only first
// chunk must survive so clients can establish the message role.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	changed := false
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			if v, present := delta["function_call"]; present && isEmptyValue(v) {
				delete(delta, "function_call")
				changed = true
			}
			if v, present := delta["tool_calls"]; present {
				if arr, isArr := v.([]any); isArr && len(arr) == 0 {
					delete(delta, "tool_calls")
					changed = true
				}
			}
			// Upstream often pads terminal/noop deltas with empty noise fields
			// that clients ignore but pollute wire size / some parsers.
			for _, noise := range []string{"extra_fields", "refusal", "reasoning_content"} {
				if v, present := delta[noise]; present && isEmptyValue(v) {
					delete(delta, noise)
					changed = true
				}
			}
			// Drop a fully-empty delta ONLY when the choice carries no other
			// signal (no finish_reason): e.g. {"delta":{"function_call":null}}
			// reduced to {}. A delta with role/content:"" is meaningful and
			// never reaches this branch (those fields are preserved above).
			if len(delta) == 0 {
				if fr, _ := choice["finish_reason"].(string); fr == "" {
					return ""
				}
			}
		}
	}
	if !changed {
		return s
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	// tool_calls arrive as streaming deltas: each chunk carries an index plus a
	// partial call (id/type/function.name on the first delta, argument text
	// fragments afterwards). Merge by index instead of appending raw fragments
	// so the folded completion holds whole calls.
	toolCalls := map[int]map[string]any{}
	var toolOrder []int
	var scanErr error

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if v, ok := call["index"].(float64); ok {
							idx = int(v)
						}
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}
	if err := scanner.Err(); err != nil {
		scanErr = err
	}
	// A mid-stream read failure means the folded completion is truncated. The
	// host discards the payload entirely when the plugin returns an error
	// (sdk/api/handlers executeWithPluginExecutor), so fail fast here instead
	// of assembling a partial completion nobody can safely consume.
	if scanErr != nil {
		return nil, fmt.Errorf("upstream stream read error: %w", scanErr)
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-qoder"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// aggregateQoderSSE folds Qoder's nested SSE stream into a single
// OpenAI chat.completion object. The gateway emits frames shaped:
//
//	data:{"headers":{...},"body":"<json-string>","statusCodeValue":200,...}
//
// where `body` is a JSON-encoded string that itself contains an OpenAI-style
// chunk: {"choices":[{"delta":{...}}],...}. We unwrap the outer envelope,
// then aggregate the inner chunks using the same logic as aggregateCompletion.
//
// Terminal frames: data:{"body":"[DONE]"} followed by an event:finish line
// with timing metadata (ignored).
func aggregateQoderSSE(r io.Reader, model string) ([]byte, error) {
	// Unwrap the nested SSE into an inner plain-text stream of OpenAI chunks,
	// then delegate to aggregateCompletion. We materialise the inner stream
	// into memory because the gateway's SSE is short-lived (one chat call)
	// and the inner stream is at most a few hundred KB.
	var inner strings.Builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			// Skip event:/id:/retry:/comment lines
			continue
		}
		payload := strings.TrimPrefix(line, "data:")
		var outer map[string]any
		if err := json.Unmarshal([]byte(payload), &outer); err != nil {
			continue
		}
		bodyStr, ok := outer["body"].(string)
		if !ok {
			continue
		}
		if bodyStr == "[DONE]" {
			break
		}
		// An error envelope travels inside HTTP 200. Folding it into an empty
		// completion would hand the client a 200 with no content instead of an
		// error, which is the hardest failure to notice.
		if code, msg := sseErrorEnvelope(outer, bodyStr); code != 0 {
			return nil, fmt.Errorf("upstream %d: %s", code, truncateRedacted(msg, 200))
		}
		// Re-emit the inner JSON as a standard "data:<json>\n" SSE frame so
		// aggregateCompletion's parser can consume it unchanged.
		inner.WriteString("data:")
		inner.WriteString(bodyStr)
		inner.WriteString("\n\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("qoder SSE read: %w", err)
	}
	if inner.Len() == 0 {
		return nil, fmt.Errorf("upstream returned no completion chunks")
	}
	return aggregateCompletion(strings.NewReader(inner.String()), model)
}

// sseErrorEnvelope reports the upstream error carried by one SSE frame: either
// a non-2xx statusCodeValue on the envelope, or an inner document with a
// non-200 code. Returns 0 when the frame is ordinary content.
func sseErrorEnvelope(outer map[string]any, bodyStr string) (int, string) {
	status := 0
	if v, ok := outer["statusCodeValue"].(float64); ok {
		status = int(v)
	}
	var inner struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(bodyStr), &inner)
	code := 0
	switch v := inner.Code.(type) {
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			code = n
		}
	case float64:
		code = int(v)
	}
	if status < 400 && (code == 0 || code < 400) {
		return 0, ""
	}
	if code >= 400 {
		return code, inner.Message
	}
	return status, inner.Message
}

// mergeToolCallDelta folds one streaming tool_call fragment into the merged
// call: scalar fields (id/type) are taken when first seen, function.name is
// concatenated (upstream may split it), and function.arguments text fragments
// are appended in arrival order.
func mergeToolCallDelta(merged, delta map[string]any) {
	for _, k := range []string{"id", "type"} {
		if _, present := merged[k]; !present {
			if v, ok := delta[k].(string); ok && v != "" {
				merged[k] = v
			}
		}
	}
	dfn, _ := delta["function"].(map[string]any)
	if dfn == nil {
		return
	}
	mfn, _ := merged["function"].(map[string]any)
	if mfn == nil {
		mfn = map[string]any{}
		merged["function"] = mfn
	}
	if v, ok := dfn["name"].(string); ok && v != "" {
		cur, _ := mfn["name"].(string)
		mfn["name"] = cur + v
	}
	if v, ok := dfn["arguments"].(string); ok && v != "" {
		cur, _ := mfn["arguments"].(string)
		mfn["arguments"] = cur + v
	}
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// isEmptyValue reports whether v is a "zero" SSE field that should be
// stripped from outgoing chunks. Used by cleanChunkJSON to drop the legacy
// function_call shell ({"name":"","arguments":""}) some upstreams emit on
// the terminal chunk — clients that strictly validate the delta will reject
// the shell as an invalid tool call.
func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		for _, val := range x {
			if !isEmptyValue(val) {
				return false
			}
		}
		return true
	}
	return false
}
