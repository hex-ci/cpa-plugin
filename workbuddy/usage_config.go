// usage_config.go decodes plugin config from config_yaml on every
// register/reconfigure call and resolves the CPAMP usage report URL/key.
// All plugin-level config lives here so the rest of the plugin reads
// consistent, lock-protected snapshots.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// check-in schedule: 09:00 and 21:00 local time.
var checkinHours = []int{9, 21}

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	checkinAuto   = true // enabled by default
	checkinAutoMu sync.RWMutex

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import (only path that reaches request monitoring;
	// c-shared plugins cannot use host usage.DefaultManager/redisqueue).
	//
	// Resolution order (community-style, like codex-auth-importer env injection):
	//  1) plugins.configs.workbuddy.usage_report_* in config.yaml
	//  2) env USAGE_REPORT_URL / USAGE_REPORT_KEY / CPAMP_ADMIN_KEY
	//  3) secret files (docker secrets / bind-mount), e.g. /run/secrets/cpamp_admin_key
	// Default URL targets the compose service name of CPA-Manager-Plus.
	usageReportURL = defaultUsageReportURL
	usageReportKey = ""
	usageReportMu  sync.RWMutex

	// managementAPIKey: plugin-layer auth for /v0/management/plugins/workbuddy/*
	// write endpoints. When empty, plugin relies on host-side auth (CPA's
	// management middleware) — that's the historical default and stays
	// backward-compatible. When set via config_yaml management_key: or env
	// WB_MANAGEMENT_KEY, handleManagement enforces constant-time Bearer match
	// plus per-IP token-bucket rate limiting on mutating endpoints.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex
)

// Default URL tries localhost first (works for both bare-metal and Docker
// host-network), falls back to Docker compose service name. The probe runs
// once at configure() time; a reachable endpoint wins.
//
// For users who run CPA Manager Plus on a different host/port, set
// usage_report_url in plugin config or env USAGE_REPORT_URL.
const defaultUsageReportURL = "http://127.0.0.1:18317/v0/management/usage/import"

const fallbackUsageReportURL = "http://cpa-manager-plus:18317/v0/management/usage/import"

// configure decodes plugin config from the lifecycle request.
func configure(raw []byte) error {
	// Parse config without holding any lock (fixes nested-lock hazard).
	nextCheckinAuto := true
	nextLifecycleAuto := true
	nextSchedulerMode := schedulerModeOff // reset to default on reconfigure
	nextKeepaliveAuto := true
	nextMgmtKey := ""
	nextProxyURL := ""

	cfgURL, cfgKey := "", ""
	var configYAML []byte
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
			return errors.New("invalid plugin configuration")
		}
		configYAML = req.ConfigYAML
	}

	configScalars, err := parseTopLevelConfigScalars(configYAML)
	if err != nil {
		proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
		return err
	}
	if value, ok := configScalars["checkin_auto"]; ok {
		nextCheckinAuto = enabledConfigValue(value)
	}
	if value, ok := configScalars["lifecycle_auto"]; ok {
		nextLifecycleAuto = enabledConfigValue(value)
	}
	if configScalars["scheduler_mode"] == schedulerModeCredits {
		nextSchedulerMode = schedulerModeCredits
	}
	cfgURL = configScalars["usage_report_url"]
	cfgKey = configScalars["usage_report_key"]
	nextMgmtKey = configScalars["management_key"]
	if value, ok := configScalars["token_keepalive"]; ok {
		nextKeepaliveAuto = enabledConfigValue(value)
	}

	nextProxyURL, err = parseProxyURLConfig(configYAML)
	if err != nil {
		proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
		return err
	}
	nextFeatures, err := parseFeatureRuntime(configYAML)
	if err != nil {
		return err
	}
	if err := configureProxy(nextProxyURL); err != nil {
		return err
	}

	// Apply each setting under its own lock — no nesting.
	checkinAutoMu.Lock()
	checkinAuto = nextCheckinAuto
	checkinAutoMu.Unlock()

	lifecycleAutoMu.Lock()
	lifecycleAuto = nextLifecycleAuto
	lifecycleAutoMu.Unlock()

	schedulerModeMu.Lock()
	schedulerMode = nextSchedulerMode
	schedulerModeMu.Unlock()

	keepaliveAutoMu.Lock()
	keepaliveAuto = nextKeepaliveAuto
	keepaliveAutoMu.Unlock()

	// management key: config_yaml > env > keep existing. Empty stays empty
	// (plugin-layer auth disabled, host middleware still guards).
	if nextMgmtKey == "" {
		nextMgmtKey = strings.TrimSpace(os.Getenv("WB_MANAGEMENT_KEY"))
	}
	managementAPIKeyMu.Lock()
	managementAPIKey = nextMgmtKey
	managementAPIKeyMu.Unlock()

	resolveUsageReport(cfgURL, cfgKey)
	ensureScheduler()
	currentModelRuntime().commitFeatureRuntime(nextFeatures)
	return nil
}

func parseValidatedConfigRoot(raw []byte) (*yaml.Node, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("invalid config_yaml")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("config_yaml must contain exactly one document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config_yaml must be a mapping")
	}
	root := document.Content[0]
	if err := validateConfigYAMLNode(root, strings.Split(string(raw), "\n")); err != nil {
		return nil, err
	}
	return root, nil
}

