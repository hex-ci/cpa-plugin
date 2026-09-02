package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExecutorHTTPRequestForwardsAllowedUpstream(t *testing.T) {
	const authID = "executor-http-ready"
	installModelStatesForTest(t, map[string]modelReadinessState{authID: modelReady})
	oldClient := sharedHTTPClient()
	defer func() { sharedClient = oldClient }()
	var got *http.Request
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"X-Upstream": []string{"ok"}},
			Body:       io.NopCloser(strings.NewReader("response")),
			Request:    req,
		}, nil
	})}
	storage := mustJSON(&storedAuth{
		Auth:    storedTokens{AccessToken: "access-token"},
		Account: storedAccount{UID: "uid-1"},
	})
	raw, err := handleMethod(pluginabi.MethodExecutorHTTPRequest, mustJSON(pluginapi.ExecutorHTTPRequest{
		AuthID:      authID,
		Method:      http.MethodPost,
		URL:         upstreamBaseCN + "/v2/plugin/ping",
		Headers:     http.Header{"Content-Type": []string{"application/json"}},
		Body:        []byte(`{"hello":"world"}`),
		StorageJSON: storage,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("unexpected envelope error: %+v", env.Error)
	}
	var resp pluginapi.ExecutorHTTPResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || string(resp.Body) != "response" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if got == nil || got.Method != http.MethodPost || got.URL.String() != upstreamBaseCN+"/v2/plugin/ping" {
		t.Fatalf("unexpected forwarded request: %+v", got)
	}
	if got.Header.Get("Authorization") != "Bearer access-token" {
		t.Fatalf("missing derived authorization: %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("X-Refresh-Token") != "" {
		t.Fatal("refresh token must not be sent on executor HTTP requests")
	}
}

func TestExecutorHTTPRequestRejectsForeignHost(t *testing.T) {
	const authID = "executor-http-foreign-host"
	installModelStatesForTest(t, map[string]modelReadinessState{authID: modelReady})
	called := false
	oldClient := sharedHTTPClient()
	defer func() { sharedClient = oldClient }()
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}
	storage := mustJSON(&storedAuth{Auth: storedTokens{AccessToken: "access-token"}})
	_, err := handleMethod(pluginabi.MethodExecutorHTTPRequest, mustJSON(pluginapi.ExecutorHTTPRequest{
		AuthID:      authID,
		Method:      http.MethodGet,
		URL:         "https://attacker.example.invalid/steal",
		StorageJSON: storage,
	}))
	if err == nil || !strings.Contains(err.Error(), "upstream host") {
		t.Fatalf("expected foreign-host rejection, got %v", err)
	}
	if called {
		t.Fatal("foreign-host request must not reach HTTP client")
	}
}

func TestExecutorReadinessBlocksBeforeCredentialParsing(t *testing.T) {
	oldClient := sharedHTTPClient()
	httpCalls := 0
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		httpCalls++
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("unexpected executor HTTP")),
			Request:    req,
		}, nil
	})}
	t.Cleanup(func() { sharedClient = oldClient })

	for _, state := range []modelReadinessState{modelFailed, modelNotStarted} {
		t.Run(string(state), func(t *testing.T) {
			executeID := string(state) + "-execute"
			streamID := string(state) + "-stream"
			httpID := string(state) + "-http"
			installModelStatesForTest(t, map[string]modelReadinessState{
				executeID: state,
				streamID:  state,
				httpID:    state,
			})
			before := httpCalls
			for _, handler := range []struct {
				name string
				call func([]byte) ([]byte, error)
				raw  []byte
			}{
				{
					name: "execute",
					call: handleExecExecute,
					raw: mustJSON(executorRequestWire{ExecutorRequest: pluginapi.ExecutorRequest{
						AuthID: executeID, StorageJSON: []byte(`{invalid-storage`),
					}}),
				},
				{
					name: "stream",
					call: handleExecStream,
					raw: mustJSON(executorStreamRequest{ExecutorRequest: pluginapi.ExecutorRequest{
						AuthID: streamID, StorageJSON: []byte(`{invalid-storage`),
					}}),
				},
				{
					name: "http_request",
					call: handleExecHTTPRequest,
					raw: mustJSON(executorHTTPRequestWire{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
						AuthID: httpID, Method: http.MethodGet, URL: "https://attacker.example.invalid/", StorageJSON: []byte(`{invalid-storage`),
					}}),
				},
			} {
				t.Run(handler.name, func(t *testing.T) {
					raw, err := handler.call(handler.raw)
					if err != nil {
						t.Fatalf("readiness guard ran after credential parsing: %v", err)
					}
					assertNotReadyEnvelope(t, raw)
				})
			}
			if httpCalls != before {
				t.Fatalf("blocked executors made %d HTTP calls", httpCalls-before)
			}
		})
	}
}

