// billing.go owns the upstream billing API surface: check-in status, user
// resource (credits / packages), payment type, and the perform-* call wrappers
// for daily check-in. Includes the shared JSON helpers used to tolerate the
// upstream's loosely-typed response shapes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func billingHeaders(req *http.Request, sa *storedAuth) {
	// Account-plane endpoints authenticate with the active token as a plain
	// Bearer — jobToken (jt-) or device token (dt-), both accepted upstream.
	// No COSY signing here; the UA/request-id come from the realm so the call
	// does not advertise itself as a script.
	accountHeaders(realmOf(sa))(req)
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
}

// checkinStatusResponse mirrors GET /sash/api/v1/me/daily-check-in/status
// (plain JSON, no envelope).
type checkinStatusResponse struct {
	Status             string `json:"status"` // CLAIMABLE | CLAIMED
	RewardCredits      int64  `json:"rewardCredits"`
	NextClaimAt        int64  `json:"nextClaimAt"` // s epoch
	CurrentStreakDays  int64  `json:"currentStreakDays"`
	TotalClaimDays     int64  `json:"totalClaimDays"`
	TotalRewardCredits int64  `json:"totalRewardCredits"`
	LastClaimedAt      int64  `json:"lastClaimedAt"`   // s epoch
	RewardExpiresAt    int64  `json:"rewardExpiresAt"` // s epoch
}

