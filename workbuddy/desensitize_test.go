package main

import (
	"strings"
	"sync"
	"testing"
)

func TestDefaultDesensitizeTermsAreComplete(t *testing.T) {
	want := []string{
		"DoS", "DDoS", "exploit", "credential testing", "credential stuffing",
		"supply chain compromise", "supply-chain compromise", "detection evasion",
		"C2 frameworks", "C2 framework", "command and control", "malicious purposes",
		"malicious intent", "mass targeting", "brute force", "brute-force",
		"privilege escalation", "reverse shell", "remote code execution", "SQL injection",
		"XSS", "CSRF", "phishing", "malware", "ransomware", "keylogger", "rootkit",
		"backdoor", "botnet", "zero-day", "0day", "vulnerability", "vulnerabilities",
		"red teaming", "red-teaming", "sandbox", "sandboxing", "sandboxed", "unsandboxed",
		"escalated privileges", "escalated", "escalation", "destructive action",
		"destructive command", "destructive", "attack", "attacks", "cybersecurity",
		"security review", "exploit development", "hacking", "penetration testing",
		"penetration test", "injection", "weaponize", "weaponized", "harmful", "dangerous",
		"abuse", "abusive", "illegal", "terrorist", "terrorism", "bomb", "weapon",
		"weapons", "drug", "drugs", "narcotic", "suicide", "self-harm", "murder",
		"kill", "violence", "violent", "Claude Code", "Claude Opus", "Claude Sonnet",
		"Claude Haiku", "Claude Fable", "Anthropic", "Co-Authored-By",
		"noreply@anthropic.com", "Codex", "codex",
	}

	cfg := currentFeatureRuntime()
	if cfg == nil {
		t.Fatal("feature runtime is nil")
	}
	if !sameStrings(cfg.desensitizeTerms, want) {
		t.Fatalf("default terms = %#v, want %#v", cfg.desensitizeTerms, want)
	}
	if cfg.desensitizeEnabled {
		t.Fatal("desensitize must default off")
	}
	if cfg.desensitizeSource != "default" {
		t.Fatalf("source = %q, want default", cfg.desensitizeSource)
	}
	if cfg.oauthClientMode != oauthClientModeCLI || cfg.enterpriseCredits {
		t.Fatalf("unsafe defaults: mode=%q enterprise=%v", cfg.oauthClientMode, cfg.enterpriseCredits)
	}
}

