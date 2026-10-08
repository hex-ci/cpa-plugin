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
	resolveUsageReport("", "")
	usageReportMu.RLock()
	url := usageReportURL
	usageReportMu.RUnlock()
	if url != "" {
		t.Fatalf("keyless resolve invented url %q, want reporting disabled", url)
	}

	// A key without a URL is still not enough to invent one.
	resolveUsageReport("", "configured-key")
	usageReportMu.RLock()
	url = usageReportURL
	usageReportMu.RUnlock()
	if url != "" {
		t.Fatalf("resolve invented url %q from a key alone", url)
	}

	// A configured URL is taken verbatim: no probing, no rewrite.
	want := "http://cpamp.example.invalid/v0/management/usage/import"
	resolveUsageReport(want, "configured-key")
	usageReportMu.RLock()
	url, key := usageReportURL, usageReportKey
	usageReportMu.RUnlock()
	if url != want || key != "configured-key" {
		t.Fatalf("configured resolve = %q/key set=%t, want %q/true", url, key != "", want)
	}
}
