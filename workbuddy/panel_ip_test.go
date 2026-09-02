package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func egressResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestFetchEgressIPUsesSingleRoutingSnapshot(t *testing.T) {
	body := handlerSource(t, "panel.go", "func fetchEgressIPWithCallback(callbackID string)")
	if got := strings.Count(body, "currentProxyState()"); got != 1 {
		t.Fatalf("fetchEgressIP loads proxy state %d times, want 1", got)
	}
	if !strings.Contains(body, "state := currentProxyState()") {
		t.Fatal("fetchEgressIP does not capture the routing snapshot")
	}
	if !strings.Contains(body, "hostHTTPDoWithStateAndCallback(state, req, callbackID)") {
		t.Fatal("fetchEgressIP does not use the captured snapshot and callback for its HTTP route")
	}
}

func TestFetchEgressIPUsesExplicitPluginProxy(t *testing.T) {
	old := currentProxyState()
	defer proxyState.Store(old)

	called := false
	proxyState.Store(&proxyRoutingState{
		mode: proxyModeExplicit,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			if req.Method != http.MethodGet || req.URL.String() != egressIPURL {
				t.Fatalf("request = %s %s", req.Method, req.URL)
			}
			return egressResponse(http.StatusOK, `{"ip":"203.0.113.7"}`), nil
		})},
	})

	got, err := fetchEgressIP()
	if err != nil {
		t.Fatal(err)
	}
	if !called || got != "203.0.113.7" {
		t.Fatalf("called=%v ip=%q", called, got)
	}
}

func TestFetchEgressIPRejectsWindowsInheritedRoute(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows inherited bridge policy")
	}
	oldState := currentProxyState()
	oldShared := sharedHTTPClient()
	t.Cleanup(func() {
		proxyState.Store(oldState)
		sharedClient = oldShared
	})

	directCalls := 0
	sharedClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		directCalls++
		return egressResponse(http.StatusOK, `{"ip":"192.0.2.1"}`), nil
	})}
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})

	if _, err := fetchEgressIP(); err == nil {
		t.Fatal("Windows inherited probe returned the direct egress address")
	}
	if directCalls != 0 {
		t.Fatalf("Windows inherited probe made %d direct requests", directCalls)
	}
}

func TestFetchEgressIPValidatesResponse(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "non-200", status: http.StatusBadGateway, body: `{"ip":"203.0.113.7"}`},
		{name: "invalid json", status: http.StatusOK, body: `{`},
		{name: "missing ip", status: http.StatusOK, body: `{}`},
		{name: "invalid ip", status: http.StatusOK, body: `{"ip":"not-an-ip"}`},
	}
	old := currentProxyState()
	defer proxyState.Store(old)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxyState.Store(&proxyRoutingState{
				mode: proxyModeExplicit,
				client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return egressResponse(tt.status, tt.body), nil
				})},
			})
			if _, err := fetchEgressIP(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestManagementRegistersAndServesEgressIP(t *testing.T) {
	found := false
	for _, route := range managementRegistration().Routes {
		if route.Method == http.MethodGet && route.Path == "/plugins/workbuddy/egress-ip" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("egress-ip route not registered")
	}

	old := currentProxyState()
	defer proxyState.Store(old)
	proxyState.Store(&proxyRoutingState{
		mode: proxyModeExplicit,
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return egressResponse(http.StatusOK, `{"ip":"2001:db8::1"}`), nil
		})},
	})

	raw, err := handleManagement(mustJSON(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedManagementBasePath() + "/plugins/workbuddy/egress-ip",
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"ip":"2001:db8::1"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestManagementEgressIPFailureIsGeneric(t *testing.T) {
	old := currentProxyState()
	defer proxyState.Store(old)
	proxyState.Store(&proxyRoutingState{
		mode: proxyModeExplicit,
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("secret proxy detail")
		})},
	})

	raw, err := handleManagement(mustJSON(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedManagementBasePath() + "/plugins/workbuddy/egress-ip",
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
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), "secret proxy detail") {
		t.Fatalf("response leaked transport error: %s", resp.Body)
	}
}

