package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementRegistrationExposesPanelAndAccounts(t *testing.T) {
	reg := managementRegistration()
	var sawAccounts, sawPanel, sawCredits bool
	wantRoutes := map[string]string{
		"/plugins/" + providerName + "/login/start":    http.MethodPost,
		"/plugins/" + providerName + "/login/status":   http.MethodGet,
		"/plugins/" + providerName + "/login/current":  http.MethodGet,
		"/plugins/" + providerName + "/login/callback": http.MethodPost,
		"/plugins/" + providerName + "/login/cancel":   http.MethodPost,
		"/plugins/" + providerName + "/credits":        http.MethodGet,
		"/plugins/" + providerName + "/checkin":        http.MethodPost,
		"/plugins/" + providerName + "/checkin/config": http.MethodPost,
	}
	for _, route := range reg.Routes {
		if method, ok := wantRoutes[route.Path]; ok {
			if route.Method != method {
				t.Errorf("%s method = %s, want %s", route.Path, route.Method, method)
			}
			delete(wantRoutes, route.Path)
		}
		if route.Method == http.MethodGet && route.Path == "/plugins/"+providerName+"/accounts" {
			sawAccounts = true
		}
		if route.Method == http.MethodGet && route.Path == "/plugins/"+providerName+"/credits" {
			sawCredits = true
		}
	}
	if len(wantRoutes) != 0 {
		t.Errorf("login routes missing from the registration: %v", wantRoutes)
	}
	var sawAuthorize bool
	for _, res := range reg.Resources {
		if res.Path == "/panel" && res.Menu == displayName {
			sawPanel = true
		}
		if res.Path == "/authorize" && res.Menu == "" {
			sawAuthorize = true
		}
	}
	if !sawAuthorize {
		t.Error("the OAuth callback target must be a registered resource route")
	}
	if !sawCredits {
		t.Error("the dashboard reads quota per card, so /credits must be registered")
	}
	if !sawAccounts || !sawPanel {
		t.Fatalf("registration lacks the accounts route (%t) or the panel resource (%t)", sawAccounts, sawPanel)
	}
}

func TestManagementAuthHonoursKeyWhenConfigured(t *testing.T) {
	managementKey.Store("")
	t.Cleanup(func() { managementKey.Store("") })

	bare := managementRequestWire{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodGet}}
	if !managementAuthorized(bare) {
		t.Error("without a configured key the host's own auth is the only gate")
	}

	managementKey.Store("s3cret")
	header := managementRequestWire{ManagementRequest: pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Headers: http.Header{"Authorization": []string{"Bearer s3cret"}},
	}}
	if !managementAuthorized(header) {
		t.Error("a matching Bearer header must be accepted")
	}
	query := managementRequestWire{ManagementRequest: pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Query:  url.Values{"key": []string{"s3cret"}},
	}}
	if !managementAuthorized(query) {
		t.Error("the panel's ?key= parameter must be accepted")
	}
	wrong := managementRequestWire{ManagementRequest: pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Headers: http.Header{"Authorization": []string{"Bearer nope"}},
	}}
	if managementAuthorized(wrong) {
		t.Error("a wrong key must be rejected")
	}
	if managementAuthorized(managementRequestWire{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodGet}}) {
		t.Error("a missing key must be rejected once one is configured")
	}
}

func TestManagementResponseHelpers(t *testing.T) {
	jsonResp := mgmtJSONResponse(http.StatusTeapot, map[string]any{"a": 1})
	if jsonResp.StatusCode != http.StatusTeapot || len(jsonResp.Body) == 0 {
		t.Fatalf("json response = %+v", jsonResp)
	}
	if got := jsonResp.Headers.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content type = %q", got)
	}
	htmlResp := mgmtHTMLResponse([]byte("<html></html>"))
	if got := htmlResp.Headers.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("content type = %q", got)
	}
}

