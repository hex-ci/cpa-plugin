package main

import (
	"errors"
	"sync/atomic"
	"testing"
)

// packFixture is one entitlement package as the endpoint returns it.
func packFixture(name string, limit, used float64, expires int64) entPack {
	var pack entPack
	pack.DisplayDesc = name
	pack.ExpireTime = expires
	pack.EntitlementBaseInfo.Quota.CreditsLimit = limit
	pack.Usage.CreditsAmount = used
	return pack
}

// The real shape: a 500-credit monthly bonus, 14.65 spent, ratio from the
// server. The card needs remain/used/size plus one row per package.
func TestCreditsFromFoldsUsageIntoTheCardShape(t *testing.T) {
	var ent entUsageResponse
	ent.IsCreditsBilling = true
	ent.UsageSummary.TotalAmount = 500
	ent.UsageSummary.ConsumedAmount = 14.6484
	ent.UsageSummary.ConsumptionRatio = 0.0293
	ent.UserEntitlementPackList = []entPack{
		packFixture("免费", 0, 0, 1793462399),
		packFixture("每月登录积分", 500, 14.6484, 1793462399),
	}

	view := creditsFrom(ent, payStatusResponse{UserPayIdentityStr: "Free"})
	if view.TotalSize != 500 || view.TotalUsed != 14.65 || view.TotalRemain != 485.35 {
		t.Fatalf("credits = %+v, want 485.35 剩余 / 14.65 已用 / 500 池", view)
	}
	if view.Billing != "积分计费" {
		t.Errorf("billing = %q, want 积分计费", view.Billing)
	}
	if view.Identity != "Free" {
		t.Errorf("identity = %q, want the pay-status identity", view.Identity)
	}
	if view.PackCount != 2 || len(view.Packs) != 2 {
		t.Fatalf("packs = %+v, want both packages listed", view.Packs)
	}
	if got := view.Packs[1]; got.Name != "每月登录积分" || got.Limit != 500 || got.Used != 14.65 || got.Expires != "2026-10-31" {
		t.Errorf("pack = %+v, want the monthly bonus with its expiry", got)
	}
	if view.FetchedAt == "" {
		t.Error("a snapshot without a timestamp cannot be told apart from a stale one")
	}
}

// Without a reported ceiling the packages are the only source of what is left.
func TestCreditsFromFallsBackToThePackages(t *testing.T) {
	var ent entUsageResponse
	ent.IsDollarUsageBilling = true
	ent.UserEntitlementPackList = []entPack{
		packFixture("每月登录积分", 500, 14.65, 0),
	}

	view := creditsFrom(ent, payStatusResponse{})
	if view.TotalRemain != 485.35 || view.TotalSize != 500 || view.TotalUsed != 14.65 {
		t.Fatalf("credits = %+v, want the packages to supply the pool", view)
	}
	if view.Billing != "美元计费" {
		t.Errorf("billing = %q, want 美元计费", view.Billing)
	}
}

// Hidden packages are noise; an unnamed one still needs a label.
func TestPacksFromSkipsHiddenAndNamesTheRest(t *testing.T) {
	hidden := packFixture("隐藏", 10, 1, 0)
	hidden.IsHide = true
	anon := packFixture("", 0, 0, 0)
	anon.EntitlementBaseInfo.EndTime = 1793462399

	packs := packsFrom([]entPack{hidden, anon})
	if len(packs) != 1 {
		t.Fatalf("packs = %+v, want the hidden one dropped", packs)
	}
	if packs[0].Name != "额度包" {
		t.Errorf("name = %q, want a fallback label", packs[0].Name)
	}
	if packs[0].Expires != "2026-10-31" {
		t.Errorf("expires = %q, want the base-info end time", packs[0].Expires)
	}
}

func TestCheckinFrom(t *testing.T) {
	if got := checkinFrom(checkinStatusResponse{}); got != nil {
		t.Errorf("a disabled, empty response must not produce a check-in row: %+v", got)
	}
	got := checkinFrom(checkinStatusResponse{Enable: true, Credits: 100, ExtraCredits: 100})
	if got == nil || !got.Enabled || got.TodayCheckedIn || got.Credits+got.ExtraCredits != 200 {
		t.Fatalf("checkin = %+v, want 200 available and not taken", got)
	}
	// DidCheckedIn covers the response variant that reports the claim that way.
	if got := checkinFrom(checkinStatusResponse{DidCheckedIn: true}); got == nil || !got.TodayCheckedIn {
		t.Errorf("did_checked_in must count as taken: %+v", got)
	}
}

