//go:build live

// Opt-in live probe for the read-only account surfaces (plan + credits) against
// one stored credential. Never runs in CI or `make test`.
//
//	QODER_LIVE_AUTH=/path/qoder-<uid>.json \
//	  go test -tags live -run TestLiveAccountReads -v .

package main

import (
	"net/http"
	"os"
	"testing"
)

func TestLiveAccountReads(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := realmOf(sa)
	t.Logf("realm=%s plan_url=%s", r.ID, r.planURL())

	// Raw probe of the plan call: fetchPaymentType swallows errors, so probe
	// the transport directly to see status and body shape.
	if req, rerr := http.NewRequest(http.MethodGet, r.planURL(), nil); rerr == nil {
		billingHeaders(req, sa)
		if resp, herr := hostHTTPDo(req); herr != nil {
			t.Logf("plan raw: transport error %v", herr)
		} else {
			body := string(resp.Body)
			if len(body) > 300 {
				body = body[:300]
			}
			t.Logf("plan raw: status=%d body=%s", resp.StatusCode, body)
		}
	} else {
		t.Logf("plan raw: request build error %v", rerr)
	}

	if p, perr := fetchPaymentType(sa); true {
		t.Logf("fetchPaymentType direct = %q err=%v", p, perr)
	}
	plan, ci, cr, errs := cachedAccountDetails("live-probe", sa, true)
	t.Logf("plan=%q errs=%v", plan, errs)
	if ci != nil {
		t.Logf("checkin active=%t today=%t", ci.Active, ci.TodayCheckedIn)
	}
	if cr != nil {
		t.Logf("credits remain=%d used=%d size=%d packs=%d exhausted=%t",
			cr.TotalRemain, cr.TotalUsed, cr.TotalSize, cr.PackCount, isCreditsExhausted(cr))
	}
}

// TestLiveWarmCatalogs verifies the panel's status strip source can be filled
// from a live fetch (the static fallback must not count as warm).
func TestLiveWarmCatalogs(t *testing.T) {
	path := os.Getenv("QODER_LIVE_AUTH")
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, _ := os.ReadFile(path)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	catalogCache.Delete(realmOf(sa).ID)
	warmCatalogs([]*storedAuth{sa})
	entries, live := catalogStateFor(sa)
	t.Logf("realm=%s entries=%d live=%t", realmOf(sa).ID, len(entries), live)
	if !live {
		t.Fatal("catalog still reports the fallback snapshot after warming")
	}
}
