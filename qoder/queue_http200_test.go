package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// realQueueFrame is the exact upstream answer captured from a live 403 that
// arrived inside an HTTP 200 SSE envelope (RESOURCE_LIMIT / queueType p3).
const realQueueFrame = `data:{"headers":{"Content-Type":["application/json"]},"body":"{\"code\":\"403\",\"message\":\"{\\\"code\\\":\\\"10605\\\",\\\"message\\\":\\\"{\\\\\\\"isQueued\\\\\\\":true,\\\\\\\"modelKey\\\\\\\":\\\\\\\"qmodel_latest\\\\\\\",\\\\\\\"queueCount\\\\\\\":0,\\\\\\\"queueType\\\\\\\":\\\\\\\"p3\\\\\\\",\\\\\\\"retryAfterSeconds\\\\\\\":30,\\\\\\\"serviceAvailable\\\\\\\":false,\\\\\\\"subCode\\\\\\\":\\\\\\\"RESOURCE_LIMIT\\\\\\\",\\\\\\\"waitTime\\\\\\\":30}\\\",\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\"}\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}

`

// A 10605 answer arrives inside an HTTP 200 SSE envelope. The retry loop used
// to break out on the status alone, so the frame reached the parser and the
// client saw an error instead of a wait.
func TestPeekQueueSignalFindsQueueInOKStream(t *testing.T) {
	sig, queued, _, err := peekQueueSignal(strings.NewReader(realQueueFrame))
	if err != nil {
		t.Fatalf("peek error: %v", err)
	}
	if !queued {
		t.Fatal("queue not detected in HTTP 200 stream")
	}
	if sig.RetryAfter != 30 {
		t.Fatalf("retryAfter = %d, want 30", sig.RetryAfter)
	}
}

// A content frame must pass through untouched: the peeked head is replayed, so
// aggregation still sees the stream from its first byte.
func TestPeekQueueSignalReplaysContentHead(t *testing.T) {
	const rest = `data:{"headers":{},"body":"{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}","statusCodeValue":200}` + "\n\n"
	sig, queued, replay, err := peekQueueSignal(strings.NewReader(rest))
	if err != nil {
		t.Fatalf("peek error: %v", err)
	}
	if queued {
		t.Fatalf("content frame misread as a queue answer: %+v", sig)
	}
	replayed, err := io.ReadAll(replay)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if string(replayed) != rest {
		t.Fatalf("replay lost bytes: want %d, got %d", len(rest), len(replayed))
	}
	completion, err := aggregateQoderSSE(strings.NewReader(string(replayed)), "m")
	if err != nil {
		t.Fatalf("aggregate after replay: %v", err)
	}
	if !strings.Contains(string(completion), `"hi"`) {
		t.Fatalf("aggregation lost the content chunk: %s", completion)
	}
}

// The queue wait must be bounded by config; when waiting is off the caller gets
// a retryable error instead of a silent passthrough.
func TestQueueWaitGateRespectsQueueWaitConfig(t *testing.T) {
	cfgMu.Lock()
	prev := cfg
	cfg.QueueWait = false
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg = prev
		cfgMu.Unlock()
	})

	err := queueWaitGate(&storedAuth{}, "qmodel_latest", queueSignal{RetryAfter: 30, Detail: "busy"}, 0)
	if err == nil {
		t.Fatal("queueWaitGate waited although queue_wait is off")
	}
	if !strings.Contains(err.Error(), "capacity queue") {
		t.Fatalf("error = %v, want a retryable queue error", err)
	}
}

// Sanity: detectQueueSignal keeps rejecting ordinary 4xx bodies.
func TestDetectQueueSignalIgnoresPlainErrors(t *testing.T) {
	if _, ok := detectQueueSignal(http.StatusForbidden, []byte(`{"code":"101","message":"Signature invalid"}`)); ok {
		t.Fatal("plain 403 misread as a queue answer")
	}
	if _, ok := detectQueueSignal(http.StatusOK, []byte(`data:{"body":"{\"choices\":[]}"}`)); ok {
		t.Fatal("content frame misread as a queue answer")
	}
}