func TestExecutorReadinessAllowsReadyAndStaleCredentialParsing(t *testing.T) {
	for _, state := range []modelReadinessState{modelReady, modelStale} {
		t.Run(string(state), func(t *testing.T) {
			executeID := string(state) + "-execute-pass"
			streamID := string(state) + "-stream-pass"
			httpID := string(state) + "-http-pass"
			installModelStatesForTest(t, map[string]modelReadinessState{
				executeID: state,
				streamID:  state,
				httpID:    state,
			})
			for _, handler := range []struct {
				name string
				call func([]byte) ([]byte, error)
				raw  []byte
			}{
				{
					name: "execute",
					call: handleExecExecute,
					raw: mustJSON(executorRequestWire{ExecutorRequest: pluginapi.ExecutorRequest{
						AuthID: executeID, StorageJSON: []byte(`{invalid-storage`),
					}}),
				},
				{
					name: "stream",
					call: handleExecStream,
					raw: mustJSON(executorStreamRequest{ExecutorRequest: pluginapi.ExecutorRequest{
						AuthID: streamID, StorageJSON: []byte(`{invalid-storage`),
					}}),
				},
				{
					name: "http_request",
					call: handleExecHTTPRequest,
					raw: mustJSON(executorHTTPRequestWire{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
						AuthID: httpID, Method: http.MethodGet, URL: upstreamBaseCN, StorageJSON: []byte(`{invalid-storage`),
					}}),
				},
			} {
				t.Run(handler.name, func(t *testing.T) {
					raw, err := handler.call(handler.raw)
					if err == nil || !strings.Contains(err.Error(), "storage_parse_error") {
						t.Fatalf("executable state did not reach credential parsing: raw=%s err=%v", raw, err)
					}
				})
			}
		})
	}
}

