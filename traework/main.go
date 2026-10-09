// main.go wires the plugin into the host: RPC dispatch, capability
// registration and plugin configuration.
package main

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "0.1.1"

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	FrontendAuthProvider  bool                         `json:"frontend_auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	Scheduler             bool                         `json:"scheduler"`
	ManagementAPI         bool                         `json:"management_api"`
	UsagePlugin           bool                         `json:"usage_plugin"`
}

// The host injects its management/resource base paths at registration so the
// panel can call the right endpoints regardless of the host's routing layout.
var (
	managementKey         atomic.Value // string
	panelBaseURL          atomic.Value // string
	loginCallbackOverride atomic.Value // string
	managementBasePath    atomic.Value // string
	resourceBasePath      atomic.Value // string
)

func loadString(v atomic.Value) string {
	s, _ := v.Load().(string)
	return s
}

func loadedManagementBasePath() string {
	if path := loadString(managementBasePath); path != "" {
		return path
	}
	return defaultManagementBasePath
}

func loadedResourceBasePath() string {
	if path := loadString(resourceBasePath); path != "" {
		return path
	}
	return defaultResourceBasePath
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(twRegistration()), nil
	case pluginabi.MethodModelStatic:
		return handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return handleModelForAuth(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName}), nil
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName}), nil
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return handleExecCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return handleExecHTTPRequest(request)
	case pluginabi.MethodManagementRegister:
		var regReq pluginapi.ManagementRegistrationRequest
		if err := json.Unmarshal(request, &regReq); err == nil {
			if regReq.BasePath != "" {
				managementBasePath.Store(regReq.BasePath)
			}
			if regReq.ResourceBasePath != "" {
				resourceBasePath.Store(regReq.ResourceBasePath)
			}
		}
		return okEnvelope(managementRegistration()), nil
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func twRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             displayName,
			Version:          version,
			Author:           "Hex",
			GitHubRepository: "https://github.com/hex-ci/cpa-plugin",
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "Bearer key enforced by TraeWork for mutating management endpoints; also env TW_MANAGEMENT_KEY."},
				{Name: "proxy-url", Type: pluginapi.ConfigFieldTypeString, Description: "Optional plugin-level proxy for all TraeWork HTTP traffic. Supports http, https, socks5, socks5h; empty inherits the host routing. Invalid settings fail closed."},
				{Name: "checkin_auto", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Claim the daily TraeWork bonus automatically at 09:00 and 21:00 local time (the evening slot retries a busy morning). Manual claims stay available in the panel either way; default true."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
			ManagementAPI:         true,
		},
	}
}

// configure applies the plugin configuration pushed with register/reconfigure.
// Callers must not hold locks here; the host calls it on its own goroutine.
func configure(raw []byte) error {
	nextMgmtKey := ""
	nextProxyURL := ""
	nextPanelBase := ""
	nextCallbackURL := ""
	nextCheckinAuto := true
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
			return errors.New("invalid plugin configuration")
		}
		scalars, err := parseTopLevelConfigScalars(req.ConfigYAML)
		if err != nil {
			proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
			return err
		}
		nextMgmtKey = strings.TrimSpace(scalars["management_key"])
		nextProxyURL = strings.TrimSpace(scalars["proxy-url"])
		if nextMgmtKey == "" {
			nextMgmtKey = strings.TrimSpace(scalars["management-key"])
		}
		nextPanelBase = strings.TrimSpace(scalars["panel_base_url"])
		if nextPanelBase == "" {
			nextPanelBase = strings.TrimSpace(scalars["panel-base-url"])
		}
		nextCallbackURL = strings.TrimSpace(scalars["login_callback_url"])
		if nextCallbackURL == "" {
			nextCallbackURL = strings.TrimSpace(scalars["login-callback-url"])
		}
		if nextCallbackURL != "" && !strings.HasPrefix(nextCallbackURL, "http://") && !strings.HasPrefix(nextCallbackURL, "https://") {
			proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
			return errors.New("login_callback_url must be an http(s) URL")
		}
		checkinRaw := strings.TrimSpace(scalars["checkin_auto"])
		if checkinRaw == "" {
			checkinRaw = strings.TrimSpace(scalars["checkin-auto"])
		}
		if checkinRaw != "" {
			nextCheckinAuto = checkinRaw == "true"
		}
	}
	managementKey.Store(nextMgmtKey)
	panelBaseURL.Store(nextPanelBase)
	loginCallbackOverride.Store(nextCallbackURL)
	setCheckinAuto(nextCheckinAuto)
	// configure() runs on every register/reconfigure; the scheduler arms once.
	ensureCheckinScheduler()
	return configureProxy(nextProxyURL)
}
