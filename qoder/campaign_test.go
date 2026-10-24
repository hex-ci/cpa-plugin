package main

import (
	"net/http"
	"testing"
)

// The activity endpoints only answer like a desktop client. The header set is
// load-bearing: without UA/cosy-clienttype/cosy-version the list comes back
// empty, and with the full derived cosy-machine* set upstream silently drops
// the claimable campaigns.
func TestDesktopAccountHeadersIdentity(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test"}}
	req, err := http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	desktopAccountHeaders(sa)(req)

	if got := req.Header.Get("User-Agent"); got != "Qoder" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("cosy-clienttype"); got != desktopClientType {
		t.Fatalf("cosy-clienttype = %q, want %q", got, desktopClientType)
	}
	if req.Header.Get("cosy-version") == "" {
		t.Fatal("cosy-version missing")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer dt-test" {
		t.Fatalf("Authorization = %q", got)
	}
	for _, h := range []string{"cosy-machineid", "cosy-machinetoken", "cosy-machinetype", "cosy-machinecode", "cosy-machineos", "cosy-machinehostname"} {
		if v := req.Header.Get(h); v != "" {
			t.Fatalf("%s must not be sent (derived machine headers hide the campaigns), got %q", h, v)
		}
	}
	if req.Header.Get("Content-Type") != "" {
		t.Fatal("GET listing must not carry Content-Type")
	}
}

// Upstream downgrades actionType to VIEW_DETAILS after a claim, so claimability
// has to key off claimStatus.
func TestCampaignClassification(t *testing.T) {
	claimable := campaign{CampaignID: "c1", ClaimStatus: "CLAIMABLE", ActionType: "CLAIM_BENEFIT"}
	claimable.Benefit.Kind = "CREDITS"
	claimable.Benefit.Amount = 100
	if !claimable.claimable() || !claimable.isCredits() || claimable.creditAmount() != 100 {
		t.Fatal("claimable campaign misclassified")
	}

	claimed := campaign{CampaignID: "c1", ClaimStatus: "CLAIMED", ActionType: "VIEW_DETAILS"}
	claimed.Benefit.Kind = "CREDITS"
	claimed.Benefit.Amount = 100
	if claimed.claimable() {
		t.Fatal("claimed campaign must not be claimable")
	}
	if !claimed.claimed() {
		t.Fatal("claimed campaign not detected")
	}

	code := campaign{CampaignID: "c2", ClaimStatus: "CLAIMABLE", ActionType: "CLAIM_BENEFIT"}
	code.Benefit.Kind = "REDEMPTION_CODE"
	code.Benefit.Amount = 1
	if code.isCredits() {
		t.Fatal("redemption-code campaign counted as credits")
	}
}

func TestMergeCampaignStateOverridesLegacyDisabled(t *testing.T) {
	sum := &checkinSummary{Active: false, ActivityName: "每日签到"}
	list := &campaignList{ShowCampaign: true, Claimable: true, Campaigns: []campaign{
		{CampaignID: "c1", CampaignKey: "act-1", ClaimStatus: "CLAIMABLE", ActionType: "CLAIM_BENEFIT",
			Benefit: struct {
				Kind   string  `json:"kind"`
				Amount float64 `json:"amount"`
			}{Kind: "CREDITS", Amount: 100}},
	}}
	mergeCampaignState(sum, list, nil)
	if !sum.Active {
		t.Fatal("campaign must make the summary active")
	}
	if sum.DailyCredit != 100 || sum.TodayCheckedIn {
		t.Fatalf("summary = %+v", sum)
	}

	claimedList := &campaignList{Campaigns: []campaign{
		{CampaignID: "c1", CampaignKey: "act-1", ClaimStatus: "CLAIMED", ActionType: "VIEW_DETAILS",
			Benefit: struct {
				Kind   string  `json:"kind"`
				Amount float64 `json:"amount"`
			}{Kind: "CREDITS", Amount: 100}},
	}}
	mergeCampaignState(sum, claimedList, nil)
	if !sum.TodayCheckedIn || sum.TodayCredit != 100 {
		t.Fatalf("claimed state not reflected: %+v", sum)
	}
}

// A list with no credits campaigns must leave the legacy summary untouched
// rather than claiming an activity exists.
func TestMergeCampaignStateIgnoresEmptyList(t *testing.T) {
	sum := &checkinSummary{Active: false}
	mergeCampaignState(sum, &campaignList{ShowCampaign: false}, nil)
	if sum.Active {
		t.Fatal("empty campaign list must not activate the summary")
	}
}

// A granted claim must win over a list that still reports CLAIMABLE: upstream
// keeps serving the pre-claim state for the rest of the day, and without the
// local marker the panel re-offers "签到" for credits already collected.
func TestClaimedTodayWinsOverStaleCampaignList(t *testing.T) {
	sa := testAuth(realmCN, "uid-claimed")
	markClaimedToday(sa)
	t.Cleanup(func() {
		claimedTodayMu.Lock()
		delete(claimedTodayByUser, "uid-claimed")
		claimedTodayMu.Unlock()
	})
	if !claimedToday(sa) {
		t.Fatal("marker not recorded")
	}

	sum := &checkinSummary{}
	list := &campaignList{Claimable: true, Campaigns: []campaign{{
		CampaignID:  "c1",
		CampaignKey: "daily-100",
		ClaimStatus: "CLAIMABLE",
		ActionType:  "CLAIM_BENEFIT",
		Benefit: struct {
			Kind   string  `json:"kind"`
			Amount float64 `json:"amount"`
		}{Kind: "CREDITS", Amount: 100},
	}}}
	mergeCampaignState(sum, list, sa)
	if !sum.TodayCheckedIn {
		t.Fatal("claimed marker must override the stale CLAIMABLE list")
	}
	if sum.TodayCredit != 100 {
		t.Fatalf("today credit = %d, want 100", sum.TodayCredit)
	}

	// Another account is unaffected.
	other := testAuth(realmCN, "uid-other")
	if claimedToday(other) {
		t.Fatal("marker leaked across accounts")
	}
}

// Upstream withdraws the activity once today's reward is gone, so an empty
// campaign list plus a local grant is the normal "already claimed" shape.
func TestClaimedMarkerWithEmptyCampaignList(t *testing.T) {
	sa := testAuth(realmCN, "uid-empty-list")
	markClaimedToday(sa)
	t.Cleanup(func() {
		claimedTodayMu.Lock()
		delete(claimedTodayByUser, "uid-empty-list")
		claimedTodayMu.Unlock()
	})

	sum := &checkinSummary{DailyCredit: 100}
	mergeCampaignState(sum, &campaignList{}, sa)
	if !sum.TodayCheckedIn {
		t.Fatal("empty list must not hide a granted claim")
	}
	if sum.TodayCredit != 100 {
		t.Fatalf("today credit = %d, want 100", sum.TodayCredit)
	}

	// Without the marker the empty list means "nothing to claim".
	other := &checkinSummary{DailyCredit: 100}
	mergeCampaignState(other, &campaignList{}, testAuth(realmCN, "uid-no-marker"))
	if other.TodayCheckedIn {
		t.Fatal("no marker, no claim: must stay unchecked")
	}
}