func TestFormatDayHandlesSecondsAndMilliseconds(t *testing.T) {
	for _, stamp := range []int64{1793462399, 1793462399000} {
		if got := formatDay(stamp); got != "2026-10-31" {
			t.Errorf("formatDay(%d) = %q, want 2026-10-31", stamp, got)
		}
	}
	if got := formatDay(0); got != "" {
		t.Errorf("formatDay(0) = %q, want empty", got)
	}
}

// resetBillingCache keeps the package-level cache from leaking between tests.
func resetBillingCache() {
	billingMu.Lock()
	billingCache = map[string]billingEntry{}
	billingMu.Unlock()
}

// stubFetchCredentials replaces the upstream read with a counter.
func stubFetchCredentials(t *testing.T, credits *creditsView, checkin *checkinView, err error) *int32 {
	t.Helper()
	var calls int32
	previous := fetchCredentialsFn
	fetchCredentialsFn = func(*storedAuth) (*creditsView, *checkinView, error) {
		atomic.AddInt32(&calls, 1)
		return credits, checkin, err
	}
	t.Cleanup(func() { fetchCredentialsFn = previous })
	return &calls
}

func TestCachedCredentialsServesFromCacheAndHonoursFresh(t *testing.T) {
	resetBillingCache()
	want := &creditsView{TotalRemain: 300}
	calls := stubFetchCredentials(t, want, nil, nil)
	sa := &storedAuth{AccessToken: "token"}

	got, _, err := cachedCredentials(sa, "idx-1", false)
	if err != nil || got != want {
		t.Fatalf("first read = %+v (err %v), want the fetched snapshot", got, err)
	}
	if _, _, err := cachedCredentials(sa, "idx-1", false); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("upstream calls = %d, want the second read served from cache", n)
	}

	if _, _, err := cachedCredentials(sa, "idx-1", true); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Errorf("upstream calls = %d, want the refresh to bypass the cache", n)
	}
}

func TestCachedCredentialsKeepsPerAccountEntriesApart(t *testing.T) {
	resetBillingCache()
	first := &creditsView{TotalRemain: 1}
	second := &creditsView{TotalRemain: 2}
	previous := fetchCredentialsFn
	fetchCredentialsFn = func(sa *storedAuth) (*creditsView, *checkinView, error) {
		if sa.UID == "a" {
			return first, nil, nil
		}
		return second, nil, nil
	}
	t.Cleanup(func() { fetchCredentialsFn = previous })

	if got, _, _ := cachedCredentials(&storedAuth{UID: "a"}, "idx-a", false); got != first {
		t.Errorf("idx-a = %+v, want its own snapshot", got)
	}
	if got, _, _ := cachedCredentials(&storedAuth{UID: "b"}, "idx-b", false); got != second {
		t.Errorf("idx-b = %+v, want its own snapshot", got)
	}
}

// Listing accounts must stay one round trip: peek never fetches.
func TestPeekCredentialsDoesNotFetch(t *testing.T) {
	resetBillingCache()
	calls := stubFetchCredentials(t, &creditsView{TotalRemain: 1}, nil, nil)

	if credits, checkin := peekCredentials("idx-1"); credits != nil || checkin != nil {
		t.Fatalf("peek returned %+v / %+v before any fetch", credits, checkin)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("peek triggered %d upstream calls, want none", n)
	}

	if _, _, err := cachedCredentials(&storedAuth{}, "idx-1", false); err != nil {
		t.Fatal(err)
	}
	if credits, _ := peekCredentials("idx-1"); credits == nil || credits.TotalRemain != 1 {
		t.Errorf("peek after a fetch = %+v, want the cached snapshot", credits)
	}
}

func TestCachedCredentialsPropagatesFailure(t *testing.T) {
	resetBillingCache()
	want := errors.New("upstream HTTP 500")
	stubFetchCredentials(t, nil, nil, want)

	if _, _, err := cachedCredentials(&storedAuth{}, "idx-1", false); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the upstream failure", err)
	}
}
