// config.go decodes `plugins.configs.qoder` from config_yaml on every
// register/reconfigure call.
//
// Parsing is strict on purpose: a typo must fail loudly at configure time
// (the plugin then refuses to register and every upstream call fails) instead
// of silently falling back to a default that looks like the setting worked.
// Unknown top-level keys are ignored — the host writes its own `store` block
// into the same map.
package main

import (
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type pluginConfig struct {
	CosyVersion string
	// DesktopVersion is what the account-plane calls advertise; the activity
	// endpoints only answer when asked like the desktop client.
	DesktopVersion string
	// CampaignCheckin claims the daily credits activity (desktop flow). The
	// legacy sash check-in it replaced answers DISABLED for current accounts.
	CampaignCheckin bool
	// Desensitize turns on blocked-term obfuscation (U+200B insertion) for
	// prompt and tool metadata. Off by default.
	Desensitize bool
	// DesensitizeTerms is nil for the built-in list; a non-nil empty slice is
	// an explicitly empty list (obfuscation enabled but nothing blocked).
	DesensitizeTerms *[]string
	RealmDefault     string // cn | global
	Scheduler        string // off | credits

	CheckinAuto     bool
	TokenKeepalive  bool
	LifecycleAuto   bool
	QueueWait       bool
	QueueMaxWait    int // seconds
	CosyMachineOS   string
	ModelListBody   string // kwm | empty
	UsageReportURL  string
	UsageReportKey  string
	ManagementKey   string
	StaticModels    []staticModel
	hasConfigSource bool
}

// staticModel is one entry of the optional `models:` override list.
type staticModel struct {
	ID             string
	Name           string
	ContextLength  int64
	MaxOutput      int64
	Enabled        bool
	Reasoning      bool
	ThinkingLevels []string
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		CosyVersion:     defaultCosyVersion,
		DesktopVersion:  defaultDesktopVersion,
		CampaignCheckin: true,
		RealmDefault:    realmCN,
		Scheduler:       schedulerModeOff,
		CheckinAuto:     true,
		// Token keepalive is on by default: Qoder's dt-/drt- pair goes stale
		// while idle and a stale refresh token needs a fresh login.
		TokenKeepalive: true,
		LifecycleAuto:  true,
		QueueWait:      true,
		QueueMaxWait:   600,
		ModelListBody:  modelListBodyKWM,
	}
}

const (
	modelListBodyKWM   = "kwm"
	modelListBodyEmpty = "empty"
)

// pluginConfigSnapshot holds the live configuration. Readers take the pointer
// under RLock; writers replace the whole value.
var (
	cfgMu sync.RWMutex
	cfg   = defaultPluginConfig()
)

func currentConfig() pluginConfig {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

// cosyVersion returns the cosy-version header/payload value.
func cosyVersion() string {
	v := strings.TrimSpace(currentConfig().CosyVersion)
	if v == "" {
		return defaultCosyVersion
	}
	return v
}

// defaultRealmName returns the region used when an import does not state one.
func defaultRealmName() string { return currentConfig().RealmDefault }

// loadPluginConfig parses and validates one config_yaml document.
func loadPluginConfig(raw []byte) (pluginConfig, error) {
	out := defaultPluginConfig()
	if len(raw) == 0 {
		return out, nil
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return out, fmt.Errorf("config_yaml: %w", err)
	}
	// A second document means the operator pasted two configs together; the
	// host merges only one, so the extra one would be silently ignored.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return out, fmt.Errorf("config_yaml: expected exactly one document")
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return out, nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return out, fmt.Errorf("config_yaml: expected exactly one document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return out, fmt.Errorf("config_yaml: top level must be a mapping")
	}
	if err := rejectUnsupportedYAML(root); err != nil {
		return out, err
	}
	seen := make(map[string]bool, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		val := root.Content[i+1]
		if seen[key] {
			return out, fmt.Errorf("config_yaml: duplicate key %q", key)
		}
		seen[key] = true
		if err := out.apply(key, val); err != nil {
			return out, err
		}
	}
	out.hasConfigSource = true
	if err := out.validate(); err != nil {
		return out, err
	}
	return out, nil
}

// rejectUnsupportedYAML refuses anchors, aliases and merge keys: they make the
// effective configuration depend on evaluation order and hide typos.
func rejectUnsupportedYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.AliasNode:
		return fmt.Errorf("config_yaml: aliases are not allowed")
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "<<" || n.Content[i].Tag == "!!merge" {
				return fmt.Errorf("config_yaml: merge keys are not allowed")
			}
			if err := rejectUnsupportedYAML(n.Content[i+1]); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if err := rejectUnsupportedYAML(c); err != nil {
				return err
			}
		}
	}
	if n.Anchor != "" {
		return fmt.Errorf("config_yaml: anchors are not allowed")
	}
	return nil
}