func TestHandleManagementRoutesUnknownPathsToNotFound(t *testing.T) {
	managementKey.Store("")
	raw, err := handleManagement(mustJSON(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/plugins/" + providerName + "/nope"}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("unknown management paths answer with a 404 body, not an RPC error: %s", raw)
	}
}

func TestPanelIsServedAsHTML(t *testing.T) {
	raw, err := handleManagement(mustJSON(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedResourceBasePath() + "/panel",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(resp.Body) == 0 {
		t.Fatalf("panel response = %d with %d bytes", resp.StatusCode, len(resp.Body))
	}
	if got := resp.Headers.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("content type = %q", got)
	}
	if !strings.Contains(string(resp.Body), `"/plugins/traework"`) {
		t.Error("the served panel must point at the plugin's own endpoints")
	}
	if strings.Contains(string(resp.Body), panelBasePathPlaceholder) {
		t.Error("the management base path placeholder must be substituted before serving")
	}
}

// A resource sub-path other than the panel is not served.
func TestUnknownResourcePathIs404(t *testing.T) {
	raw, err := handleManagement(mustJSON(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedResourceBasePath() + "/nope",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), "404") {
		t.Errorf("body = %q, want a 404 page", resp.Body)
	}
}

// The panel must carry the host's management base path, not a hardcoded one.
func TestPanelUsesTheHostBasePath(t *testing.T) {
	managementBasePath.Store("/v0/management")
	t.Cleanup(func() { managementBasePath.Store("") })
	body := servePanel("/panel")
	if !strings.Contains(string(body), `"/v0/management"`) {
		t.Error("the served panel must embed the host management base path")
	}
}

/* ---------- 面板登录路由 ---------- */

// managementCall drives one management request the way the host does.
func managementCall(t *testing.T, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := handleManagement(mustJSON(req))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("management envelope error: %+v", env.Error)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func loginStartViaManagement(t *testing.T) map[string]any {
	t.Helper()
	managementKey.Store("")
	t.Cleanup(func() { managementKey.Store("") })
	resp := managementCall(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/plugins/" + providerName + "/login/start",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login start status = %d (%s)", resp.StatusCode, resp.Body)
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["state"] == "" || payload["authorize_url"] == "" {
		t.Fatalf("login start payload = %+v", payload)
	}
	return payload
}

func TestManagementLoginFlowRoutesWork(t *testing.T) {
	stubPanelExchange(t, "route-code", nil)
	stubAuthSave(t)

	start := loginStartViaManagement(t)
	state := start["state"].(string)

	status := managementCall(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/plugins/" + providerName + "/login/status",
		Query:  url.Values{"state": []string{state}},
	})
	if status.StatusCode != http.StatusOK {
		t.Fatalf("status route = %d (%s)", status.StatusCode, status.Body)
	}
	var pending map[string]any
	if err := json.Unmarshal(status.Body, &pending); err != nil {
		t.Fatal(err)
	}
	if pending["status"] != string(pluginapi.AuthLoginStatusPending) {
		t.Fatalf("status payload = %+v, want pending", pending)
	}

	submit := managementCall(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/plugins/" + providerName + "/login/callback",
		Body:   mustJSON(map[string]any{"state": state, "input": "route-code"}),
	})
	var done map[string]any
	if err := json.Unmarshal(submit.Body, &done); err != nil {
		t.Fatal(err)
	}
	if done["status"] != string(pluginapi.AuthLoginStatusSuccess) {
		t.Fatalf("submit payload = %+v, want success", done)
	}

	// A fresh attempt can be dropped.
	second := loginStartViaManagement(t)
	cancel := managementCall(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/plugins/" + providerName + "/login/cancel",
		Body:   mustJSON(map[string]any{"state": second["state"].(string)}),
	})
	if cancel.StatusCode != http.StatusOK {
		t.Fatalf("cancel route = %d (%s)", cancel.StatusCode, cancel.Body)
	}
}

func TestManagementLoginRoutesNeedTheKeyWhenOneIsConfigured(t *testing.T) {
	managementKey.Store("s3cret")
	t.Cleanup(func() { managementKey.Store("") })

	resp := managementCall(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/plugins/" + providerName + "/login/start",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("keyless login start = %d, want 401", resp.StatusCode)
	}

	authorized := managementCall(t, pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    "/plugins/" + providerName + "/login/start",
		Headers: http.Header{"Authorization": []string{"Bearer s3cret"}},
	})
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("authorized login start = %d (%s)", authorized.StatusCode, authorized.Body)
	}
	var payload map[string]any
	_ = json.Unmarshal(authorized.Body, &payload)
	if state, _ := payload["state"].(string); state != "" {
		t.Cleanup(func() { _, _ = panelLoginCancel(state) })
	}
}

/* ---------- 公网回调路由（部署在反代后面时的回调目标） ---------- */

func resourceCall(t *testing.T, query url.Values) (int, string) {
	t.Helper()
	resp := managementCall(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedResourceBasePath() + "/authorize",
		Query:  query,
	})
	return resp.StatusCode, string(resp.Body)
}

func TestAuthorizeResourceCapturesTheCode(t *testing.T) {
	stubPanelExchange(t, "public-route-code", nil)
	saved := stubAuthSave(t)
	state, _ := panelAttempt(t)

	status, body := resourceCall(t, url.Values{
		"authCodeInfo": []string{`{"AuthCode":"public-route-code"}`},
	})
	if status != http.StatusOK {
		t.Fatalf("authorize resource = %d", status)
	}
	if !strings.Contains(body, "登录成功") {
		t.Fatalf("authorize resource body = %q, want the success page", body)
	}

	payload, err := panelLoginStatus(state)
	if err != nil {
		t.Fatal(err)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusSuccess) {
		t.Fatalf("panel status after the public callback = %+v", payload)
	}
	if len(*saved) != 1 {
		t.Fatalf("saved auth files = %d, want one", len(*saved))
	}

	// A second hit with no pending attempt must not fabricate a login.
	if _, body := resourceCall(t, url.Values{"code": []string{"late-code"}}); !strings.Contains(body, "没有进行中的登录") {
		t.Errorf("a callback without a pending login = %q", body)
	}
}

func TestAuthorizeResourceReportsFailures(t *testing.T) {
	state, _ := panelAttempt(t)
	status, body := resourceCall(t, url.Values{
		"error_msg": []string{"用户取消登录"},
	})
	if status != http.StatusOK || !strings.Contains(body, "登录失败") {
		t.Fatalf("authorize resource = %d %q", status, body)
	}
	payload, err := panelLoginStatus(state)
	if err != nil {
		t.Fatal(err)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusError) {
		t.Fatalf("panel status = %+v, want the failure reported", payload)
	}
	if !strings.Contains(payload["message"].(string), "用户取消登录") {
		t.Errorf("message = %v", payload["message"])
	}
}

func TestAuthorizeResourceIgnoresNonCallbackHits(t *testing.T) {
	status, body := resourceCall(t, url.Values{"_": []string{"1"}})
	if status != http.StatusOK || !strings.Contains(body, "等待登录回调") {
		t.Fatalf("a bare visit = %d %q", status, body)
	}
}