func fetchCheckinStatus(sa *storedAuth) (*checkinSummary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realmOf(sa).checkinStatusURL(), nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDoFallback(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed ||
		resp.StatusCode == http.StatusGone {
		// This region never had the check-in endpoint (Global returns 404). It
		// is an absent capability, not a failure — say so instead of painting
		// an error on the card.
		return &checkinSummary{Active: false, ActivityName: "本区无签到"}, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("checkin status http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var q checkinStatusResponse
	if err := json.Unmarshal(resp.Body, &q); err != nil {
		return nil, fmt.Errorf("checkin status parse: %w", err)
	}
	today := time.Now().Format("2006-01-02")
	lastClaimed := ""
	if q.LastClaimedAt > 0 {
		lastClaimed = time.Unix(q.LastClaimedAt, 0).Format("2006-01-02")
	}
	sum := &checkinSummary{
		Active:          q.Status == "CLAIMABLE" || q.Status == "CLAIMED",
		TodayCheckedIn:  q.Status == "CLAIMED" && lastClaimed == today,
		StreakDays:      q.CurrentStreakDays,
		DailyCredit:     q.RewardCredits,
		TodayCredit:     0,
		TotalCredits:    q.TotalRewardCredits,
		WeekCheckinDays: q.TotalClaimDays,
		ActivityName:    "每日签到",
	}
	if sum.TodayCheckedIn {
		sum.TodayCredit = q.RewardCredits
	}
	// The legacy endpoint is DISABLED for current accounts; the daily credits
	// live in the activity platform. Overlay it so the card shows the real
	// state instead of "不可签".
	if currentConfig().CampaignCheckin {
		if cl, cerr := fetchCampaigns(sa); cerr == nil {
			mergeCampaignState(sum, cl, sa)
		}
	}
	return sum, nil
}

// mergeCampaignState folds the activity list into the check-in summary. Legacy
// fields win only when they carry a real value: the campaign is the live source.
func mergeCampaignState(sum *checkinSummary, list *campaignList, sa *storedAuth) {
	if sum == nil || list == nil {
		return
	}
	var amount int64
	claimed, claimedAmount := false, int64(0)
	claimable := false
	activity := ""
	for _, c := range list.Campaigns {
		if !c.isCredits() {
			continue
		}
		if activity == "" {
			activity = c.CampaignKey
		}
		if c.creditAmount() > amount {
			amount = c.creditAmount()
		}
		if c.claimed() && c.creditAmount() > 0 {
			claimed = true
			if c.creditAmount() > claimedAmount {
				claimedAmount = c.creditAmount()
			}
		}
		if c.claimable() {
			claimable = true
		}
	}
	if activity == "" {
		// No campaign is published for this account. That is also what a
		// claimed-out day looks like (upstream withdraws the activity once the
		// reward is gone), so a local grant still has to show as checked.
		if !sum.TodayCheckedIn && claimedToday(sa) {
			sum.TodayCheckedIn = true
			if sum.TodayCredit == 0 {
				sum.TodayCredit = sum.DailyCredit
			}
		}
		return
	}
	sum.Active = true
	if activity != "" && sum.ActivityName == "" {
		sum.ActivityName = "每日签到"
	}
	if amount > 0 {
		sum.DailyCredit = amount
	}
	sum.TodayCheckedIn = claimed && !claimable
	if claimedAmount > 0 && sum.TodayCredit == 0 {
		sum.TodayCredit = claimedAmount
	}
	// The list can still say CLAIMABLE after a successful claim (upstream lag),
	// so a local grant wins over it.
	if !sum.TodayCheckedIn && claimedToday(sa) {
		sum.TodayCheckedIn = true
		if sum.TodayCredit == 0 && amount > 0 {
			sum.TodayCredit = amount
		}
	}
}

// quotaUsageResponse mirrors GET /api/v2/quota/usage response (plain JSON,
// no envelope). Both userQuota (base credits) and addOnQuota (one-time pro
// upgrade + checkin packs) are summed for the panel.
type quotaUsageResponse struct {
	UserID               string  `json:"userId"`
	UserType             string  `json:"userType"`
	UsageType            string  `json:"usageType"`
	TotalUsagePercentage float64 `json:"totalUsagePercentage"`
	IsQuotaExceeded      bool    `json:"isQuotaExceeded"`
	ExpiresAt            int64   `json:"expiresAt"` // ms epoch
	UpgradeURL           string  `json:"upgradeUrl"`
	UserQuota            struct {
		Total     float64 `json:"total"`
		Used      float64 `json:"used"`
		Remaining float64 `json:"remaining"`
		Unit      string  `json:"unit"`
	} `json:"userQuota"`
	AddOnQuota struct {
		Total     float64 `json:"total"`
		Used      float64 `json:"used"`
		Remaining float64 `json:"remaining"`
	} `json:"addOnQuota"`
}

// fetchUserResource queries Qoder's quota endpoint and aggregates base +
// add-on credits into the panel's creditsSummary shape.
func fetchUserResource(sa *storedAuth) (*creditsSummary, error) {
	req, err := http.NewRequest(http.MethodGet, realmOf(sa).quotaURL(), nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDoFallback(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("quota/usage http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var q quotaUsageResponse
	if err := json.Unmarshal(resp.Body, &q); err != nil {
		return nil, fmt.Errorf("quota/usage parse: %w", err)
	}
	sum := &creditsSummary{
		TotalRemain: int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining),
		TotalUsed:   int64(q.UserQuota.Used + q.AddOnQuota.Used),
		TotalSize:   int64(q.UserQuota.Total + q.AddOnQuota.Total),
		PackCount:   2,
		Packages: []packageSummary{
			{Name: "基础额度", Remain: int64(q.UserQuota.Remaining), Used: int64(q.UserQuota.Used), Size: int64(q.UserQuota.Total)},
			{Name: "赠送/签到额度", Remain: int64(q.AddOnQuota.Remaining), Used: int64(q.AddOnQuota.Used), Size: int64(q.AddOnQuota.Total)},
		},
	}
	return sum, nil
}

// planResponse mirrors GET /api/v2/user/plan (plain JSON, no envelope).
type planResponse struct {
	UserType      string `json:"user_type"`
	PlanTierName  string `json:"plan_tier_name"`
	IsPersonal    bool   `json:"is_personal_version"`
	IsPaid        bool   `json:"is_paid_plan"`
	IsHighestTier bool   `json:"is_highest_tier"`
	// feature_allowed mixes booleans with numeric limits (wiki_file_count),
	// so a typed map kills the whole plan parse and blanks the panel row.
	FeatureAllowed map[string]any `json:"feature_allowed"`
	StartDate      int64          `json:"start_date"` // ms epoch
	EndDate        int64          `json:"end_date"`   // ms epoch
}

// fetchPlanInfo returns the plan tier label and the account class. user_type
// only exists here — /api/v1/userinfo omits it — and the chat body has to carry
// the real class, so this is where both come from.
func fetchPlanInfo(sa *storedAuth) (tier, userType string, err error) {
	req, reqErr := http.NewRequest(http.MethodGet, realmOf(sa).planURL(), nil)
	if reqErr != nil {
		return "", "", reqErr
	}
	billingHeaders(req, sa)
	resp, respErr := hostHTTPDoFallback(req)
	if respErr != nil {
		return "", "", respErr
	}
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("http %d", resp.StatusCode)
	}
	var p planResponse
	if decodeErr := json.Unmarshal(resp.Body, &p); decodeErr != nil {
		return "", "", fmt.Errorf("decode: %w", decodeErr)
	}
	tier = p.PlanTierName
	if tier == "" {
		tier = p.UserType
	}
	return tier, p.UserType, nil
}

func fetchPaymentType(sa *storedAuth) (string, error) {
	tier, _, err := fetchPlanInfo(sa)
	return tier, err
}

func performCheckinCall(sa *storedAuth) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, realmOf(sa).checkinClaimURL(), strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDoFallback(req)
	if err != nil {
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	// A 409 carrying AlreadyExists is "today's reward is gone", not a failure:
	// surface it in the shape checkinOneAccount already understands.
	if resp.StatusCode == http.StatusConflict && strings.Contains(strings.ToLower(string(resp.Body)), "alreadyexists") {
		return map[string]any{"success": true, "result": "ALREADY_CLAIMED", "errorCode": "AlreadyExists"}, nil
	}
	if resp.StatusCode >= 400 {
		return map[string]any{"success": false, "message": fmt.Sprintf("http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return nil, err
	}
	// Qoder checkin claim returns {"success":true, "rewardCredits":100,...}
	// on success, or {"success":false,"error":"..."} on already-claimed.
	// Normalise to the panel's expected shape (bool success).
	if _, ok := m["success"]; !ok {
		m["success"] = true
	}
	return m, nil
}

// isCreditsExhausted is the shared "耗尽" definition for panel + scheduler.
// Exhausted = we have usage signal and no remaining credits.
// Missing credits data is NOT exhausted (unknown).
func isCreditsExhausted(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	if cr.TotalRemain > 0 {
		return false
	}
	// remain==0: exhausted only when we know there was/is a package total
	// (used>0, size>0, or packages present). Pure zero with no packages = no data.
	if cr.TotalUsed > 0 || cr.TotalSize > 0 {
		return true
	}
	return len(cr.Packages) > 0
}
