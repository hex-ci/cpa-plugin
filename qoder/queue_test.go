package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// A 10605 answer means the model is busy, not that the account or key is at
// fault. Misreading it as an account error makes callers rotate accounts and
// burn every credential on the same model queue.
func TestDetectQueueSignal(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		queued bool
		retry  int
	}{
		{
			name:   "code 10605 with nested message",
			status: http.StatusForbidden,
			body:   `{"code":"10605","message":"{\"isQueued\":true,\"retryAfterSeconds\": 30}"}`,
			queued: true, retry: 30,
		},
		{
			name:   "queued marker without duration",
			status: http.StatusTooManyRequests,
			body:   `{"msg":"RESOURCE_LIMIT","isQueued":true}`,
			queued: true, retry: defaultQueueRetrySeconds,
		},
		{
			name:   "sse envelope status",
			status: http.StatusServiceUnavailable,
			body:   `{"statusCodeValue":403,"body":"{\"code\":\"10605\"}"}`,
			queued: true, retry: defaultQueueRetrySeconds,
		},
		{
			name:   "auth failure is not a queue",
			status: http.StatusForbidden,
			body:   `{"code":"12153","message":"TOKEN_EXPIRE"}`,
			queued: false,
		},
		{
			name:   "queue body on a 200 is not a queue",
			status: http.StatusOK,
			body:   `{"code":"10605"}`,
			queued: false,
		},
		{
			name:   "nonsense duration falls back",
			status: http.StatusForbidden,
			body:   `{"code":"10605","message":"retryAfterSeconds: 999999"}`,
			queued: true, retry: defaultQueueRetrySeconds,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, ok := detectQueueSignal(tc.status, []byte(tc.body))
			if ok != tc.queued {
				t.Fatalf("detected=%t, want %t", ok, tc.queued)
			}
			if ok && sig.RetryAfter != tc.retry {
				t.Fatalf("retryAfter=%d, want %d", sig.RetryAfter, tc.retry)
			}
		})
	}
}

// stubQueue replaces the status poll for one test.
func stubQueue(t *testing.T, fn func(call int) (*queueStatus, error)) *int {
	t.Helper()
	calls := 0
	old := queueStatusFetcher
	queueStatusFetcher = func(_ *storedAuth, _, _ string) (*queueStatus, error) {
		calls++
		return fn(calls)
	}
	t.Cleanup(func() { queueStatusFetcher = old })
	return &calls
}

func TestAwaitQueueTurnReturnsWhenQueueClears(t *testing.T) {
	stubQueue(t, func(call int) (*queueStatus, error) {
		if call < 3 {
			return &queueStatus{IsQueued: true, RetryAfterSeconds: 1}, nil
		}
		return &queueStatus{IsQueued: false}, nil
	})
	var slept []time.Duration
	err := awaitQueueTurn(context.Background(), &storedAuth{}, "rs", "qmodel_38max", time.Minute, time.Second,
		func(d time.Duration) { slept = append(slept, d) })
	if err != nil {
		t.Fatalf("awaitQueueTurn: %v", err)
	}
	if len(slept) != 2 {
		t.Fatalf("slept %d times (%v), want 2", len(slept), slept)
	}
}

func TestAwaitQueueTurnHonoursBudget(t *testing.T) {
	stubQueue(t, func(int) (*queueStatus, error) {
		return &queueStatus{IsQueued: true, RetryAfterSeconds: 30}, nil
	})
	err := awaitQueueTurn(context.Background(), &storedAuth{}, "rs", "m", 5*time.Second, 30*time.Second,
		func(time.Duration) {})
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("err = %v, want ErrQueueTimeout", err)
	}
}

// Older regions never had the queue endpoint: sleep the advertised hint once
// and let the caller retry, instead of failing the request.
func TestAwaitQueueTurnFallsBackWhenEndpointMissing(t *testing.T) {
	stubQueue(t, func(int) (*queueStatus, error) {
		return nil, errQueueStatusUnsupported
	})
	var slept []time.Duration
	err := awaitQueueTurn(context.Background(), &storedAuth{}, "rs", "m", time.Minute, 20*time.Second,
		func(d time.Duration) { slept = append(slept, d) })
	if err != nil {
		t.Fatalf("awaitQueueTurn: %v", err)
	}
	if len(slept) != 1 || slept[0] != 20*time.Second {
		t.Fatalf("slept %v, want one 20s wait", slept)
	}
}

func TestAwaitQueueTurnGivesUpAfterRepeatedPollFailures(t *testing.T) {
	stubQueue(t, func(int) (*queueStatus, error) {
		return nil, errors.New("boom")
	})
	err := awaitQueueTurn(context.Background(), &storedAuth{}, "rs", "m", time.Minute, time.Second,
		func(time.Duration) {})
	if err == nil || errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("err = %v, want a poll-failure error", err)
	}
}

func TestAwaitQueueTurnStopsOnContextCancel(t *testing.T) {
	stubQueue(t, func(int) (*queueStatus, error) {
		return &queueStatus{IsQueued: true, RetryAfterSeconds: 1}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := awaitQueueTurn(ctx, &storedAuth{}, "rs", "m", time.Minute, time.Second, func(time.Duration) {}); err == nil {
		t.Fatal("cancelled context was ignored")
	}
}

// The real 10605 answer arrives as an SSE envelope over HTTP 200, with the
// queue document escaped twice. Reading only the transport status makes the
// plugin hand the client an empty 200 instead of waiting or reporting.
func TestDetectQueueSignalUnwrapsSSEEnvelope(t *testing.T) {
	body := `data:{"headers":{"Content-Type":["application/json"],"Retry-After":["30"]},` +
		`"body":"{\"code\":\"403\",\"message\":\"{\\\"code\\\":\\\"10605\\\",` +
		`\\\"message\\\":\\\"{\\\\\\\"isQueued\\\\\\\":true,\\\\\\\"retryAfterSeconds\\\\\\\":30}\\\",` +
		`\\\"subCode\\\":\\\"CALL_COUNT_LIMIT\\\"}\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}`
	sig, ok := detectQueueSignal(http.StatusOK, []byte(body))
	if !ok {
		t.Fatalf("queue not detected in %s", body)
	}
	if sig.RetryAfter != 30 {
		t.Fatalf("retryAfter = %d, want 30", sig.RetryAfter)
	}
}

// Error envelopes inside HTTP 200 must be recognised, otherwise the aggregate
// path folds them into an empty completion.
func TestSSEErrorEnvelopeDetection(t *testing.T) {
	outer := map[string]any{"statusCodeValue": float64(403), "statusCode": "FORBIDDEN"}
	if code, _ := sseErrorEnvelope(outer, `{"code":"10605","message":"busy"}`); code == 0 {
		t.Fatal("403 envelope not detected")
	}
	plain := map[string]any{"statusCodeValue": float64(200)}
	if code, _ := sseErrorEnvelope(plain, `{"choices":[{"delta":{"content":"hi"}}]}`); code != 0 {
		t.Fatalf("content frame flagged as error: %d", code)
	}
	inner := map[string]any{"statusCodeValue": float64(200)}
	if code, _ := sseErrorEnvelope(inner, `{"code":"400","message":"bad"}`); code != 400 {
		t.Fatalf("inner error code = %d, want 400", code)
	}
}
