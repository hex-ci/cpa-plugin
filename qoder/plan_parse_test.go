package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// feature_allowed carries limits (wiki_file_count: 100000) next to booleans.
// A typed map[string]bool fails the whole decode and silently blanks the plan
// badge on a perfectly healthy account.
func TestPlanResponseParsesMixedFeatureAllowed(t *testing.T) {
	body := `{"user_type":"teams","plan_tier_name":"Teams","is_paid_plan":true,` +
		`"organization":{"org_id":"o1","org_name":"Org"},"feature_allowed":{"wiki":true,"wiki_file_count":100000},` +
		`"start_date":1792745470000,"end_date":1795423870000}`
	var p planResponse
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.PlanTierName != "Teams" {
		t.Fatalf("plan_tier_name = %q", p.PlanTierName)
	}
	if p.StartDate != 1792745470000 {
		t.Fatalf("start_date = %d", p.StartDate)
	}
}

// The note is what the host shows next to the auth file, so the region has to
// follow the credential rather than a compile-time default.
func TestDisplayNoteUsesCredentialRealm(t *testing.T) {
	cr := &creditsSummary{TotalRemain: 2980, TotalUsed: 20, TotalSize: 3000}
	global := &storedAuth{Auth: storedTokens{Domain: "https://qoder.com"}}
	cn := &storedAuth{Auth: storedTokens{Domain: "https://qoder.com.cn"}}

	if note := displayNote(global, cr, false); !strings.HasPrefix(note, "Global") {
		t.Fatalf("global note = %q", note)
	}
	if note := displayNote(cn, cr, false); !strings.HasPrefix(note, "CN") {
		t.Fatalf("cn note = %q", note)
	}
	if note := displayNote(global, nil, true); !strings.HasPrefix(note, "Global") || !strings.Contains(note, "已禁用") {
		t.Fatalf("disabled note = %q", note)
	}
}

// A parse-time refresh has no credits, so it must keep a balance note written by
// displayNote but never adopt a foreign one (which would freeze on the row).
func TestParseKeepsOnlyPluginWrittenNotes(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "at"}, Account: storedAccount{UID: "u1"}}
	keep := toAuthDataOpts(sa, nil, false, map[string]any{"note": "Global · 余2980 已用20 池3000"})
	if got := keep.Metadata["note"]; got != "Global · 余2980 已用20 池3000" {
		t.Fatalf("plugin note dropped: %#v", got)
	}
	replace := toAuthDataOpts(sa, nil, false, map[string]any{"note": "stale note from host"})
	if got := replace.Metadata["note"]; got == "stale note from host" {
		t.Fatalf("foreign note kept: %#v", got)
	}
}