func (c *pluginConfig) apply(key string, val *yaml.Node) error {
	switch key {
	case "cosy_version":
		return decodeString(val, &c.CosyVersion)
	case "realm_default":
		return decodeString(val, &c.RealmDefault)
	case "scheduler_mode":
		return decodeString(val, &c.Scheduler)
	case "desktop_version":
		return decodeString(val, &c.DesktopVersion)
	case "campaign_checkin":
		return decodeBool(val, &c.CampaignCheckin)
	case "desensitize":
		return decodeBool(val, &c.Desensitize)
	case "desensitize_terms":
		return decodeDesensitizeTerms(val, &c.DesensitizeTerms)
	case "cosy_machine_os":
		return decodeString(val, &c.CosyMachineOS)
	case "model_list_body":
		return decodeString(val, &c.ModelListBody)
	case "usage_report_url":
		return decodeString(val, &c.UsageReportURL)
	case "usage_report_key":
		return decodeString(val, &c.UsageReportKey)
	case "management_key":
		return decodeString(val, &c.ManagementKey)
	case "checkin_auto":
		return decodeBool(val, &c.CheckinAuto)
	case "token_keepalive":
		return decodeBool(val, &c.TokenKeepalive)
	case "lifecycle_auto":
		return decodeBool(val, &c.LifecycleAuto)
	case "queue_wait":
		return decodeBool(val, &c.QueueWait)
	case "queue_max_wait":
		return decodeInt(val, &c.QueueMaxWait)
	case "models":
		return c.decodeStaticModels(val)
	default:
		return nil // host-owned keys (store, install, ...) and forward-compatible keys
	}
}

func decodeString(val *yaml.Node, dst *string) error {
	if val.Kind != yaml.ScalarNode || val.Tag == "!!bool" || val.Tag == "!!int" || val.Tag == "!!float" {
		return fmt.Errorf("config_yaml: %s must be a string", val.Value)
	}
	*dst = strings.TrimSpace(val.Value)
	return nil
}

func decodeBool(val *yaml.Node, dst *bool) error {
	var b bool
	if err := val.Decode(&b); err != nil {
		return fmt.Errorf("config_yaml: %s must be a boolean: %w", val.Value, err)
	}
	*dst = b
	return nil
}

func decodeInt(val *yaml.Node, dst *int) error {
	var n int
	if err := val.Decode(&n); err != nil {
		return fmt.Errorf("config_yaml: %s must be an integer: %w", val.Value, err)
	}
	*dst = n
	return nil
}

func (c *pluginConfig) decodeStaticModels(val *yaml.Node) error {
	if val.Kind != yaml.SequenceNode {
		return fmt.Errorf("config_yaml: models must be a list")
	}
	out := make([]staticModel, 0, len(val.Content))
	for _, item := range val.Content {
		if item.Kind != yaml.MappingNode {
			return fmt.Errorf("config_yaml: models items must be mappings")
		}
		m := staticModel{Enabled: true}
		for i := 0; i+1 < len(item.Content); i += 2 {
			k, v := item.Content[i].Value, item.Content[i+1]
			switch k {
			case "id":
				if err := decodeString(v, &m.ID); err != nil {
					return err
				}
			case "name":
				if err := decodeString(v, &m.Name); err != nil {
					return err
				}
			case "context":
				if err := decodeInt64(v, &m.ContextLength); err != nil {
					return err
				}
			case "max_tokens":
				if err := decodeInt64(v, &m.MaxOutput); err != nil {
					return err
				}
			case "reasoning":
				if err := decodeBool(v, &m.Reasoning); err != nil {
					return err
				}
			case "enabled":
				if err := decodeBool(v, &m.Enabled); err != nil {
					return err
				}
			case "levels":
				if v.Kind != yaml.SequenceNode {
					return fmt.Errorf("config_yaml: models.levels must be a list")
				}
				for _, lv := range v.Content {
					var s string
					if err := decodeString(lv, &s); err != nil {
						return err
					}
					if s != "" {
						m.ThinkingLevels = append(m.ThinkingLevels, s)
					}
				}
			}
		}
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("config_yaml: models items need an id")
		}
		out = append(out, m)
	}
	c.StaticModels = out
	return nil
}

func decodeInt64(val *yaml.Node, dst *int64) error {
	var n int64
	if err := val.Decode(&n); err != nil {
		return fmt.Errorf("config_yaml: %s must be an integer: %w", val.Value, err)
	}
	*dst = n
	return nil
}

func (c *pluginConfig) validate() error {
	if c.CosyVersion == "" {
		return fmt.Errorf("config_yaml: cosy_version must not be empty")
	}
	if len(c.CosyVersion) > 32 || strings.ContainsAny(c.CosyVersion, " \t\n") {
		return fmt.Errorf("config_yaml: cosy_version %q is not a version string", c.CosyVersion)
	}
	switch strings.ToLower(c.RealmDefault) {
	case realmCN, realmGlobal:
		c.RealmDefault = strings.ToLower(c.RealmDefault)
	default:
		return fmt.Errorf("config_yaml: realm_default must be cn or global, got %q", c.RealmDefault)
	}
	switch c.Scheduler {
	case schedulerModeOff, schedulerModeCredits:
	default:
		return fmt.Errorf("config_yaml: scheduler_mode must be off or credits, got %q", c.Scheduler)
	}
	switch c.ModelListBody {
	case modelListBodyKWM, modelListBodyEmpty:
	default:
		return fmt.Errorf("config_yaml: model_list_body must be kwm or empty, got %q", c.ModelListBody)
	}
	if c.QueueMaxWait <= 0 || c.QueueMaxWait > 3600 {
		return fmt.Errorf("config_yaml: queue_max_wait must be within 1..3600 seconds")
	}
	if os := strings.TrimSpace(c.CosyMachineOS); os != "" {
		if !strings.Contains(os, "_") || strings.ContainsAny(os, " \t") {
			return fmt.Errorf("config_yaml: cosy_machine_os must look like {arch}_{platform}, got %q", os)
		}
		c.CosyMachineOS = os
	}
	return nil
}