func TestParseFeatureRuntimeDistinguishesDefaultCustomEmptyAndNull(t *testing.T) {
	custom, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms:\n  - ' exploit '\n  - exploit\n  - Codex\n  - codex\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !custom.desensitizeEnabled || custom.desensitizeSource != "custom" {
		t.Fatalf("custom config = %#v", custom)
	}
	want := []string{"exploit", "Codex", "codex"}
	if !sameStrings(custom.desensitizeTerms, want) {
		t.Fatalf("custom terms = %#v, want %#v", custom.desensitizeTerms, want)
	}

	empty, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.desensitizeSource != "custom" || len(empty.desensitizeTerms) != 0 {
		t.Fatalf("empty custom terms = %#v", empty)
	}

	for _, raw := range [][]byte{nil, []byte("desensitize_terms: null\n")} {
		defaults, err := parseFeatureRuntime(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(defaults.desensitizeTerms) != 85 || defaults.desensitizeSource != "default" {
			t.Fatalf("defaults = %#v", defaults)
		}
	}
}

func TestParseFeatureRuntimeRejectsUnsafeTermsAndModes(t *testing.T) {
	for _, raw := range []string{
		"desensitize_terms: [x]\n",
		"desensitize_terms: ['a​b']\n",
		"oauth_client_mode: browser\n",
	} {
		if _, err := parseFeatureRuntime([]byte(raw)); err == nil {
			t.Fatalf("parseFeatureRuntime(%q) succeeded", raw)
		}
	}
}

func TestDesensitizeMatcherIsLiteralCaseInsensitiveConvergentAndIdempotent(t *testing.T) {
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [exploit, DDoS, DoS, kill, attack, noreply@anthropic.com, Anthropic, abc, bcd, 'a+b', Codex, codex]\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		in   string
		want string
	}{
		{"DDoS", "D​D​oS"},
		{"EXPLOIT-free skill", "E​XPLOIT-free sk​ill"},
		{"attacker", "a​ttacker"},
		{"noreply@anthropic.com", "n​oreply@a​nthropic.com"},
		{"abcd", "a​b​cd"},
		{"a+b", "a​+b"},
		{"Codex codex", "C​odex c​odex"},
		{"a​ttack", "a​ttack"},
	} {
		got := cfg.matcher.replace(tt.in)
		if got != tt.want {
			t.Errorf("replace(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if again := cfg.matcher.replace(got); again != got {
			t.Errorf("replace is not idempotent: %q -> %q", got, again)
		}
	}
	if strings.Contains(cfg.matcher.replace("DDoS"), "DoS") {
		t.Fatal("nested term still matches after convergence")
	}
}

func TestFeatureRuntimeSnapshotTermsCannotBeMutatedByCallers(t *testing.T) {
	before := currentFeatureRuntime()
	if len(before.desensitizeTerms) == 0 {
		t.Fatal("missing default terms")
	}
	original := before.desensitizeTerms[0]
	before.desensitizeTerms[0] = "mutated"
	if got := currentFeatureRuntime().desensitizeTerms[0]; got != original {
		t.Fatalf("mutating caller snapshot changed runtime term to %q", got)
	}
}

func TestConfigureFeatureRuntimeKeepsPreviousSnapshotOnInvalidTerms(t *testing.T) {
	old := featureRuntime.Load()
	t.Cleanup(func() { featureRuntime.Store(old) })

	if err := configure(mustJSON(map[string]any{
		"config_yaml": []byte("desensitize: true\ndesensitize_terms: [attack]\nusage_report_url: http://127.0.0.1:1\n"),
	})); err != nil {
		t.Fatal(err)
	}
	before := currentFeatureRuntime()
	if !before.desensitizeEnabled || !sameStrings(before.desensitizeTerms, []string{"attack"}) {
		t.Fatalf("configured runtime = %#v", before)
	}

	if err := configure(mustJSON(map[string]any{
		"config_yaml": []byte("desensitize_terms: [x]\nusage_report_url: http://127.0.0.1:1\n"),
	})); err == nil {
		t.Fatal("invalid desensitize terms were accepted")
	}
	after := currentFeatureRuntime()
	if after.desensitizeEnabled != before.desensitizeEnabled || !sameStrings(after.desensitizeTerms, before.desensitizeTerms) {
		t.Fatalf("invalid config replaced runtime: before=%#v after=%#v", before, after)
	}
}

func TestConfigureInvalidProxyFailsClosedEvenWhenFeatureValidationFails(t *testing.T) {
	oldProxy := currentProxyState()
	oldFeatures := featureRuntime.Load()
	t.Cleanup(func() {
		proxyState.Store(oldProxy)
		featureRuntime.Store(oldFeatures)
	})
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})

	err := configure(mustJSON(map[string]any{
		"config_yaml": []byte("desensitize_terms: [x]\nproxy-url: [not-a-string]\n"),
	}))
	if err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	if got := currentProxyState().mode; got != proxyModeBlocked {
		t.Fatalf("proxy mode = %v, want blocked", got)
	}
}

func TestFeatureRuntimeSnapshotsStayIndependentDuringConcurrentReads(t *testing.T) {
	old := featureRuntime.Load()
	t.Cleanup(func() { featureRuntime.Store(old) })

	first, err := parseFeatureRuntime([]byte("desensitize_terms: [attack]\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseFeatureRuntime([]byte("desensitize_terms: [exploit]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(first)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			featureRuntime.Store(second)
			featureRuntime.Store(first)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			cfg := currentFeatureRuntime()
			if len(cfg.desensitizeTerms) != 1 {
				t.Errorf("terms = %#v", cfg.desensitizeTerms)
				return
			}
			cfg.desensitizeTerms[0] = "caller mutation"
		}
	}()
	close(start)
	wg.Wait()

	for _, cfg := range []*featureRuntimeConfig{first, second} {
		if cfg.desensitizeTerms[0] == "caller mutation" {
			t.Fatalf("runtime snapshot was mutated: %#v", cfg)
		}
	}
}
