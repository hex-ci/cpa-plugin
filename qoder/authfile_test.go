package main

import "testing"

// Auth-file naming is the plugin's ownership boundary: it must claim its own
// files (including the pre-rename qoderwork ones) and nothing else. `qoderwork-`
// does not share the `qoder-` prefix because of the hyphen, so WorkBuddy's and
// the other providers' files can never be mistaken for ours.
func TestAuthFileNameOwnership(t *testing.T) {
	owned := []string{
		"qoder-abc123.json",
		"qoder.json",
		"qoderwork-abc123.json", // legacy, migrated on next save
		"qoderwork.json",        // legacy single-file name
	}
	for _, name := range owned {
		if !isQoderAuthFileName(name) {
			t.Fatalf("%q must be claimed by this plugin", name)
		}
	}
	foreign := []string{
		"workbuddy-abc.json",
		"qwenwork-abc.json",
		"traework-abc.json",
		"qoderx-abc.json",
		"",
	}
	for _, name := range foreign {
		if isQoderAuthFileName(name) {
			t.Fatalf("%q must not be claimed by this plugin", name)
		}
	}
}

func TestLegacyAuthNamesMigrate(t *testing.T) {
	for _, name := range []string{"qoder.json", "qoderwork.json", "qoderwork-uid.json"} {
		if !isLegacyAuthName(name) {
			t.Fatalf("%q should be treated as legacy", name)
		}
	}
	if isLegacyAuthName("qoder-uid.json") {
		t.Fatal("the canonical name must not be treated as legacy")
	}
}

func TestSafeAuthPath(t *testing.T) {
	ok := []string{
		"/var/lib/cpa/auth/qoder-uid.json",
		"/var/lib/cpa/auth/qoder.json",
		"/var/lib/cpa/auth/qoderwork-uid.json",
	}
	for _, p := range ok {
		if !isSafeAuthPath(p) {
			t.Fatalf("%q rejected", p)
		}
	}
	bad := []string{
		"/var/lib/cpa/auth/workbuddy-uid.json",
		"/var/lib/cpa/auth/../etc/passwd",
		"/var/lib/cpa/auth/qoder-uid.txt",
		"",
	}
	for _, p := range bad {
		if isSafeAuthPath(p) {
			t.Fatalf("%q accepted", p)
		}
	}
}
