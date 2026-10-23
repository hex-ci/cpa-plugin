package main

import (
	"testing"
)

// Reporting must not fire any request unless an operator configured both a URL
// and a key. The removed default-probe used to GET candidate ports at
// configure() time; on a bare-metal CPA those ports belong to the CPA itself,
// whose management middleware bans the caller's IP after five rejected
// attempts. Gone with the probe: the package no longer even imports net/http.
func TestUsageReportStaysOffWithoutConfiguredURL(t *testing.T) {
	usageReportMu.RLock()
	oldURL, oldKey := usageReportURL, usageReportKey
	usageReportMu.RUnlock()
	t.Cleanup(func() {
		usageReportMu.Lock()
		usageReportURL, usageReportKey = oldURL, oldKey
		usageReportMu.Unlock()
	})

	// Nothing configured: the URL must stay empty instead of resolving to a
	// probed default.
	if url, key := resolveUsageReport("", ""); url != "" || key != "" {
		t.Fatalf("keyless resolve invented url=%q key set=%t, want reporting disabled", url, key != "")
	}

	// A key without a URL is still not enough to invent one.
	if url, _ := resolveUsageReport("", "configured-key"); url != "" {
		t.Fatalf("resolve invented url %q from a key alone", url)
	}

	// A configured URL is taken verbatim: no probing, no rewrite.
	want := "http://cpamp.example.invalid/v0/management/usage/import"
	url, key := resolveUsageReport(want, "configured-key")
	if url != want || key != "configured-key" {
		t.Fatalf("configured resolve = %q/key set=%t, want %q/true", url, key != "", want)
	}
}