func TestReadinessGateScope(t *testing.T) {
	const failedAuthID = "scope-failed-auth"
	installModelStatesForTest(t, map[string]modelReadinessState{failedAuthID: modelFailed})
	managementAPIKeyMu.Lock()
	oldManagementKey := managementAPIKey
	managementAPIKey = ""
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldManagementKey
		managementAPIKeyMu.Unlock()
	})

	assertOutsideGate := func(t *testing.T, raw []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if env.Error != nil && env.Error.Code == "not_ready" {
			t.Fatalf("non-executor path was readiness-gated: %#v", env.Error)
		}
	}

	t.Run("management.handle", func(t *testing.T) {
		raw, err := handleMethod(pluginabi.MethodManagementHandle, mustJSON(pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   loadedManagementBasePath() + "/plugins/" + providerName + "/missing",
		}))
		assertOutsideGate(t, raw, err)
	})

	t.Run("management.register", func(t *testing.T) {
		raw, err := handleMethod(pluginabi.MethodManagementRegister, mustJSON(pluginapi.ManagementRegistrationRequest{
			BasePath:         loadedManagementBasePath(),
			ResourceBasePath: loadedResourceBasePath(),
		}))
		assertOutsideGate(t, raw, err)
	})

	t.Run("auth.parse", func(t *testing.T) {
		raw, err := handleMethod(pluginabi.MethodAuthParse, mustJSON(pluginapi.AuthParseRequest{
			Provider: providerName,
			FileName: authFileName,
			RawJSON:  mustJSON(syntheticStoredAuth(t, workBuddyRealmCN)),
		}))
		assertOutsideGate(t, raw, err)
	})

	for _, route := range []struct {
		name   string
		method string
		path   string
		body   []byte
		ip     string
	}{
		{name: "import", method: http.MethodPost, path: "/import", ip: "192.0.2.11"},
		{name: "check-in", method: http.MethodPost, path: "/checkin", body: []byte(`{"auth_index":"missing"}`), ip: "192.0.2.12"},
		{name: "billing", method: http.MethodGet, path: "/credits"},
		{name: "keepalive", method: http.MethodPost, path: "/keepalive", ip: "192.0.2.13"},
	} {
		route := route
		t.Run(route.name, func(t *testing.T) {
			headers := make(http.Header)
			if route.ip != "" {
				headers.Set("X-Forwarded-For", route.ip)
			}
			raw, err := handleMethod(pluginabi.MethodManagementHandle, mustJSON(pluginapi.ManagementRequest{
				Method:  route.method,
				Path:    loadedManagementBasePath() + "/plugins/" + providerName + route.path,
				Headers: headers,
				Body:    route.body,
			}))
			assertOutsideGate(t, raw, err)
		})
	}

	t.Run("panel resource", func(t *testing.T) {
		raw, err := handleMethod(pluginabi.MethodManagementHandle, mustJSON(pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   loadedResourceBasePath() + "/panel",
		}))
		assertOutsideGate(t, raw, err)
	})

	t.Run("count_tokens", func(t *testing.T) {
		raw, err := handleMethod(pluginabi.MethodExecutorCountTokens, mustJSON(pluginapi.ExecutorRequest{AuthID: failedAuthID}))
		assertOutsideGate(t, raw, err)
	})

	t.Run("auth.refresh", func(t *testing.T) {
		const authID = "scope-refresh-auth"
		runtime := installModelStatesForTest(t, map[string]modelReadinessState{authID: modelFailed})
		sa := syntheticStoredAuth(t, workBuddyRealmCN)
		sa.Auth.RefreshToken = "refresh-token"
		oldClient := sharedHTTPClient()
		oldProxy := proxyState.Load()
		calls := 0
		sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"accessToken":"refreshed-access","refreshToken":"refreshed-refresh","expiresIn":3600}}`)),
				Request:    req,
			}, nil
		})}
		proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
		t.Cleanup(func() {
			sharedClient = oldClient
			proxyState.Store(oldProxy)
		})

		raw, err := handleMethod(pluginabi.MethodAuthRefresh, mustJSON(authRefreshRequestWire{
			AuthRefreshRequest: pluginapi.AuthRefreshRequest{AuthID: authID, StorageJSON: mustJSON(sa)},
			HostCallbackID:     "scope-refresh-callback",
		}))
		assertOutsideGate(t, raw, err)
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		var resp pluginapi.AuthRefreshResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		refreshed, err := parseStored(resp.Auth.StorageJSON)
		if err != nil || refreshed.Auth.AccessToken != "refreshed-access" {
			t.Fatalf("refresh response auth=%#v err=%v", resp.Auth, err)
		}
		if calls != 1 {
			t.Fatalf("refresh HTTP calls = %d, want 1", calls)
		}
		if snapshot := runtime.snapshotForAuthID(authID); snapshot.State != modelNotStarted {
			t.Fatalf("successful refresh did not invalidate model readiness: %#v", snapshot)
		}
	})
}

func assertNotReadyEnvelope(t *testing.T, raw []byte) {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "not_ready" || env.Error.Message != "WorkBuddy model catalog is not ready" || env.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("not-ready envelope = %#v", env)
	}
}

func TestInboundRPCWrappersPreserveHostCallbackID(t *testing.T) {
	fixtures := []struct {
		name   string
		raw    []byte
		decode func([]byte) string
	}{
		{
			name: "executor.execute",
			raw:  mustJSON(map[string]any{"host_callback_id": "execute-1"}),
			decode: func(raw []byte) string {
				var wire executorRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
		{
			name: "executor.http_request",
			raw:  mustJSON(map[string]any{"host_callback_id": "http-2"}),
			decode: func(raw []byte) string {
				var wire executorHTTPRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
		{
			name: "model.for_auth",
			raw:  mustJSON(map[string]any{"host_callback_id": "model-3"}),
			decode: func(raw []byte) string {
				var wire authModelRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
		{
			name: "auth.refresh",
			raw:  mustJSON(map[string]any{"host_callback_id": "refresh-3"}),
			decode: func(raw []byte) string {
				var wire authRefreshRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
		{
			name: "management.handle",
			raw:  mustJSON(map[string]any{"host_callback_id": "management-4"}),
			decode: func(raw []byte) string {
				var wire managementRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			if got := fixture.decode(fixture.raw); got == "" {
				t.Fatal("host_callback_id was dropped during decode")
			}
		})
	}
}

func TestInboundHandlersForwardHostCallbackID(t *testing.T) {
	tests := []struct {
		file         string
		signature    string
		requirements []string
	}{
		{
			file:      "main.go",
			signature: "func handleExecExecute(",
			requirements: []string{
				"var req executorRequestWire",
				"hostHTTPDoStreamWithCallback(httpReq, req.HostCallbackID)",
			},
		},
		{
			file:      "main.go",
			signature: "func handleExecStream(",
			requirements: []string{
				"collectUpstreamStream(body, sa, sseFramed, collector, req.HostCallbackID)",
				"pumpUpstreamStream(httpReq, cancel, req.StreamID, sseFramed, req.Model, upstreamModel, authUID, started, req.AuthID, req.HostCallbackID)",
			},
		},
		{
			file:      "executor_http.go",
			signature: "func handleExecHTTPRequest(",
			requirements: []string{
				"var req executorHTTPRequestWire",
				"hostHTTPDoWithCallback(httpReq, req.HostCallbackID)",
			},
		},
		{
			file:      "models.go",
			signature: "func handleModelForAuth(",
			requirements: []string{
				"var req authModelRequestWire",
				"currentModelRuntime().ensureForAuth(req)",
			},
		},
		{
			file:      "oauth.go",
			signature: "func handleRefreshAuth(",
			requirements: []string{
				"var req authRefreshRequestWire",
				"refreshCallWithCallback(sa, req.HostCallbackID)",
			},
		},
		{
			file:      "management.go",
			signature: "func handleManagement(",
			requirements: []string{
				"var req managementRequestWire",
				"fetchEgressIPWithCallback(req.HostCallbackID)",
				"buildDashboardExWithCallback(true, true, req.HostCallbackID)",
				"handleManualCheckinWithCallback(req.ManagementRequest, req.HostCallbackID)",
				"handleCreditsQueryWithCallback(req.ManagementRequest, req.HostCallbackID)",
				"handleClaimTrialWithCallback(req.ManagementRequest, req.HostCallbackID)",
				"handleKeepaliveNowWithCallback(req.ManagementRequest, req.HostCallbackID)",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.signature, func(t *testing.T) {
			body := handlerSource(t, test.file, test.signature)
			for _, requirement := range test.requirements {
				if !strings.Contains(body, requirement) {
					t.Fatalf("handler does not forward callback through %s", requirement)
				}
			}
		})
	}
}

func handlerSource(t *testing.T, file, signature string) string {
	t.Helper()
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), signature)
	if start < 0 {
		t.Fatalf("handler %s not found", signature)
	}
	open := strings.Index(string(source[start:]), "{")
	if open < 0 {
		t.Fatalf("handler %s body not found", signature)
	}
	open += start
	depth := 0
	for end := open; end < len(source); end++ {
		switch source[end] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return string(source[start : end+1])
			}
		}
	}
	t.Fatalf("handler %s body is unclosed", signature)
	return ""
}
