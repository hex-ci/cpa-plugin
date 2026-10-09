// executor.go serves chat traffic: it maps the client's OpenAI payload onto the
// solo channel request and translates the upstream event stream back into
// chat-completion frames.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type executorRequestWire struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// executorStreamRequest wraps executor.execute_stream: the ExecutorRequest plus
// the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// soloPayload is the subset of the client request this channel understands.
type soloPayload struct {
	Messages    []map[string]any `json:"messages"`
	Tools       []map[string]any `json:"tools"`
	ToolChoice  json.RawMessage  `json:"tool_choice"`
	Temperature *float64         `json:"temperature"`
	TopP        *float64         `json:"top_p"`
	MaxTokens   *int             `json:"max_tokens"`
}

// buildSoloRequest rewrites the client payload into the solo request. The
// channel always streams; non-streaming callers fold the stream afterwards.
func buildSoloRequest(payload []byte, model string) ([]byte, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, errors.New("empty request payload")
	}
	var in soloPayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, fmt.Errorf("invalid request payload: %w", err)
	}
	if len(in.Messages) == 0 {
		return nil, errors.New("request has no messages")
	}
	messages := make([]map[string]any, 0, len(in.Messages))
	for _, message := range in.Messages {
		messages = append(messages, mapSoloMessage(message))
	}
	body := map[string]any{
		"messages":    messages,
		"function":    soloFunction,
		"stream":      true,
		"config_name": model,
		"model":       model,
	}
	if len(in.Tools) > 0 {
		body["tools"] = mapSoloTools(in.Tools)
	}
	if len(in.ToolChoice) > 0 && string(bytes.TrimSpace(in.ToolChoice)) != "null" {
		var choice any
		if err := json.Unmarshal(in.ToolChoice, &choice); err == nil {
			body["tool_choice"] = choice
		}
	}
	if in.Temperature != nil {
		body["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		body["top_p"] = *in.TopP
	}
	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		body["max_tokens"] = *in.MaxTokens
	}
	// Last step, so the rewrite sees exactly what goes upstream.
	applyDesensitizeInPlace(body, currentDesensitize())
	return json.Marshal(body)
}

// mapSoloMessage converts one OpenAI message into the channel's shape: content
// is always a list of typed parts, and tool calls/results keep their ids.
func mapSoloMessage(message map[string]any) map[string]any {
	out := map[string]any{}
	if role, ok := message["role"].(string); ok && role != "" {
		out["role"] = role
	} else {
		out["role"] = "user"
	}
	out["content"] = soloContentParts(message["content"])
	if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
		mapped := make([]map[string]any, 0, len(calls))
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			entry := map[string]any{}
			if id, ok := call["id"].(string); ok && id != "" {
				entry["id"] = id
			}
			entry["type"] = "function"
			if function, ok := call["function"].(map[string]any); ok {
				fn := map[string]any{}
				if name, ok := function["name"].(string); ok {
					fn["name"] = name
				}
				switch args := function["arguments"].(type) {
				case string:
					fn["arguments"] = args
				case map[string]any, []any:
					if raw, err := json.Marshal(args); err == nil {
						fn["arguments"] = string(raw)
					}
				}
				// The channel names this field function_call, matching the
				// shape it emits. A stricter model route (Doubao) rejects the
				// OpenAI spelling with "required field Name is not set".
				entry["function_call"] = fn
			}
			mapped = append(mapped, entry)
		}
		out["tool_calls"] = mapped
	}
	if id, ok := message["tool_call_id"].(string); ok && id != "" {
		out["tool_call_id"] = id
	}
	if name, ok := message["name"].(string); ok && name != "" {
		out["name"] = name
	}
	return out
}

// soloContentParts normalises a message content field into typed parts. Only
// text parts are forwarded: this channel's non-text parts have not been
// verified, and inventing a mapping would send the model something wrong.
func soloContentParts(content any) []map[string]any {
	parts := []map[string]any{}
	switch typed := content.(type) {
	case string:
		if typed != "" {
			parts = append(parts, map[string]any{"type": "text", "text": typed})
		}
	case []any:
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		}
	}
	if len(parts) == 0 {
		// The channel rejects an empty content list; an empty text part keeps
		// the turn well-formed (e.g. an assistant message that only calls a tool).
		parts = append(parts, map[string]any{"type": "text", "text": ""})
	}
	return parts
}

