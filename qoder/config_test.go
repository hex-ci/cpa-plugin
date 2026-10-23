package main

import (
	"strings"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	c, err := loadPluginConfig(nil)
	if err != nil {
		t.Fatalf("loadPluginConfig(nil): %v", err)
	}
	if c.CosyVersion != defaultCosyVersion {
		t.Fatalf("cosy_version = %q, want %q", c.CosyVersion, defaultCosyVersion)
	}
	if c.RealmDefault != realmCN {
		t.Fatalf("realm_default = %q, want cn", c.RealmDefault)
	}
	if !c.CheckinAuto || !c.TokenKeepalive || !c.LifecycleAuto || !c.QueueWait || !c.ProClaimAuto {
		t.Fatalf("boolean defaults = %#v, want them all enabled", c)
	}
	if c.ModelListBody != modelListBodyKWM {
		t.Fatalf("model_list_body = %q, want kwm (the CLI shape)", c.ModelListBody)
	}
	if c.QueueMaxWait != 600 {
		t.Fatalf("queue_max_wait = %d, want 600", c.QueueMaxWait)
	}
}

func TestConfigParsesFullDocument(t *testing.T) {
	doc := []byte(`
cosy_version: "1.2.3"
realm_default: global
scheduler_mode: credits
checkin_auto: false
pro_claim_auto: false
token_keepalive: false
lifecycle_auto: false
queue_wait: false
queue_max_wait: 120
cosy_machine_os: x86_64_win32
model_list_body: empty
usage_report_url: http://cpamp.invalid/v0/management/usage/import
usage_report_key: k
management_key: mgmt
store:
  version: 1.0.0
models:
  - id: custom-model
    name: Custom
    context: 64000
    max_tokens: 2048
    reasoning: true
    levels: [low, high]
`)
	c, err := loadPluginConfig(doc)
	if err != nil {
		t.Fatalf("loadPluginConfig: %v", err)
	}
	if c.CosyVersion != "1.2.3" || c.RealmDefault != realmGlobal || c.Scheduler != schedulerModeCredits {
		t.Fatalf("scalars = %#v", c)
	}
	if c.CheckinAuto || c.TokenKeepalive || c.LifecycleAuto || c.QueueWait || c.ProClaimAuto {
		t.Fatalf("booleans = %#v, want them all disabled", c)
	}
	if c.QueueMaxWait != 120 || c.CosyMachineOS != "x86_64_win32" || c.ModelListBody != modelListBodyEmpty {
		t.Fatalf("typed values = %#v", c)
	}
	if len(c.StaticModels) != 1 || c.StaticModels[0].ID != "custom-model" ||
		strings.Join(c.StaticModels[0].ThinkingLevels, ",") != "low,high" {
		t.Fatalf("models = %#v", c.StaticModels)
	}
}

func TestConfigAcceptsYAMLBoolSpellings(t *testing.T) {
	// YAML resolves True/TRUE as booleans; treating the raw text as a string
	// comparison would silently turn them into false.
	for _, spelling := range []string{"true", "True", "TRUE", "yes", "on"} {
		c, err := loadPluginConfig([]byte("checkin_auto: " + spelling))
		if err != nil {
			t.Fatalf("checkin_auto: %s: %v", spelling, err)
		}
		if !c.CheckinAuto {
			t.Fatalf("checkin_auto: %s parsed as false", spelling)
		}
	}
}

func TestConfigRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"bad realm":         "realm_default: mars",
		"bad scheduler":     "scheduler_mode: fastest",
		"bad body mode":     "model_list_body: gzip",
		"zero queue wait":   "queue_max_wait: 0",
		"huge queue wait":   "queue_max_wait: 99999",
		"empty version":     `cosy_version: ""`,
		"spaced version":    `cosy_version: "1.1 .75"`,
		"bad machine os":    `cosy_machine_os: windows`,
		"bool as string":    `checkin_auto: "yes please"`,
		"int as string":     `queue_max_wait: soon`,
		"models not a list": "models: 3",
	}
	for name, doc := range cases {
		if _, err := loadPluginConfig([]byte(doc)); err == nil {
			t.Fatalf("%s: %q was accepted, want a validation error", name, doc)
		}
	}
}

func TestConfigRejectsYAMLHazards(t *testing.T) {
	cases := map[string]string{
		"duplicate key": "checkin_auto: true\ncheckin_auto: false\n",
		"anchor":        "checkin_auto: &a true\nlifecycle_auto: *a\n",
		"merge key":     "base: &b {checkin_auto: true}\n<<: *b\n",
		"two documents": "checkin_auto: true\n---\ncheckin_auto: false\n",
		"not a mapping": "- a\n- b\n",
	}
	for name, doc := range cases {
		if _, err := loadPluginConfig([]byte(doc)); err == nil {
			t.Fatalf("%s: %q was accepted, want a parse error", name, doc)
		}
	}
}

func TestConfigIgnoresHostOwnedKeys(t *testing.T) {
	c, err := loadPluginConfig([]byte("store:\n  version: \"9\"\ninstall:\n  type: direct\n"))
	if err != nil {
		t.Fatalf("host-owned keys must not fail validation: %v", err)
	}
	if c.CosyVersion != defaultCosyVersion {
		t.Fatalf("cosy_version = %q, want the default", c.CosyVersion)
	}
}
