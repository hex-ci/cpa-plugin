// plugin_errors.go carries structured plugin errors to the host.
//
// The host parses an envelope error into a structured rpcError only when the
// error block carries http_status: without it every failure degrades to
// HTTP 500 internal_server_error, so a "try again later" answer reaches the
// client as a hard error and the host's retry/cooldown logic cannot tell it
// apart from a real fault. queueBusyError is the one case the plugin can
// classify precisely: upstream answered 10605 and the wait budget ran out.
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// pluginError is the host-visible error block. It mirrors pluginabi.Error plus
// the fields newer hosts understand (retry_after_seconds, credential_scoped);
// older hosts ignore what they do not know, so sending them is safe.
type pluginError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	CredentialScoped  bool   `json:"credential_scoped,omitempty"`
}

// failedEnvelope renders a failure the host can classify. code carries the
// machine-readable class (transient_rate_limit / credential_quota / ...), status
// the HTTP status the client should see.
func failedEnvelope(code, message string, status int, retryAfter time.Duration, credentialScoped bool) []byte {
	errBlock := pluginError{
		Code:             code,
		Message:          redactSecrets(message),
		HTTPStatus:       status,
		CredentialScoped: credentialScoped,
	}
	if retryAfter > 0 {
		errBlock.RetryAfterSeconds = int(retryAfter / time.Second)
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: errBlock})
	return raw
}

// queueBusyError answers "upstream queued this model past the wait budget": a
// transient per-model 429, so the host cools only this model and the client can
// retry with the advertised delay instead of treating it as a fault.
func queueBusyError(signal queueSignal, budget time.Duration) []byte {
	return failedEnvelope(
		"transient_rate_limit",
		queuedError(signal.Detail, budget).Error(),
		http.StatusTooManyRequests,
		budget,
		false,
	)
}

// envelopeErrorDetail pulls code and message out of an envelope error, which is
// either a local envelopeError or the structured pluginError a host sends back.
func envelopeErrorDetail(env envelope) (string, string, bool) {
	switch v := env.Error.(type) {
	case pluginError:
		return v.Code, v.Message, true
	case *pluginError:
		if v == nil {
			return "", "", false
		}
		return v.Code, v.Message, true
	case envelopeError:
		return v.Code, v.Message, true
	case *envelopeError:
		if v == nil {
			return "", "", false
		}
		return v.Code, v.Message, true
	case map[string]any:
		code, _ := v["code"].(string)
		message, _ := v["message"].(string)
		return code, message, true
	default:
		return "", "", false
	}
}

// envelopeErrorMessage is envelopeErrorDetail without the code.
func envelopeErrorMessage(env envelope) string {
	_, message, _ := envelopeErrorDetail(env)
	return message
}

// queueBusyDetail is queueBusyError as the struct form used by stream emits.
func queueBusyDetail(signal queueSignal, budget time.Duration) *pluginError {
	return &pluginError{
		Code:              "transient_rate_limit",
		Message:           queuedError(signal.Detail, budget).Error(),
		HTTPStatus:        http.StatusTooManyRequests,
		RetryAfterSeconds: int(budget / time.Second),
	}
}

// upstreamFailureDetail classifies a non-2xx upstream answer for stream emits.
func upstreamFailureDetail(status int, message string) *pluginError {
	code := "upstream_error"
	credentialScoped := false
	switch status {
	case http.StatusUnauthorized:
		code = "credential_rejected"
		credentialScoped = true
	case http.StatusForbidden:
		code = "credential_quota"
	case http.StatusTooManyRequests:
		code = "transient_rate_limit"
	}
	return &pluginError{
		Code:             code,
		Message:          message,
		HTTPStatus:       status,
		CredentialScoped: credentialScoped,
	}
}

// pluginFailure carries a classified failure out of the executors so the
// dispatcher can render it as an envelope instead of a bare Go error (which
// would lose the HTTP status on the way to the host).
type pluginFailure struct {
	detail pluginError
}

func (f pluginFailure) Error() string { return f.detail.Message }

func (f pluginFailure) envelope() []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: f.detail})
	return raw
}

func newPluginFailure(detail *pluginError) pluginFailure {
	if detail == nil {
		detail = &pluginError{Code: "internal_error", Message: "plugin failure"}
	}
	return pluginFailure{detail: *detail}
}

// failureEnvelope renders err as a classified envelope. ok is false when the
// error carries no classification, so the caller keeps its existing handling.
func failureEnvelope(err error) ([]byte, bool) {
	var failure pluginFailure
	if errors.As(err, &failure) {
		return failure.envelope(), true
	}
	return nil, false
}
