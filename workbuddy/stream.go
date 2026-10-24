// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// (pumpUpstreamStream), collecting it synchronously (collectUpstreamStream),
// and the SSE-frame helpers that re-frame, filter, and aggregate chunks.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var streamHostCall = hostCall

type upstreamStatusError struct {
	status  int
	message string
}

func (e *upstreamStatusError) Error() string { return e.message }

// StatusCode exposes the upstream HTTP status as an int. The host probes the
// returned error for this method to map a failed executor call onto its own
// status/retry handling; without it every upstream failure would surface as an
// opaque error. Pure addition — callers that read .status or match on the
// message keep working.
func (e *upstreamStatusError) StatusCode() int { return e.status }

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := streamHostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamErrorWire(streamID, message string) []byte {
	raw, _ := json.Marshal(map[string]any{"stream_id": streamID, "error": redactSecrets(message)})
	return raw
}

func streamCloseWire(streamID string) []byte {
	raw, _ := json.Marshal(map[string]any{"stream_id": streamID})
	return raw
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	_, _ = streamHostCall(pluginabi.MethodHostStreamEmit, streamErrorWire(streamID, message))
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	_, _ = streamHostCall(pluginabi.MethodHostStreamClose, streamCloseWire(streamID))
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream opens the upstream SSE response and drains it into the
// host stream, reporting a failed attempt as an error frame. It is the
// fire-and-forget entry point used by tests and by callers that cannot report a
// status synchronously; the executor prefers openUpstreamStream so the failure
// reaches the host as an HTTP status (see executeStreamAsync).
func pumpUpstreamStream(httpReq *http.Request, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID string, callbackID string) {
	stream, statusCode, respHeaders, err := hostHTTPDoStreamWithCallback(httpReq, callbackID)
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	if err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		return
	}
	if statusCode >= 400 {
		payload := recordFailedStream(stream, statusCode, respHeaders, requestedModel, upstreamModel, authUID, started)
		streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(payload, 200)))
		return
	}
	pumpOpenStream(stream, streamID, sseFramed, requestedModel, upstreamModel, authUID, started, authID)
}

// recordFailedStream drains an error body, records the attempt and runs the
// credential reconcile, then closes the stream. It returns the raw body so the
// caller can surface it.
func recordFailedStream(stream *hostHTTPStream, statusCode int, respHeaders http.Header, requestedModel, upstreamModel, authUID string, started time.Time) string {
	defer stream.Close()
	payload, _ := io.ReadAll(newHostStreamReader(stream))
	publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(payload))
	if authUID != "" {
		go reconcileByUID(authUID, statusCode, string(payload))
	}
	// See retry_after.go: the host cannot receive the upstream wait hint,
	// so it is logged instead.
	if hint := retryAfterHint(respHeaders); hint != "" {
		hostLogf("warn", fmt.Sprintf("workbuddy upstream %d on auth=%s: upstream asks to wait %s",
			statusCode, shortUID(authUID), hint))
	}
	return string(payload)
}

// pumpOpenStream copies an already-opened 2xx upstream stream to the host,
// emitting one cleaned chunk per SSE event, then closes both. A 2xx response
// that produces no usable event is reported as an error frame: the client would
// otherwise see an empty successful stream.
func pumpOpenStream(stream *hostHTTPStream, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID string) {
	defer stream.Close()
	defer streamClose(streamID)
	collector := &sseUsageCollector{}
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	validEvents := 0
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" {
			continue
		}
		if content == "[DONE]" {
			break
		}
		collector.feed(content)
		cleaned := cleanChunkJSON(content)
		if cleaned == "" || !json.Valid([]byte(cleaned)) {
			continue
		}
		validEvents++
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
	if validEvents == 0 {
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, "empty upstream stream")
		streamEmitError(streamID, "empty upstream stream")
		return
	}
	publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), false, 0, "")
	invalidateAccountCredits(authID, authUID)
}

// collectUpstreamStream is the synchronous fallback (no async stream id): drain
// the upstream, clean each chunk, return them as a slice. The collector, when
// non-nil, observes raw upstream chunks for usage extraction. statusCode is the
// upstream HTTP status (0 for transport-level failures).
func collectUpstreamStream(body []byte, sa *storedAuth, sseFramed bool, collector *sseUsageCollector, callbackID string) ([]pluginapi.ExecutorStreamChunk, int, error) {
	httpReq, err := http.NewRequest(http.MethodPost, endpointChatFor(sa), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	backendHeaders(httpReq, sa)
	// Compliance: route via host.http.do_stream so request-log captures the call.
	stream, statusCode, respHeaders, err := hostHTTPDoStreamWithCallback(httpReq, callbackID)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if statusCode >= 400 {
		errPayload, _ := io.ReadAll(reader)
		if sa != nil && sa.Account.UID != "" {
			go reconcileByUID(sa.Account.UID, statusCode, string(errPayload))
		}
		// See retry_after.go: the host cannot receive the upstream wait hint,
		// so it is logged instead.
		if hint := retryAfterHint(respHeaders); hint != "" {
			hostLogf("warn", fmt.Sprintf("workbuddy upstream %d: upstream asks to wait %s", statusCode, hint))
		}
		return nil, statusCode, &upstreamStatusError{
			status:  statusCode,
			message: fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(string(errPayload), 200)),
		}
	}
	chunks, errAgg := aggregateSSEWithCollector(reader, sseFramed, collector)
	if errAgg != nil {
		return chunks, statusCode, errAgg
	}
	return chunks, statusCode, nil
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

// aggregateSSEWithCollector reads an upstream SSE stream and emits one chunk
// per data event. Empty tool-call shells are stripped and the trailing [DONE]
// is dropped (the host appends its own stream terminator). When sseFramed is
// true each payload is emitted as a "data: " line for cross-format
// translators; otherwise the payload is the raw JSON object and the host
// chat-completions writer adds the framing itself. A mid-stream read error
// aborts collection and is returned so the caller records the attempt as
// failed. The collector, when non-nil, observes raw upstream chunks for usage
// extraction.
func aggregateSSEWithCollector(r io.Reader, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var chunks []pluginapi.ExecutorStreamChunk
	validEvents := 0
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" {
			continue
		}
		if content == "[DONE]" {
			break
		}
		if collector != nil {
			collector.feed(content)
		}
		cleaned := cleanChunkJSON(content)
		if cleaned == "" || !json.Valid([]byte(cleaned)) {
			continue
		}
		validEvents++
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
	}
	if err := scanner.Err(); err != nil {
		return chunks, fmt.Errorf("upstream stream read error: %w", err)
	}
	if validEvents == 0 {
		return chunks, fmt.Errorf("upstream stream contained no valid data events")
	}
	return chunks, nil
}