func TestPanelManualRefreshAuthenticatesBeforeEgressIP(t *testing.T) {
	html := strings.ReplaceAll(string(panelHTML), "\r\n", "\n")
	start := strings.Index(html, "async function load(force, btn){")
	if start < 0 {
		t.Fatal("load function not found")
	}
	end := strings.Index(html[start:], "\nfunction checkinResultToast")
	if end < 0 {
		t.Fatal("load function end not found")
	}
	body := html[start : start+end]
	refresh := strings.Index(body, `await api("/refresh"`)
	egress := strings.Index(body, `if(force&&manualRefresh) loadEgressIP();`)
	render := strings.Index(body, `document.getElementById("autoToggle")`)
	if refresh < 0 || egress < 0 || render < 0 {
		t.Fatalf("manual refresh sequence is incomplete: refresh=%d egress=%d render=%d", refresh, egress, render)
	}
	if egress < refresh {
		t.Fatal("manual refresh starts egress IP lookup before /refresh authentication completes")
	}
	if egress > render {
		t.Fatal("manual refresh delays egress IP lookup until after account rendering starts")
	}
	if strings.Contains(body, `await loadEgressIP()`) {
		t.Fatal("manual refresh blocks account rendering on the egress IP lookup")
	}
}

func TestPanelProgrammaticForcedLoadDoesNotRefreshEgressIP(t *testing.T) {
	html := strings.ReplaceAll(string(panelHTML), "\r\n", "\n")
	start := strings.Index(html, "async function load(force, btn){")
	if start < 0 {
		t.Fatal("load function not found")
	}
	end := strings.Index(html[start:], "\nfunction checkinResultToast")
	if end < 0 {
		t.Fatal("load function end not found")
	}
	body := html[start : start+end]
	manual := strings.Index(body, `const manualRefresh=!!btn;`)
	fallback := strings.Index(body, `if(!btn) btn=document.getElementById("refreshBtn");`)
	refresh := strings.Index(body, `await api("/refresh"`)
	egress := strings.Index(body, `if(force&&manualRefresh) loadEgressIP();`)
	if manual < 0 || fallback < 0 || refresh < 0 || egress < 0 {
		t.Fatalf("programmatic refresh contract is incomplete: manual=%d fallback=%d refresh=%d egress=%d", manual, fallback, refresh, egress)
	}
	if manual > fallback {
		t.Fatal("manual refresh state is captured after the fallback button assignment")
	}
	if egress < refresh {
		t.Fatal("forced load can start egress IP lookup before /refresh succeeds")
	}
	if got := strings.Count(body, `loadEgressIP();`); got != 1 {
		t.Fatalf("load contains %d egress IP lookups, want exactly the guarded manual lookup", got)
	}
}

func TestPanelShowsAndRefreshesEgressIP(t *testing.T) {
	html := strings.ReplaceAll(string(panelHTML), "\r\n", "\n")
	required := []struct {
		name string
		want string
	}{
		{name: "footer", want: `<strong id="egressIp" role="status">`},
		{name: "label", want: `当前出口 IP`},
		{name: "loader", want: `async function loadEgressIP()`},
		{name: "endpoint", want: `api("/egress-ip")`},
		{name: "key save", want: `async function saveKey(){
  const v=document.getElementById("keyInput").value.trim();
  if(!v)return;
  if(!storeSessionKey(v)){showAuth();return}
  document.getElementById("authBox").style.display="none";
  if(await load(false)) loadEgressIP();
}`},
		{name: "refresh button", want: `<button id="refreshBtn" onclick="load(true,this)">刷新数据</button>`},
		{name: "explicit refresh click", want: `if(force&&manualRefresh) loadEgressIP();`},
		{name: "keyed startup", want: `async function loadInitial(){
  if(getKey()){
    if(await load(false)) loadEgressIP();
  }else{
    showAuth();
  }
}
loadInitial();`},
		{name: "successful account load result", want: `    return true;
  }catch(e){`},
		{name: "failed account load result", want: `    return false;
  }finally{`},
	}
	for _, requirement := range required {
		if !strings.Contains(html, requirement.want) {
			t.Fatalf("panel missing %s contract %q", requirement.name, requirement.want)
		}
	}
	if got := strings.Count(html, `loadEgressIP();`); got != 3 {
		t.Fatalf("loadEgressIP call count = %d, want 3", got)
	}
	if !strings.Contains(html, `el.textContent="不可用"`) {
		t.Fatal("panel lacks non-blocking unavailable state")
	}
}
