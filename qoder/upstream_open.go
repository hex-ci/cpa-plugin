// upstream_open.go opens a chat stream and classifies the upstream's answer
// before anything is sent to the client.
//
// The open has to happen synchronously: once a streaming response has started,
// every later failure can only travel as an in-band frame, so a capacity queue,
// an expired credential or a throttled upstream would all reach the client as
// HTTP 200 with an error body. Opening first keeps those cases on the envelope
// path where the status is still ours to choose.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// openUpstreamStream performs one upstream attempt and, when it succeeds,
// returns the live stream plus a reader that replays from its first byte. On a
// capacity queue it waits (bounded by config) and retries; on any other
// non-2xx answer it returns a classified pluginFailure.
func openUpstreamStream(ctx context.Context, sa *storedAuth, encodedBody, chatURL, modelKey string) (*hostHTTPStream, io.Reader, error) {
	cfg := currentConfig()
	for attempt := 0; ; attempt++ {
		httpReq, err := newChatRequest(ctx, sa, encodedBody, chatURL, modelKey)
		if err != nil {
			return nil, nil, newPluginFailure(&pluginError{
				Code:       "internal_error",
				Message:    "cosy: " + err.Error(),
				HTTPStatus: http.StatusInternalServerError,
			})
		}
		stream, statusCode, _, err := hostHTTPDoStream(httpReq)
		if err != nil {
			return nil, nil, newPluginFailure(&pluginError{
				Code:       "upstream_error",
				Message:    "http_error: " + err.Error(),
				HTTPStatus: http.StatusBadGateway,
			})
		}

		if statusCode < 400 {
			sig, queued, replay, peekErr := peekQueueSignal(newHostStreamReader(stream))
			if !queued {
				if peekErr != nil && replay == nil {
					stream.Close()
					return nil, nil, newPluginFailure(&pluginError{
						Code:       "upstream_error",
						Message:    "upstream stream read error: " + peekErr.Error(),
						HTTPStatus: http.StatusBadGateway,
					})
				}
				return stream, replay, nil
			}
			stream.Close()
			if waitErr := queueWaitGate(sa, modelKey, sig, attempt); waitErr != nil {
				return nil, nil, newPluginFailure(queueBusyDetail(sig, queueWaitBudget(cfg)))
			}
			continue
		}

		payload, _ := io.ReadAll(newHostStreamReader(stream))
		stream.Close()
		body := truncateRedacted(string(payload), 200)
		if signal, isQueue := detectQueueSignal(statusCode, payload); isQueue {
			if waitErr := queueWaitGate(sa, modelKey, signal, attempt); waitErr != nil {
				return nil, nil, newPluginFailure(queueBusyDetail(signal, queueWaitBudget(cfg)))
			}
			continue
		}
		failure := upstreamFailureDetail(statusCode, fmt.Sprintf("upstream %d: %s", statusCode, body))
		if authFailed := reconcileAfterOpenFailure(sa, statusCode, string(payload)); authFailed {
			failure.CredentialScoped = true
		}
		return nil, nil, newPluginFailure(failure)
	}
}

// queueWaitBudget is the configured total wait, falling back to the default so a
// zero value never turns into a zero-second budget.
func queueWaitBudget(cfg pluginConfig) time.Duration {
	if cfg.QueueMaxWait <= 0 {
		return time.Duration(defaultQueueRetrySeconds) * time.Second
	}
	return time.Duration(cfg.QueueMaxWait) * time.Second
}

// reconcileAfterOpenFailure lets the account lifecycle react to a hard credit
// error before the failure is reported. It reports whether the whole credential
// (not just this model) should be treated as unusable.
func reconcileAfterOpenFailure(sa *storedAuth, status int, body string) bool {
	if sa == nil || sa.Account.UID == "" {
		return false
	}
	go reconcileByUID(sa.Account.UID, status, body)
	return isHardCreditError(status, body)
}
