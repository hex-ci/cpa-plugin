package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementUsesRegisteredResourceBasePath(t *testing.T) {
	defer func() {
		_, _ = handleMethod(pluginabi.MethodManagementRegister, mustJSON(pluginapi.ManagementRegistrationRequest{
			BasePath:         "/v0/management",
			ResourceBasePath: "/v0/resource/plugins/workbuddy",
		}))
	}()

	_, err := handleMethod(pluginabi.MethodManagementRegister, mustJSON(pluginapi.ManagementRegistrationRequest{
		BasePath:         "/custom/manage",
		ResourceBasePath: "/custom/resources/workbuddy/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleManagement(mustJSON(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/custom/resources/workbuddy/panel",
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if len(resp.Body) == 0 {
		t.Fatal("empty panel body")
	}
	html := string(resp.Body)
	if !strings.Contains(html, `const MANAGEMENT_BASE_PATH="/custom/manage";`) {
		t.Fatalf("panel does not contain registered BasePath: %s", html)
	}
	if strings.Contains(html, `fetch("/v0/management/plugins/workbuddy`) {
		t.Fatal("panel still hardcodes the historical management BasePath")
	}
}