// mapSoloTools rewrites the tools array. parameters must be a JSON *string* on
// this channel: an object is rejected as an invalid argument.
func mapSoloTools(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		entry := map[string]any{"type": "function"}
		fn := map[string]any{}
		if name, ok := function["name"].(string); ok {
			fn["name"] = name
		}
		if description, ok := function["description"].(string); ok {
			fn["description"] = description
		}
		switch params := function["parameters"].(type) {
		case string:
			fn["parameters"] = params
		case nil:
			fn["parameters"] = "{}"
		default:
			encoded, err := json.Marshal(params)
			if err != nil {
				continue
			}
			fn["parameters"] = string(encoded)
		}
		entry["function"] = fn
		out = append(out, entry)
	}
	return out
}

// soloChatRequest builds the upstream request for one account.
func soloChatRequest(sa *storedAuth, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, soloAPIBase+soloChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range ideHeaders(sa.AccessToken, sa.UID, sa.MachineID, sa.DeviceID) {
		if value != "" {
			req.Header.Set(key, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

func handleExecExecute(raw []byte) ([]byte, error) {
	var req executorRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_storage", err.Error()), nil
	}
	body, err := buildSoloRequest(payloadOf(req.ExecutorRequest), req.Model)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error()), nil
	}
	httpReq, err := soloChatRequest(sa, body)
	if err != nil {
		return errorEnvelope("internal_error", err.Error()), nil
	}
	resp, err := hostHTTPDoWithCallback(httpReq, req.HostCallbackID)
	if err != nil {
		return errorEnvelope("http_error", redactSecrets(err.Error())), nil
	}
	if resp.StatusCode >= 400 {
		return errorEnvelopeWithStatus("http_error",
			fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 300)), resp.StatusCode), nil
	}
	translator := newTraeTranslator(req.Model)
	if err := scanTraeSSE(bytes.NewReader(resp.Body), func(event traeSSEEvent) error {
		_, errHandle := translator.handle(event)
		return errHandle
	}); err != nil {
		return errorEnvelope("upstream_error", redactSecrets(err.Error())), nil
	}
	completion := translator.completion()
	if len(completion) == 0 {
		return errorEnvelope("upstream_error", "upstream produced no completion"), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion}), nil
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_storage", err.Error()), nil
	}
	body, err := buildSoloRequest(payloadOf(req.ExecutorRequest), req.Model)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error()), nil
	}
	httpReq, err := soloChatRequest(sa, body)
	if err != nil {
		return errorEnvelope("internal_error", err.Error()), nil
	}
	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	if req.StreamID == "" {
		// No async stream id: collect synchronously and hand back the chunks.
		chunks, errCollect := collectTraeChunks(httpReq, req.Model, sseFramed, req.HostCallbackID)
		if errCollect != nil {
			if status, message, ok := upstreamStatusOf(errCollect); ok {
				return errorEnvelopeWithStatus("http_error", message, status), nil
			}
			return errorEnvelope("upstream_error", redactSecrets(errCollect.Error())), nil
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks}), nil
	}

	// Async: return at once and pump frames to the host stream.
	go pumpTraeStream(httpReq, req.StreamID, req.Model, sseFramed, req.HostCallbackID)
	return okEnvelope(streamResponse{Headers: headers}), nil
}

func handleExecCountTokens([]byte) ([]byte, error) {
	return errorEnvelope("unsupported_method", "TraeWork does not expose a count_tokens API"), nil
}

func payloadOf(req pluginapi.ExecutorRequest) []byte {
	if len(bytes.TrimSpace(req.Payload)) > 0 {
		return req.Payload
	}
	return req.OriginalRequest
}

// upstreamStatusError carries the upstream HTTP status so the host can map a
// failed executor call onto its own status and retry handling.
type upstreamStatusError struct {
	status  int
	message string
}

func (e *upstreamStatusError) Error() string { return e.message }

// StatusCode exposes the upstream status as an int for the host.
func (e *upstreamStatusError) StatusCode() int { return e.status }

// upstreamStatusOf extracts the upstream HTTP status from a collection error.
func upstreamStatusOf(err error) (int, string, bool) {
	var statusErr *upstreamStatusError
	if errors.As(err, &statusErr) {
		return statusErr.status, statusErr.Error(), true
	}
	return 0, "", false
}

// collectTraeChunks drains the upstream synchronously and returns the frames,
// framing each payload when the entry path needs "data: " prefixes.
func collectTraeChunks(httpReq *http.Request, model string, sseFramed bool, callbackID string) ([]pluginapi.ExecutorStreamChunk, error) {
	resp, err := hostHTTPDoWithCallback(httpReq, callbackID)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	if resp.StatusCode >= 400 {
		// Drain the body so the bridge can reuse the connection.
		_, _ = io.Copy(io.Discard, bytes.NewReader(resp.Body))
		return nil, &upstreamStatusError{
			status:  resp.StatusCode,
			message: fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 300)),
		}
	}
	translator := newTraeTranslator(model)
	frames := [][]byte{}
	handle := func(event traeSSEEvent) error {
		produced, errHandle := translator.handle(event)
		if errHandle != nil {
			return errHandle
		}
		frames = append(frames, produced...)
		return nil
	}
	if err := scanTraeSSE(bytes.NewReader(resp.Body), handle); err != nil {
		return nil, err
	}
	frames = append(frames, translator.finishFrames()...)
	if len(frames) == 0 {
		return nil, errors.New("upstream produced no frames")
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(frames))
	for _, frame := range frames {
		if len(frame) == 0 {
			continue
		}
		payload := frame
		if sseFramed {
			payload = append([]byte("data: "), frame...)
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
	}
	return chunks, nil
}

