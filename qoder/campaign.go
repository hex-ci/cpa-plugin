// campaign.go implements the activity-platform daily check-in — the flow the
// official desktop client uses to hand out the daily credits.
//
// The legacy /sash/api/v1/me/daily-check-in endpoints answer DISABLED for
// current accounts, so the credits arrive as a campaign instead: list
// /sash/api/v1/me/campaigns, then POST .../{campaignId}/claim for every
// CLAIMABLE credits campaign.
//
// These endpoints only behave like a desktop client when asked like one:
//   - the listing needs the desktop identity (UA "Qoder", cosy-clienttype 10,
//     a desktop cosy-version);
//   - it must NOT carry the full derived cosy-machine* header set. Upstream
//     treats that combination as a non-official client and silently drops the
//     claimable campaigns from the list — the answer looks like "no activity
//     today" rather than an error.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// desktopClientType is what the desktop client sends on the account plane.
	// The inference plane uses 5 (CLI); mixing them up changes the response.
	desktopClientType = "10"
	// defaultDesktopVersion is the desktop build whose behaviour these calls
	// mirror. Upstream only uses it for product identification.
	defaultDesktopVersion = "0.4.9"
)

// campaign mirrors one entry of GET /sash/api/v1/me/campaigns.
type campaign struct {
	CampaignID        string `json:"campaignId"`
	CampaignKey       string `json:"campaignKey"`
	ActionType        string `json:"actionType"`
	ClaimStatus       string `json:"claimStatus"`
	UnavailableReason string `json:"unavailableReason"`
	Benefit           struct {
		Kind   string  `json:"kind"`
		Amount float64 `json:"amount"`
	} `json:"benefit"`
}

func (c campaign) creditAmount() int64 { return int64(c.Benefit.Amount) }

// isCredits reports whether the campaign pays credits (as opposed to
// redemption codes or pure detail views).
func (c campaign) isCredits() bool {
	kind := strings.ToUpper(strings.TrimSpace(c.Benefit.Kind))
	return kind == "" || kind == "CREDITS"
}

// claimable reports whether this campaign can be claimed right now.
func (c campaign) claimable() bool {
	if c.CampaignID == "" || strings.ToUpper(c.ClaimStatus) != "CLAIMABLE" {
		return false
	}
	action := strings.ToUpper(strings.TrimSpace(c.ActionType))
	return action == "" || action == "CLAIM_BENEFIT"
}

// claimed reports whether today's reward is already collected. Upstream
// downgrades actionType to VIEW_DETAILS after a claim, so the status field is
// the only reliable signal.
func (c campaign) claimed() bool {
	return strings.ToUpper(c.ClaimStatus) == "CLAIMED"
}

type campaignList struct {
	ShowCampaign bool       `json:"showCampaign"`
	Claimable    bool       `json:"claimable"`
	Campaigns    []campaign `json:"campaigns"`
}

// desktopVersion is configurable so a future client build can be matched
// without a code change.
func desktopVersion() string { return currentConfig().DesktopVersion }

// desktopAccountHeaders builds the header set the desktop client uses on the
// account plane: no Content-Type (the GET has no body), no Origin/Referer, and
// deliberately no cosy-machine* headers — see the file comment.
func desktopAccountHeaders(sa *storedAuth) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Qoder")
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
		req.Header.Set("cosy-clienttype", desktopClientType)
		req.Header.Set("cosy-version", desktopVersion())
	}
}

func campaignsURL(r realmSpec) string {
	return r.Openapi + "/sash/api/v1/me/campaigns"
}

func campaignClaimURL(r realmSpec, campaignID string) string {
	return r.Openapi + "/sash/api/v1/me/campaigns/" + campaignID + "/claim"
}

// fetchCampaigns lists the account's campaigns.
func fetchCampaigns(sa *storedAuth) (*campaignList, error) {
	r := realmOf(sa)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, campaignsURL(r), nil)
	if err != nil {
		return nil, err
	}
	desktopAccountHeaders(sa)(req)

	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, errCampaignsUnsupported
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("campaigns http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 160))
	}
	var out campaignList
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("campaigns parse: %w", err)
	}
	return &out, nil
}

var errCampaignsUnsupported = campaignError("campaigns endpoint unsupported")

type campaignError string

func (e campaignError) Error() string { return string(e) }

// claimOutcome is what POST .../claim tells us.
type claimOutcome struct {
	Status     string
	Replayed   bool
	Failure    string
	Amount     int64
	CampaignID string
}

// claimCampaign claims one campaign. Claiming is idempotent upstream.
func claimCampaign(sa *storedAuth, c campaign) (*claimOutcome, error) {
	r := realmOf(sa)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, campaignClaimURL(r, c.CampaignID), strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	desktopAccountHeaders(sa)(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Status      string `json:"status"`
		Replayed    bool   `json:"replayed"`
		FailureCode string `json:"failureCode"`
		CampaignID  string `json:"campaignId"`
		Benefit     struct {
			Amount float64 `json:"amount"`
		} `json:"benefit"`
	}
	_ = json.Unmarshal(resp.Body, &raw)
	if resp.StatusCode == http.StatusConflict || strings.EqualFold(raw.FailureCode, "ALREADY_CLAIMED") {
		return &claimOutcome{Status: "CLAIMED", Replayed: true, CampaignID: c.CampaignID, Amount: c.creditAmount()}, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("claim http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 160))
	}
	out := &claimOutcome{
		Status:     strings.ToUpper(raw.Status),
		Replayed:   raw.Replayed,
		Failure:    raw.FailureCode,
		CampaignID: c.CampaignID,
		Amount:     c.creditAmount(),
	}
	if out.Amount == 0 {
		out.Amount = int64(raw.Benefit.Amount)
	}
	return out, nil
}

// campaignCheckinResult summarises one account's activity check-in.
type campaignCheckinResult struct {
	Available  bool // an activity exists for this account at all
	Claimable  bool
	Claimed    bool // today's credits are already collected
	Amount     int64
	Earned     int64 // credits actually granted by this run
	Blocked    bool  // same-machine limit: another account already claimed
	Activity   string
	NoCampaign bool // endpoint answered but published nothing for this account
}

// runCampaignCheckin lists the campaigns and claims every claimable credits
// campaign. Claiming stops at the first same-person rejection: the limit is per
// machine, so retrying other campaigns of the same account only burns requests.
func runCampaignCheckin(sa *storedAuth) (*campaignCheckinResult, error) {
	list, err := fetchCampaigns(sa)
	if err != nil {
		return nil, err
	}
	res := &campaignCheckinResult{Available: list.ShowCampaign || len(list.Campaigns) > 0}
	for _, c := range list.Campaigns {
		if !c.isCredits() {
			continue
		}
		res.Available = true
		if res.Activity == "" {
			res.Activity = c.CampaignKey
		}
		if c.creditAmount() > res.Amount {
			res.Amount = c.creditAmount()
		}
		if c.claimed() && c.creditAmount() > 0 {
			res.Claimed = true
		}
	}
	for _, c := range list.Campaigns {
		if !c.isCredits() || !c.claimable() {
			continue
		}
		res.Claimable = true
		out, cerr := claimCampaign(sa, c)
		if cerr != nil {
			return res, cerr
		}
		if out.Failure == "SAME_PERSON_ALREADY_CLAIMED" {
			res.Blocked = true
			res.Claimed = false
			return res, nil
		}
		if out.Amount > 0 {
			res.Earned += out.Amount
		}
		res.Claimed = true
		res.Claimable = false
	}
	if res.Available && len(list.Campaigns) == 0 {
		res.NoCampaign = true
	}
	return res, nil
}
