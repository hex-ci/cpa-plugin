package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type executorHTTPRequestWire struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecHTTPRequest(raw []byte) ([]byte, error) {
	var req executorHTTPRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if blocked := guardExecutorReadiness(req.AuthID); blocked != nil {
		return blocked, nil
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		return nil, fmt.Errorf("executor HTTP request: method is required")
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("executor HTTP request: %w", err)
	}
	target, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("executor HTTP request: invalid URL: %w", err)
	}
	if err := validateExecutorHTTPURL(target, sa); err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(method, target.String(), bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("executor HTTP request: %w", err)
	}
	httpReq.Header = req.Headers.Clone()
	if httpReq.Header == nil {
		httpReq.Header = make(http.Header)
	}
	backendHeaders(httpReq, sa)
	resp, err := hostHTTPDoWithCallback(httpReq, req.HostCallbackID)
	if err != nil {
		return nil, fmt.Errorf("executor HTTP request: %w", err)
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	})
}

func validateExecutorHTTPURL(target *url.URL, sa *storedAuth) error {
	if target == nil || !target.IsAbs() || target.User != nil || target.Fragment != "" {
		return fmt.Errorf("executor HTTP request: absolute URL without userinfo or fragment is required")
	}
	expected, err := url.Parse(upstreamBaseFor(sa))
	if err != nil {
		return fmt.Errorf("executor HTTP request: invalid upstream base: %w", err)
	}
	if !strings.EqualFold(target.Scheme, expected.Scheme) ||
		!strings.EqualFold(target.Hostname(), expected.Hostname()) ||
		target.Port() != expected.Port() {
		return fmt.Errorf("executor HTTP request: upstream host %q is not allowed", target.Host)
	}
	return nil
}
