package main

import (
	"encoding/base64"
	"testing"
)

func configureYAML(t *testing.T, body string) error {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	return configure([]byte(`{"config_yaml":"` + encoded + `"}`))
}

// The callback port is not a setting: the listener takes a temporary loopback
// port, so a config that still carries the old key is simply ignored.
func TestConfigureIgnoresTheRetiredCallbackPort(t *testing.T) {
	if err := configureYAML(t, "login_callback_port: 20444\nmanagement_key: k\n"); err != nil {
		t.Fatalf("a retired key must not break configuration: %v", err)
	}
	if got := loadString(managementKey); got != "k" {
		t.Errorf("management key = %q, want the other keys still applied", got)
	}
}

// checkin_auto is the plugin's only boolean key: it must be applied, and a
// non-boolean value must fail loudly instead of being ignored.
func TestConfigureAppliesCheckinAuto(t *testing.T) {
	t.Cleanup(func() {
		setCheckinAuto(true)
		if err := configureYAML(t, "management_key: k\n"); err != nil {
			t.Fatal(err)
		}
	})

	if err := configureYAML(t, "checkin_auto: false\n"); err != nil {
		t.Fatal(err)
	}
	if checkinAutoEnabled() {
		t.Error("checkin_auto: false must turn the automatic pass off")
	}

	// The hyphenated spelling is accepted too.
	if err := configureYAML(t, "checkin-auto: true\n"); err != nil {
		t.Fatal(err)
	}
	if !checkinAutoEnabled() {
		t.Error("checkin-auto: true must turn it back on")
	}

	// Missing keeps the default (on).
	if err := configureYAML(t, "management_key: k\n"); err != nil {
		t.Fatal(err)
	}
	if !checkinAutoEnabled() {
		t.Error("the default is on")
	}

	if err := configureYAML(t, "checkin_auto: \"yes\"\n"); err == nil {
		t.Error("a quoted string is not a boolean and must be rejected")
	}
}

// The panel's login address is relative by default so a reverse-proxied
// management UI resolves it on its own origin; panel_base_url makes it absolute
// for operators who want a copyable link.
func TestConfigureAppliesPanelBaseURL(t *testing.T) {
	t.Cleanup(func() {
		panelBaseURL.Store("")
		_ = configureYAML(t, "management_key: k\n")
	})

	if err := configureYAML(t, "panel_base_url: https://cpa.example.cn/\n"); err != nil {
		t.Fatal(err)
	}
	if got := loadString(panelBaseURL); got != "https://cpa.example.cn/" {
		t.Errorf("panel_base_url = %q", got)
	}

	// The hyphenated spelling is accepted too.
	if err := configureYAML(t, "panel-base-url: http://cpa.internal:8317\n"); err != nil {
		t.Fatal(err)
	}
	if got := loadString(panelBaseURL); got != "http://cpa.internal:8317" {
		t.Errorf("panel-base-url = %q", got)
	}

	// Absent key: back to the relative default.
	if err := configureYAML(t, "management_key: k\n"); err != nil {
		t.Fatal(err)
	}
	if got := loadString(panelBaseURL); got != "" {
		t.Errorf("panelBaseURL without the key = %q, want empty (relative)", got)
	}
}

// login_callback_url points the authorization page at a public address the
// operator reverse-proxies onto the plugin's listener; the default stays
// loopback.
func TestConfigureAppliesLoginCallbackURL(t *testing.T) {
	t.Cleanup(func() {
		loginCallbackOverride.Store("")
		_ = configureYAML(t, "management_key: k\n")
	})

	if err := configureYAML(t, "login_callback_url: https://cpa.example.cn/authorize\n"); err != nil {
		t.Fatal(err)
	}
	if got := loadString(loginCallbackOverride); got != "https://cpa.example.cn/authorize" {
		t.Errorf("login_callback_url = %q", got)
	}

	if err := configureYAML(t, "login-callback-url: http://cpa.example.cn/authorize\n"); err != nil {
		t.Fatal(err)
	}
	if got := loadString(loginCallbackOverride); got != "http://cpa.example.cn/authorize" {
		t.Errorf("login-callback-url = %q", got)
	}

	if err := configureYAML(t, "login_callback_url: cpa.example.cn/authorize\n"); err == nil {
		t.Error("a value without an http(s) scheme must be refused")
	}

	if err := configureYAML(t, "management_key: k\n"); err != nil {
		t.Fatal(err)
	}
	if got := loadString(loginCallbackOverride); got != "" {
		t.Errorf("loginCallbackOverride without the key = %q, want empty (loopback)", got)
	}
}