// A queue timeout must reach the client as 429, not as 200/500: the envelope
// carries the status AND the retry guidance the host needs for cooldown.
func TestQueueBusyErrorCarriesRetryableStatus(t *testing.T) {
	budget := 600 * time.Second
	sig := queueSignal{Queued: true, RetryAfter: 30, Detail: "RESOURCE_LIMIT"}
	raw := queueBusyError(sig, budget)

	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code              string `json:"code"`
			Message           string `json:"message"`
			HTTPStatus        int    `json:"http_status"`
			RetryAfterSeconds int    `json:"retry_after_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v; raw=%s", err, raw)
	}
	if env.OK {
		t.Fatalf("queue timeout rendered as success: %s", raw)
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429; raw=%s", env.Error.HTTPStatus, raw)
	}
	if env.Error.Code != "transient_rate_limit" {
		t.Fatalf("code = %q, want transient_rate_limit (per-model cooldown)", env.Error.Code)
	}
	if env.Error.RetryAfterSeconds != 600 {
		t.Fatalf("retry_after_seconds = %d, want the wait budget", env.Error.RetryAfterSeconds)
	}
	if !strings.Contains(env.Error.Message, "capacity queue") {
		t.Fatalf("message = %q", env.Error.Message)
	}
}

// A mid-stream failure must travel as an RPC error, never as a payload chunk:
// a chunk that merely contains an error object is delivered as a normal event.
func TestStreamEmitFailureUsesErrorField(t *testing.T) {
	sent, err := streamFailureBody("stream-1", "busy", &pluginError{
		Code:              "transient_rate_limit",
		Message:           "busy",
		HTTPStatus:        http.StatusTooManyRequests,
		RetryAfterSeconds: 30,
	})
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatalf("decode emit body: %v; raw=%s", err, sent)
	}
	if _, ok := body["payload"]; ok {
		t.Fatalf("failure sent as a payload chunk: %s", sent)
	}
	if body["error"] != "busy" {
		t.Fatalf("error = %#v, want the message", body["error"])
	}
	detail, ok := body["error_detail"].(map[string]any)
	if !ok {
		t.Fatalf("error_detail missing: %s", sent)
	}
	if detail["http_status"] != float64(http.StatusTooManyRequests) {
		t.Fatalf("error_detail.http_status = %#v", detail["http_status"])
	}
	if detail["retry_after_seconds"] != float64(30) {
		t.Fatalf("error_detail.retry_after_seconds = %#v", detail["retry_after_seconds"])
	}
}

// bufio buffers ahead: a peek that only replays the visible head truncates the
// stream, and the client then gets "closed before any chunk carried
// finish_reason" for a perfectly good response. Every byte must survive.
func TestPeekQueueSignalKeepsBytesBufferedAhead(t *testing.T) {
	var b strings.Builder
	b.WriteString(`data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"a\"}}]}","statusCodeValue":200}` + "\n\n")
	for i := 0; i < 400; i++ {
		b.WriteString(`data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"x\"}}]}","statusCodeValue":200}` + "\n\n")
	}
	b.WriteString(`data:{"body":"{\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}` + "\n\n")
	b.WriteString(`data:{"body":"[DONE]"}` + "\n\n")
	_, queued, replay, err := peekQueueSignal(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("peek error: %v", err)
	}
	if queued {
		t.Fatal("content stream misread as a queue answer")
	}
	got, err := io.ReadAll(replay)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if string(got) != b.String() {
		t.Fatalf("replay dropped %d of %d bytes (bufio lookahead)", len(b.String())-len(got), len(b.String()))
	}
	if !strings.Contains(string(got), `finish_reason`) {
		t.Fatal("finish_reason frame lost in replay")
	}
}
