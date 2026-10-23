// usage_config.go applies plugin config on register/reconfigure and resolves
// the CPAMP usage-report endpoint.
//
// The strict YAML parsing lives in config.go; this file only commits the parsed
// snapshot and runs the side effects that depend on it.
package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// checkin schedule: 09:00 and 21:00 local time.
// checkinHours is when the auto check-in runs. The activity platform resets the
// daily reward at 10:00 (UTC+8), so the morning tick has to be after that.
var checkinHours = []int{10, 21}

var (
	// checkinAuto can also be flipped at runtime from the panel
	// (POST /checkin/config). configure() resets it to the configured value, so
	// the panel writes the host config as well — a runtime-only flip would be
	// silently reverted by the next reconfigure.
	checkinAuto   = true
	checkinAutoMu sync.RWMutex

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import. Empty URL or key disables reporting
	// entirely — a keyless import is rejected anyway, and probing candidate
	// endpoints fires rejected management calls that ban the host's IP.
	usageReportURL = ""
	usageReportKey = ""
	usageReportMu  sync.RWMutex

	// managementAPIKey gates /v0/management/plugins/qoder/* when set. Empty
	// keeps the historical behaviour: the host's management middleware is the
	// only gate.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex
)

func checkinAutoEnabled() bool {
	checkinAutoMu.RLock()
	defer checkinAutoMu.RUnlock()
	return checkinAuto
}

// configure parses config_yaml and commits it. A parse or validation error is
// returned to the host, which then refuses the registration: the plugin must
// not run with a half-applied configuration.
func configure(raw []byte) error {
	next, err := loadPluginConfig(extractConfigYAML(raw))
	if err != nil {
		return err
	}
	// Compile the block list before publishing the config: a bad term list must
	// fail configure rather than silently disable the feature.
	dz, err := buildDesensitizeConfig(next.Desensitize, next.DesensitizeTerms)
	if err != nil {
		return err
	}
	desensitizeRuntime.Store(dz)

	next.UsageReportURL, next.UsageReportKey = resolveUsageReport(next.UsageReportURL, next.UsageReportKey)
	if strings.TrimSpace(next.ManagementKey) == "" {
		next.ManagementKey = firstNonEmpty(
			strings.TrimSpace(os.Getenv("QODER_MANAGEMENT_KEY")),
			strings.TrimSpace(os.Getenv("WB_MANAGEMENT_KEY")), // pre-rename env name
		)
	}

	cfgMu.Lock()
	cfg = next
	cfgMu.Unlock()

	checkinAutoMu.Lock()
	checkinAuto = next.CheckinAuto
	checkinAutoMu.Unlock()

	managementAPIKeyMu.Lock()
	managementAPIKey = next.ManagementKey
	managementAPIKeyMu.Unlock()

	usageReportMu.Lock()
	usageReportURL = next.UsageReportURL
	usageReportKey = next.UsageReportKey
	usageReportMu.Unlock()

	ensureScheduler()
	migrateLegacyAuthFiles()
	return nil
}

// extractConfigYAML pulls the raw document out of the register/reconfigure
// request. A request without config_yaml means "no configuration", which
// resolves to defaults.
func extractConfigYAML(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil
	}
	return req.ConfigYAML
}

// resolveUsageReport fills usageReportURL/key from config → env → secret files.
// An unauthenticated import is rejected downstream, so both halves must be
// present or reporting stays off.
func resolveUsageReport(cfgURL, cfgKey string) (string, string) {
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
		readSecretFile("/run/secrets/cpamp_admin_key"),
		readSecretFile("/run/secrets/cpamp-admin-key"),
	)
	if url == "" || key == "" {
		return "", ""
	}
	return url, key
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
