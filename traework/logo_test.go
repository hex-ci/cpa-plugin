package main

import (
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The logo must live in this repository: pointing at an upstream CDN means the
// icon disappears the day the vendor moves it.
func TestPluginLogoIsServedFromThisRepo(t *testing.T) {
	const prefix = "https://raw.githubusercontent.com/hex-ci/cpa-plugin/main/traework/assets/"
	if !strings.HasPrefix(pluginLogoURL, prefix) {
		t.Fatalf("pluginLogoURL = %q, want it under %q", pluginLogoURL, prefix)
	}
	if host := strings.TrimPrefix(pluginLogoURL, prefix); strings.Contains(host, "/") {
		t.Fatalf("pluginLogoURL %q escapes the plugin's assets directory", pluginLogoURL)
	}
	local := "assets/" + strings.TrimPrefix(pluginLogoURL, prefix)
	info, err := os.Stat(local)
	if err != nil {
		t.Fatalf("logo asset %s is missing: %v", local, err)
	}
	if info.Size() == 0 {
		t.Fatalf("logo asset %s is empty", local)
	}
}

func TestRegistrationAdvertisesOnlyImplementedCapabilities(t *testing.T) {
	reg := twRegistration()
	if !reg.Capabilities.AuthProvider {
		t.Error("auth_provider must be advertised: login and refresh are implemented")
	}
	if !reg.Capabilities.ManagementAPI {
		t.Error("management_api must be advertised: the panel and accounts route are implemented")
	}
	if !reg.Capabilities.ModelProvider {
		t.Error("model_provider must be advertised: the catalogue is served per account")
	}
	if !reg.Capabilities.Executor {
		t.Error("executor must be advertised: the solo channel serves chat")
	}
	if reg.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Errorf("executor model scope = %q, want OAuth", reg.Capabilities.ExecutorModelScope)
	}
	if len(reg.Capabilities.ExecutorInputFormats) != 1 || reg.Capabilities.ExecutorInputFormats[0] != "chat-completions" {
		t.Errorf("executor input formats = %v", reg.Capabilities.ExecutorInputFormats)
	}
	// Unimplemented capabilities must stay off so the host does not call into
	// missing methods.
	if reg.Capabilities.Scheduler || reg.Capabilities.UsagePlugin || reg.Capabilities.FrontendAuthProvider {
		t.Errorf("unimplemented capabilities advertised: %+v", reg.Capabilities)
	}
	if reg.Metadata.Name != displayName || reg.Metadata.Logo != pluginLogoURL {
		t.Errorf("metadata = %+v, want name %q with the in-repo logo", reg.Metadata, displayName)
	}
}

func TestAuthFileNamesCarryTheProviderPrefix(t *testing.T) {
	if !isTraeworkAuthListName(authFileName) {
		t.Errorf("%s must be recognised as an auth file", authFileName)
	}
	if !isTraeworkAuthListName("traework-123.json") {
		t.Error("per-account auth files must be recognised")
	}
	if isTraeworkAuthListName("workbuddy-123.json") {
		t.Error("another provider's auth file must not be claimed")
	}
}