// cleanChunkJSON strips only the known-problematic empty tool-call shells
// from choice deltas: a null/empty function_call and an empty tool_calls array
// (CodeBuddy emits these on the terminal chunk, and strict clients interpret
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
	// toolIndexByID remembers which slot each call id was assigned, so a later
	// fragment that omits index but carries the id lands in its own slot
	// instead of colliding with whatever sits at slot 0.
	toolIndexByID := map[string]int{}
	nextToolIndex := 0
	var scanErr error
	sawChoice := false
	sawDone := false

	// nextIndexFor picks the slot for one tool_call fragment. The upstream
	// usually sends index, but it is not guaranteed: a fragment with an id we
	// have seen continues that call, a fragment with a new id opens a fresh
	// slot, and a fragment with neither continues the most recent slot (a
	// continuation of the call already in flight). Falling back to slot 0 for
	// everything — the previous behaviour — merges independent calls into one
	// slot, concatenating their arguments into invalid JSON.
	nextIndexFor := func(call map[string]any) int {
		if v, ok := call["index"].(float64); ok {
			return int(v)
		}
		id, _ := call["id"].(string)
		if id != "" {
			if idx, ok := toolIndexByID[id]; ok {
				return idx
			}
			idx := nextToolIndex
			nextToolIndex++
			toolIndexByID[id] = idx
			return idx
		}
		if len(toolOrder) > 0 {
			return toolOrder[len(toolOrder)-1]
		}
		idx := nextToolIndex
		nextToolIndex++
		return idx
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
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
		if len(choices) > 0 {
			sawChoice = true
		}
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
						idx := nextIndexFor(call)
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
						// Register the id the fragment carried so later
						// fragments without index can find this slot, and so
						// a slot opened by a nameless fragment adopts the id
						// when it finally arrives.
						if id, _ := merged["id"].(string); id != "" {
							toolIndexByID[id] = idx
						}
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
	if !sawChoice {
		return nil, fmt.Errorf("upstream stream contained no valid completion events")
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			call := toolCalls[idx]
			// A stream cut short (finish_reason=length, or EOF without [DONE])
			// can leave a call's arguments half-written. Handing the client a
			// broken JSON string makes it fail to parse the call; inventing
			// "{}" would make it execute a tool with no arguments. Drop the
			// call instead — the request can be retried with a bigger budget.
			if (finish == "length" || !sawDone) && isTruncatedArguments(call) {
				continue
			}
			calls = append(calls, call)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
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

// isTruncatedArguments reports whether a folded tool call's arguments look like
// they were cut off mid-write.
//
// The test is deliberately narrow. An empty string is NOT truncation: it is the
// legitimate shape of a tool that takes no arguments. Anything that parses as
// JSON is also not truncation, even if the value type looks wrong — that is the
// model emitting bad output, which the client's own schema validation should
// report, not something the gateway may silently reinterpret. Only "non-empty
// and unparseable" means the stream was interrupted.
func isTruncatedArguments(call map[string]any) bool {
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return false
	}
	args, _ := fn["arguments"].(string)
	if strings.TrimSpace(args) == "" {
		return false
	}
	return !json.Valid([]byte(args))
}

// mergeToolCallDelta folds one streaming tool_call fragment into the merged
// call: scalar fields (id/type) are taken when first seen, function.name is
// kept from the first fragment that carries one, and function.arguments text
// fragments are appended in arrival order.
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
	// function.name is taken from the first fragment that carries one, never
	// concatenated. The upstream sends the name on the opening fragment and
	// then repeats it as an empty string on every continuation, so appending
	// is a no-op today — but a client that re-sends the name on each fragment
	// (or an upstream change that starts doing so) would otherwise produce
	// "BashBashBash". Keeping the first non-empty value is correct for both
	// shapes.
	if v, ok := dfn["name"].(string); ok && v != "" {
		if cur, _ := mfn["name"].(string); cur == "" {
			mfn["name"] = v
		}
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
