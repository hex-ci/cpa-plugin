// queue.go handles upstream capacity queueing.
//
// Qoder answers a busy model with HTTP 403 and a business code of 10605 whose
// `message` is itself a JSON document carrying retryAfterSeconds. The queue is
// per model, not per account: switching accounts makes it worse, because every
// account then hits the same queue and each one burns cooldown. The official
// client keeps the request open and polls /api/v2/service/ask/queue/status
// until isQueued goes false.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// queueCode is the upstream business code that means "model capacity".
	queueCode = "10605"
	// defaultQueueRetrySeconds is used when 10605 arrives without a duration.
	defaultQueueRetrySeconds = 30
	// maxQueueRetries bounds how many times one request re-queues before the
	// caller gets a retryable error instead of being held forever.
	maxQueueRetries = 3
	// queuePollFloorMs prevents a pathological retryAfterSeconds (0 or 1) from
	// turning the wait into a busy loop.
	queuePollFloorMs = 500
)

var (
	retryAfterRe = regexp.MustCompile(`(?i)"?retryAfterSeconds"?\s*[:=]\s*(\d+)`)
	queueWarnRe  = regexp.MustCompile(`(?i)isqueued|resource_limit|"queue"`)
)

// ErrQueueTimeout signals that the waiting budget ran out; callers surface it
// as 429 with Retry-After rather than a generic upstream error.
var ErrQueueTimeout = errors.New("upstream model queue wait budget exhausted")

// queueSignal is what a 10605 response tells us.
type queueSignal struct {
	Queued     bool
	RetryAfter int // seconds
	Detail     string
}

// detectQueueSignal reports whether status+body is a capacity queue answer.
//
// Three shapes arrive in practice: a 403 with `{"code":"10605","message":"{...}"}`,
// a 429, and an SSE envelope whose statusCodeValue is 403 with the same body.
// All of them carry the code or the isQueued marker; nothing else does, so the
// marker test is the discriminator.
func detectQueueSignal(status int, body []byte) (queueSignal, bool) {
	// The SSE transport answers HTTP 200 and carries the real status inside the
	// envelope, so the transport status alone is not the discriminator.
	effective := status
	if v := envelopeStatus(body); v != 0 {
		effective = v
	}
	if effective != http.StatusForbidden && effective != http.StatusTooManyRequests &&
		effective != http.StatusServiceUnavailable {
		return queueSignal{}, false
	}
	// Upstream nests the queue document in one or more JSON strings, so every
	// quote arrives backslash-escaped; flattening first keeps the marker and
	// duration patterns readable.
	flat := strings.ReplaceAll(string(body), "\\", "")
	lower := strings.ToLower(flat)
	if !strings.Contains(lower, queueCode) && !queueWarnRe.MatchString(lower) {
		return queueSignal{}, false
	}
	retry := defaultQueueRetrySeconds
	if m := retryAfterRe.FindStringSubmatch(flat); len(m) == 2 {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 0 && v <= 3600 {
			retry = v
		}
	}
	return queueSignal{Queued: true, RetryAfter: retry, Detail: truncateRedacted(flat, 200)}, true
}

// envelopeStatus returns statusCodeValue from an SSE-envelope body, or 0 when
// the body is not an envelope.
func envelopeStatus(body []byte) int {
	// Callers hand over the raw SSE bytes, which start with a "data:" line.
	text := strings.TrimSpace(string(body))
	text = strings.TrimPrefix(text, "data:")
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	var probe struct {
		StatusCodeValue int `json:"statusCodeValue"`
	}
	if json.Unmarshal([]byte(text), &probe) != nil {
		return 0
	}
	return probe.StatusCodeValue
}

// newRequestSetID mints the id used to correlate queue status polls. The value
// is opaque to upstream; it only has to be unique per request.
func newRequestSetID() string { return randomHex(16) }

// queueStatus mirrors GET /api/v2/service/ask/queue/status.
type queueStatus struct {
	IsQueued          bool   `json:"isQueued"`
	ServiceAvailable  *bool  `json:"serviceAvailable"`
	QueueCount        int    `json:"queueCount"`
	QueueType         string `json:"queueType"`
	RetryAfterSeconds int    `json:"retryAfterSeconds"`
	WaitTime          int    `json:"waitTime"`
	ModelKey          string `json:"modelKey"`
}

// errQueueStatusUnsupported means this realm/route has no queue endpoint; the
// caller falls back to sleeping the advertised duration.
var errQueueStatusUnsupported = errors.New("queue status endpoint unsupported")

// queueStatusFetcher is the poll seam; tests replace it.
var queueStatusFetcher = func(sa *storedAuth, requestSetID, modelKey string) (*queueStatus, error) {
	return fetchQueueStatus(sa, requestSetID, modelKey)
}

// fetchQueueStatus performs one queue-status poll.
func fetchQueueStatus(sa *storedAuth, requestSetID, modelKey string) (*queueStatus, error) {
	r := realmOf(sa)
	q := url.Values{}
	q.Set("requestSetId", requestSetID)
	q.Set("modelKey", modelKey)
	u := r.queueStatusURL() + "?" + q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	accountHeaders(r)(req)
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("X-Request-ID", randomHex(16))

	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, errQueueStatusUnsupported
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("queue status http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 120))
	}
	var st queueStatus
	if err := json.Unmarshal(resp.Body, &st); err != nil {
		return nil, fmt.Errorf("queue status parse: %w", err)
	}
	return &st, nil
}

// awaitQueueTurn blocks until the model queue says the request may proceed.
//
// Behaviour:
//   - isQueued=false            → return nil, send the request
//   - serviceAvailable=false    → keep waiting (not a failure)
//   - status endpoint missing   → sleep the advertised duration once, then let
//     the caller retry (older regions never had the endpoint)
//   - budget exhausted          → ErrQueueTimeout
func awaitQueueTurn(ctx context.Context, sa *storedAuth, requestSetID, modelKey string, budget time.Duration, hint time.Duration, sleep func(time.Duration)) error {
	if sleep == nil {
		sleep = func(d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	deadline := time.Now().Add(budget)
	if hint <= 0 {
		hint = defaultQueueRetrySeconds * time.Second
	}
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := queueStatusFetcher(sa, requestSetID, modelKey)
		switch {
		case errors.Is(err, errQueueStatusUnsupported):
			// No polling available: honour the advertised delay once and let the
			// caller decide what to do with the retry.
			if time.Now().Add(hint).After(deadline) {
				return ErrQueueTimeout
			}
			sleep(hint)
			return nil
		case err != nil:
			failures++
			if failures >= 3 {
				return fmt.Errorf("queue status unavailable after %d attempts: %w", failures, err)
			}
		default:
			failures = 0
			if !st.IsQueued {
				return nil
			}
			wait := st.RetryAfterSeconds
			if wait <= 0 {
				wait = st.WaitTime
			}
			if wait <= 0 {
				wait = int(hint / time.Second)
			}
			if wait < queuePollFloorMs/1000+1 {
				wait = queuePollFloorMs/1000 + 1
			}
			hint = time.Duration(wait) * time.Second
		}
		if time.Now().Add(hint).After(deadline) {
			return ErrQueueTimeout
		}
		sleep(hint)
	}
}