// pumpTraeStream streams the upstream to the host chunk by chunk. It always
// closes the host stream exactly once.
func pumpTraeStream(httpReq *http.Request, streamID, model string, sseFramed bool, callbackID string) {
	closed := false
	closeOnce := func() {
		if closed {
			return
		}
		closed = true
		streamClose(streamID)
	}
	defer closeOnce()

	stream, statusCode, _, err := hostHTTPDoStreamWithCallback(httpReq, callbackID)
	if err != nil {
		streamEmitError(streamID, "http_error: "+err.Error())
		return
	}
	defer stream.Close()
	if statusCode >= 400 {
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(string(payload), 300)))
		return
	}

	translator := newTraeTranslator(model)
	emitted := 0
	emit := func(frame []byte) error {
		if len(frame) == 0 {
			return nil
		}
		payload := frame
		if sseFramed {
			payload = append([]byte("data: "), frame...)
		}
		emitted++
		return streamEmit(streamID, payload)
	}
	errScan := scanTraeSSE(newHostStreamReader(stream), func(event traeSSEEvent) error {
		frames, errHandle := translator.handle(event)
		if errHandle != nil {
			return errHandle
		}
		for _, frame := range frames {
			if errEmit := emit(frame); errEmit != nil {
				return errEmit
			}
		}
		return nil
	})
	if errScan != nil {
		if emitted > 0 {
			// The client already has content: report the failure and stop.
			streamEmitError(streamID, redactSecrets(errScan.Error()))
			return
		}
		streamEmitError(streamID, redactSecrets(errScan.Error()))
		return
	}
	for _, frame := range translator.finishFrames() {
		if errEmit := emit(frame); errEmit != nil {
			return
		}
	}
}

// streamHeaders are the response headers for a streaming executor call.
func streamHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("X-Accel-Buffering", "no")
	return headers
}

// streamEmit pushes one chunk to the host stream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return errors.New("no stream id")
	}
	body, err := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	if err != nil {
		return err
	}
	_, errCall := hostCall(pluginabi.MethodHostStreamEmit, body)
	return errCall
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	body, err := json.Marshal(map[string]any{"stream_id": streamID, "error": redactSecrets(message)})
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostStreamEmit, body)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, err := json.Marshal(map[string]any{"stream_id": streamID})
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

// handleExecHTTPRequest serves the executor's generic HTTP passthrough. It is
// restricted to this provider's own upstream hosts so the capability cannot be
// used as an open proxy.
func handleExecHTTPRequest(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.ExecutorHTTPRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_storage", err.Error()), nil
	}
	target, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || !target.IsAbs() || target.User != nil {
		return errorEnvelope("invalid_request", "absolute URL without userinfo is required"), nil
	}
	if !allowedUpstreamHost(target) {
		return errorEnvelope("invalid_request", "upstream host is not allowed: "+target.Host), nil
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequest(method, target.String(), bytes.NewReader(req.Body))
	if err != nil {
		return errorEnvelope("invalid_request", err.Error()), nil
	}
	httpReq.Header = req.Headers.Clone()
	if httpReq.Header == nil {
		httpReq.Header = http.Header{}
	}
	for key, value := range ideHeaders(sa.AccessToken, sa.UID, sa.MachineID, sa.DeviceID) {
		if value != "" && httpReq.Header.Get(key) == "" {
			httpReq.Header.Set(key, value)
		}
	}
	resp, err := hostHTTPDoWithCallback(httpReq, req.HostCallbackID)
	if err != nil {
		return errorEnvelope("http_error", redactSecrets(err.Error())), nil
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	}), nil
}

// allowedUpstreamHost reports whether a passthrough target belongs to this
// provider.
func allowedUpstreamHost(target *url.URL) bool {
	for _, base := range []string{soloAPIBase, apiBaseCN} {
		allowed, err := url.Parse(base)
		if err != nil {
			continue
		}
		if strings.EqualFold(target.Host, allowed.Host) {
			return true
		}
	}
	return false
}