func validateConfigYAMLNode(node *yaml.Node, lines []string) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" {
		return errors.New("config_yaml must not use anchors or aliases")
	}
	if node.Style&yaml.TaggedStyle != 0 || nodeStartsWithNonSpecificTag(node, lines) {
		return errors.New("config_yaml must not use explicit tags")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errors.New("config_yaml mapping keys must be strings")
			}
			if key.Value == "<<" || key.Tag == "!!merge" {
				return errors.New("config_yaml must not use merge keys")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return errors.New("config_yaml must not contain duplicate keys")
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := validateConfigYAMLNode(child, lines); err != nil {
			return err
		}
	}
	return nil
}

func nodeStartsWithNonSpecificTag(node *yaml.Node, lines []string) bool {
	if node.Line < 1 || node.Line > len(lines) || node.Column < 1 {
		return false
	}
	line := []rune(strings.TrimSuffix(lines[node.Line-1], "\r"))
	column := node.Column - 1
	if column >= len(line) || line[column] != '!' {
		return false
	}
	return column+1 == len(line) || line[column+1] == ' ' || line[column+1] == '\t'
}

func parseTopLevelConfigScalars(raw []byte) (map[string]string, error) {
	root, err := parseValidatedConfigRoot(raw)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, nil
	}
	values := make(map[string]string, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}

		expected := ""
		switch key.Value {
		case "checkin_auto", "lifecycle_auto", "token_keepalive":
			expected = "boolean"
		case "scheduler_mode", "usage_report_url", "usage_report_key", "management_key":
			expected = "string"
		default:
			continue
		}
		if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			continue
		}
		if value.Kind != yaml.ScalarNode {
			return nil, errors.New(key.Value + " must be a scalar " + expected)
		}
		if expected == "boolean" {
			if value.Tag != "!!bool" && value.Tag != "!!int" && value.Tag != "!!str" {
				return nil, errors.New(key.Value + " must be a boolean")
			}
		} else if value.Tag != "!!str" {
			return nil, errors.New(key.Value + " must be a string")
		}
		values[key.Value] = strings.TrimSpace(value.Value)
	}
	return values, nil
}

func enabledConfigValue(value string) bool {
	value = strings.ToLower(value)
	return value == "true" || value == "1" || value == "yes" || value == "on"
}

func parseProxyURLConfig(raw []byte) (string, error) {
	root, err := parseValidatedConfigRoot(raw)
	if err != nil {
		return "", err
	}
	if root == nil {
		return "", nil
	}
	value := ""
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "proxy-url" {
			continue
		}
		node := root.Content[i+1]
		if node.Kind != yaml.ScalarNode {
			return "", errors.New("proxy-url must be a string")
		}
		if node.Tag == "!!null" {
			value = ""
			continue
		}
		if node.Tag != "!!str" {
			return "", errors.New("proxy-url must be a string")
		}
		value = node.Value
	}
	return value, nil
}

// resolveUsageReport fills usageReportURL/key from config → env → secret files.
// Mirrors community plugins that inject management keys via env/build (e.g.
// codex-auth-importer CODEX_AUTH_IMPORTER_MANAGEMENT_KEY), not plaintext CPA
// remote-management.secret-key (that field is bcrypt-hashed).
func resolveUsageReport(cfgURL, cfgKey string) {
	url := firstNonEmpty(
		strings.TrimSpace(cfgURL),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_URL")),
		strings.TrimSpace(os.Getenv("CPAMP_USAGE_IMPORT_URL")),
	)
	if url == "" {
		url = probeUsageReportURL()
	}
	key := firstNonEmpty(
		strings.TrimSpace(cfgKey),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_KEY")),
		strings.TrimSpace(os.Getenv("CPAMP_ADMIN_KEY")),
		strings.TrimSpace(os.Getenv("CPA_MANAGER_ADMIN_KEY")),
		readSecretFile(os.Getenv("USAGE_REPORT_KEY_FILE")),
		readSecretFile(os.Getenv("CPAMP_ADMIN_KEY_FILE")),
		readSecretFile(os.Getenv("CPA_MANAGER_ADMIN_KEY_FILE")),
		// docker compose secrets default path
		readSecretFile("/run/secrets/cpamp_admin_key"),
		readSecretFile("/run/secrets/cpamp-admin-key"),
		// optional bind-mounts used on this host
		readSecretFile("/CLIProxyAPI/secrets/cpamp-admin-key"),
		readSecretFile("/CLIProxyAPI/secrets/cpamp_admin_key"),
	)
	usageReportMu.Lock()
	usageReportURL = url
	usageReportKey = key
	usageReportMu.Unlock()
}

// probeUsageReportURL tries localhost first (bare-metal + Docker host-network),
// then Docker compose service name. Returns whichever responds; defaults to
// localhost if both fail (better to try localhost than an unreachable hostname).
func probeUsageReportURL() string {
	for _, candidate := range []string{defaultUsageReportURL, fallbackUsageReportURL} {
		if probeURL(candidate, 2*time.Second) {
			return candidate
		}
	}
	return defaultUsageReportURL
}

// probeURL does a quick HEAD/GET to check if the endpoint is reachable.
func probeURL(target string, timeout time.Duration) bool {
	state := currentProxyState()
	if state.mode == proxyModeBlocked || state.mode == proxyModeExplicit && state.client == nil {
		return false
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: rejectHTTPRedirect}
	if state.mode == proxyModeExplicit {
		client.Transport = state.client.Transport
	}
	resp, err := client.Get(target)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// A non-redirect HTTP response means the endpoint itself is reachable.
	return resp.StatusCode > 0 && (resp.StatusCode < http.StatusMultipleChoices || resp.StatusCode >= http.StatusBadRequest)
}

func readSecretFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
