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
	"os"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// check-in schedule: 09:00 and 21:00 local time.
var checkinHours = []int{9, 21}

// growth activity-report schedule: 10:00 local time, after the morning
// check-in so a freshly refilled account reports the same day.
var activityHours = []int{10}

// cat-travel schedule: 09:00 and 21:00 local time, so a trip can be
// departed in the morning and collected the same evening.
var travelHours = []int{9, 21}

// growth task-centre schedule: 10:00 local time for the ordinary tasks, plus
// 01:00 for the night-owl task, which only counts inside its 23:00-08:00 CST
// window.
var growthTasksHours = []int{10, 1}

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	checkinAuto   = true // enabled by default
	checkinAutoMu sync.RWMutex

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import (only path that reaches request monitoring;
	// c-shared plugins cannot use host usage.DefaultManager/redisqueue).
	//
	// Resolution order (env-injection style):
	//  1) plugins.configs.workbuddy.usage_report_* in config.yaml
	//  2) env USAGE_REPORT_URL / USAGE_REPORT_KEY / CPAMP_ADMIN_KEY
	//  3) secret files (docker secrets / bind-mount), e.g. /run/secrets/cpamp_admin_key
	// Default URL targets the compose service name of CPA-Manager-Plus.
	usageReportURL = "" // resolved by resolveUsageReport; empty disables reporting
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

// Growth-system scheduling. Both default to off/1: the activity report drives
// the streak and the first_buddy adoption gate on CN personal accounts, and it
// is only useful together with the cat-travel task, so the operator opts in
// once the scope rules are understood (enterprise and Global accounts are
// skipped, see activity.go).
var (
	activityAuto        = false
	activityReportCount = 5
	activityAutoMu      sync.RWMutex

	travelAuto   = false
	travelAutoMu sync.RWMutex

	growthTasksAuto   = false
	growthTasksAutoMu sync.RWMutex
)

// configure decodes plugin config from the lifecycle request.
func configure(raw []byte) error {
	// Parse config without holding any lock (fixes nested-lock hazard).
	nextCheckinAuto := true
	nextLifecycleAuto := true
	nextSchedulerMode := schedulerModeOff // reset to default on reconfigure
	nextKeepaliveAuto := true
	nextActivityAuto := false
	nextActivityCount := 5
	nextTravelAuto := false
	nextGrowthTasksAuto := false
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
	if value, ok := configScalars["activity_auto"]; ok {
		nextActivityAuto = enabledConfigValue(value)
	}
	if value, ok := configScalars["travel_auto"]; ok {
		nextTravelAuto = enabledConfigValue(value)
	}
	if value, ok := configScalars["growth_tasks_auto"]; ok {
		nextGrowthTasksAuto = enabledConfigValue(value)
	}
	if value, ok := configScalars["activity_report_count"]; ok {
		if n, err := strconv.Atoi(value); err == nil {
			if n < 1 {
				n = 1
			}
			if n > 50 {
				// A runaway event count is both useless (the gate needs five
				// conversations) and a rate-limit risk for the whole account.
				n = 50
			}
			nextActivityCount = n
		} else {
			return errors.New("activity_report_count must be an integer")
		}
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

	activityAutoMu.Lock()
	activityAuto = nextActivityAuto
	activityReportCount = nextActivityCount
	activityAutoMu.Unlock()

	travelAutoMu.Lock()
	travelAuto = nextTravelAuto
	travelAutoMu.Unlock()

	growthTasksAutoMu.Lock()
	growthTasksAuto = nextGrowthTasksAuto
	growthTasksAutoMu.Unlock()

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
		case "checkin_auto", "lifecycle_auto", "token_keepalive", "activity_auto", "travel_auto", "growth_tasks_auto":
			expected = "boolean"
		case "scheduler_mode", "usage_report_url", "usage_report_key", "management_key":
			expected = "string"
		case "activity_report_count":
			expected = "integer"
		default:
			continue
		}
		if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			continue
		}
		if value.Kind != yaml.ScalarNode {
			return nil, errors.New(key.Value + " must be a scalar " + expected)
		}
		switch expected {
		case "boolean":
			if value.Tag != "!!bool" && value.Tag != "!!int" && value.Tag != "!!str" {
				return nil, errors.New(key.Value + " must be a boolean")
			}
		case "integer":
			if value.Tag != "!!int" {
				return nil, errors.New(key.Value + " must be an integer")
			}
		default:
			if value.Tag != "!!str" {
				return nil, errors.New(key.Value + " must be a string")
			}
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
// The env-var route exists because the management key is a plaintext secret the
// host does not hand to plugins; CPA's own remote-management.secret-key field is
// bcrypt-hashed and therefore unusable for signing management calls.
func resolveUsageReport(cfgURL, cfgKey string) {
	url := firstNonEmpty(
		strings.TrimSpace(cfgURL),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_URL")),
		strings.TrimSpace(os.Getenv("CPAMP_USAGE_IMPORT_URL")),
	)
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
	if url == "" || key == "" {
		// Reporting stays off unless both are configured: an unauthenticated
		// import is rejected by the receiving side anyway, and probing candidate
		// ports would fire keyless requests at whatever listens there — on a
		// bare-metal CPA that is the CPA's own management API, where rejected
		// attempts ban the caller's IP after five tries.
		url, key = "", ""
	}
	usageReportMu.Lock()
	usageReportURL = url
	usageReportKey = key
	usageReportMu.Unlock()
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
