// billing.go — quota, plan and daily-bonus snapshots behind the dashboard cards.
// Every call in this file is a read: claiming the bonus is a write and is left
// to the client, so a panel refresh can never change what an account owns.
package main

import (
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	entUsagePath      = "/trae/api/v2/pay/ide_user_ent_usage"
	payStatusPath     = "/trae/api/v2/pay/ide_user_pay_status"
	checkinStatusPath = "/trae/api/v2/ug/checkin_credits/status"

	// billingTTL bounds how often an account's billing API is asked: the panel
	// loads one card at a time and the numbers move in credit-sized steps, not
	// seconds. The refresh button bypasses it.
	billingTTL = 60 * time.Second
)

// creditsView is one account's quota snapshot, shaped around the three numbers
// the card's progress bar and meta line show.
type creditsView struct {
	FetchedAt string `json:"fetched_at,omitempty"`
	// TotalRemain is what the account can still spend, TotalUsed what it spent
	// in the current cycle, TotalSize the cycle's ceiling (remain+used when the
	// server reports no ceiling of its own).
	TotalRemain float64 `json:"total_remain"`
	TotalUsed   float64 `json:"total_used"`
	TotalSize   float64 `json:"total_size"`
	// Ratio is the server's own consumption ratio, 0-1.
	Ratio     float64    `json:"ratio,omitempty"`
	Billing   string     `json:"billing,omitempty"`
	Identity  string     `json:"identity,omitempty"`
	PackCount int        `json:"pack_count,omitempty"`
	Packs     []packView `json:"packs,omitempty"`
}

// packView is one entitlement package, e.g. "每月登录积分" or "免费".
type packView struct {
	Name    string  `json:"name"`
	Limit   float64 `json:"limit,omitempty"`
	Used    float64 `json:"used,omitempty"`
	Expires string  `json:"expires,omitempty"`
}

// checkinView is the daily bonus state. Credits is what one check-in pays.
type checkinView struct {
	Enabled        bool  `json:"enabled"`
	TodayCheckedIn bool  `json:"today_checked_in"`
	Credits        int64 `json:"credits,omitempty"`
	ExtraCredits   int64 `json:"extra_credits,omitempty"`
}

// entUsageResponse mirrors /pay/ide_user_ent_usage. The pack list is where the
// per-package detail comes from; usage_summary carries the cycle totals.
//
// Whether a pack belongs to the SOLO pool the plugin serves (ide_credits) or to
// the TRAE Work pool (work_credits) is not distinguishable from these fields
// alone: the account's channel flags live in pay_status. Both pools land in
// usage_summary, so the card reports the account-level view and the pack rows
// name each package, rather than pretending the split is known.
type entUsageResponse struct {
	IsCreditsBilling     bool `json:"is_credits_billing"`
	IsDollarUsageBilling bool `json:"is_dollar_usage_billing"`
	UsageSummary         struct {
		ConsumedAmount   float64 `json:"consumed_amount"`
		TotalAmount      float64 `json:"total_amount"`
		ConsumptionRatio float64 `json:"consumption_ratio"`
	} `json:"usage_summary"`
	UserEntitlementPackList []entPack `json:"user_entitlement_pack_list"`
}

type entPack struct {
	DisplayDesc string `json:"display_desc"`
	GroupName   string `json:"group_name"`
	ExpireTime  int64  `json:"expire_time"`
	IsHide      bool   `json:"is_hide"`
	// BaseInfo.EndTime is the pack's own expiry; ExpireTime is the newer field
	// and may be absent, so both are read.
	EntitlementBaseInfo struct {
		EndTime int64 `json:"end_time"`
		Quota   struct {
			CreditsLimit float64 `json:"credits_limit"`
		} `json:"quota"`
	} `json:"entitlement_base_info"`
	Usage struct {
		CreditsAmount float64 `json:"credits_amount"`
	} `json:"usage"`
}

// payStatusResponse mirrors /pay/ide_user_pay_status. Only the fields the card
// uses are decoded.
type payStatusResponse struct {
	UserPayIdentityStr string `json:"user_pay_identity_str"`
	EnableSoloLite     bool   `json:"enable_solo_lite"`
	IsCreditsBilling   bool   `json:"is_credits_billing"`
}

// checkinStatusResponse mirrors /ug/checkin_credits/status.
type checkinStatusResponse struct {
	CheckedIn    bool  `json:"checked_in"`
	DidCheckedIn bool  `json:"did_checked_in"`
	Enable       bool  `json:"enable"`
	Credits      int64 `json:"credits"`
	// ExtraCredits is the promotional top-up some accounts get on top of the
	// base amount.
	ExtraCredits int64 `json:"extra_credits"`
}

// ugReqSourceLite is the client kind the user-growth endpoints expect in the
// body: 2 = SOLO/Lite (this plugin's channel), 1 = IDE.
const ugReqSourceLite = 2

// ugDeviceID is the device identifier the user-growth endpoints want. They
// reject the UUID the OAuth login registers, and they reject it with the generic
// "当前参与用户太多" (9074) business code — which reads like a busy window and
// therefore never clears. The desktop client sends the numeric id its native
// device service hands it; a stable numeric id per account is accepted the same
// way, so one is derived from the stored UUID and reused for every call.
func ugDeviceID(sa *storedAuth) string {
	seed := strings.TrimSpace(sa.DeviceID)
	if seed == "" {
		seed = strings.TrimSpace(sa.UID)
	}
	if seed == "" {
		return ""
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(seed))
	// 13 digits: the shape the client's own device id has.
	return strconv.FormatUint(1_000_000_000_000+hash.Sum64()%9_000_000_000_000, 10)
}

// ugHeaders builds the header set for the user-growth endpoints: the OAuth
// headers minus the IDE-specific ones, plus the numeric device id and the region
// the service partitions on.
func ugHeaders(sa *storedAuth) map[string]string {
	headers := map[string]string{
		"Authorization": "Cloud-IDE-JWT " + sa.AccessToken,
		"User-Agent":    ideUserAgent,
		"X-User-Region": "CN",
	}
	if device := ugDeviceID(sa); device != "" {
		headers["X-Device-Id"] = device
	}
	return headers
}

// ugBody is the body the desktop client sends to the user-growth endpoints: it
// declares which client is asking.
func ugBody() map[string]any {
	return map[string]any{"req_source": ugReqSourceLite}
}

// fetchCredentials reads the three billing endpoints. Only the entitlement
// usage is load-bearing: a failed plan or bonus lookup degrades the card
// instead of blanking out the quota it is there to show.
func fetchCredentials(sa *storedAuth) (*creditsView, *checkinView, error) {
	headers := ugHeaders(sa)

	var ent entUsageResponse
	if err := postJSON(apiBaseCN+entUsagePath, headers, map[string]any{"require_usage": true}, &ent); err != nil {
		return nil, nil, err
	}

	var pay payStatusResponse
	_ = postJSON(apiBaseCN+payStatusPath, headers, map[string]any{
		"trae_client": "Lite",
		"device_id":   ugDeviceID(sa),
	}, &pay)

	var checkin checkinStatusResponse
	_ = postJSON(apiBaseCN+checkinStatusPath, headers, ugBody(), &checkin)

	return creditsFrom(ent, pay), checkinFrom(checkin), nil
}

// creditsFrom folds the usage response into the card's view.
func creditsFrom(ent entUsageResponse, pay payStatusResponse) *creditsView {
	summary := ent.UsageSummary
	view := &creditsView{
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		TotalUsed: roundCredits(summary.ConsumedAmount),
		TotalSize: roundCredits(summary.TotalAmount),
		Ratio:     summary.ConsumptionRatio,
		Identity:  pay.UserPayIdentityStr,
		Packs:     packsFrom(ent.UserEntitlementPackList),
		PackCount: len(ent.UserEntitlementPackList),
	}
	switch {
	case ent.IsDollarUsageBilling:
		view.Billing = "美元计费"
	case ent.IsCreditsBilling:
		view.Billing = "积分计费"
	}
	// The ceiling is what the bar is measured against. When the server reports
	// none, the packages are the only source of what is left, and the pool is
	// whatever is left plus what was spent.
	if view.TotalSize > 0 {
		view.TotalRemain = roundCredits(view.TotalSize - view.TotalUsed)
		if view.TotalRemain < 0 {
			view.TotalRemain = 0
		}
	} else {
		// The summary is absent, so both numbers come from the packages; taking
		// used from the summary would leave spend uncounted.
		var used, remain float64
		for _, pack := range view.Packs {
			used += pack.Used
			if pack.Limit > pack.Used {
				remain += pack.Limit - pack.Used
			}
		}
		view.TotalUsed = roundCredits(used)
		view.TotalRemain = roundCredits(remain)
		view.TotalSize = roundCredits(view.TotalRemain + view.TotalUsed)
	}
	return view
}

// packsFrom names each package for the breakdown rows, skipping the hidden ones.
func packsFrom(packs []entPack) []packView {
	out := make([]packView, 0, len(packs))
	for _, pack := range packs {
		if pack.IsHide {
			continue
		}
		name := strings.TrimSpace(pack.DisplayDesc)
		if name == "" {
			name = strings.TrimSpace(pack.GroupName)
		}
		if name == "" {
			name = "额度包"
		}
		expires := pack.ExpireTime
		if expires == 0 {
			expires = pack.EntitlementBaseInfo.EndTime
		}
		out = append(out, packView{
			Name:    name,
			Limit:   roundCredits(pack.EntitlementBaseInfo.Quota.CreditsLimit),
			Used:    roundCredits(pack.Usage.CreditsAmount),
			Expires: formatDay(expires),
		})
	}
	return out
}

func checkinFrom(res checkinStatusResponse) *checkinView {
	if !res.Enable && !res.CheckedIn && !res.DidCheckedIn && res.Credits == 0 {
		return nil
	}
	return &checkinView{
		Enabled:        res.Enable,
		TodayCheckedIn: res.CheckedIn || res.DidCheckedIn,
		Credits:        res.Credits,
		ExtraCredits:   res.ExtraCredits,
	}
}

// roundCredits trims the float noise upstream sends (14.6484 is 14.65).
func roundCredits(value float64) float64 {
	return float64(int64(value*100+0.5)) / 100
}

// formatDay renders a seconds-or-milliseconds stamp as a date, matching how the
// client shows package expiry.
func formatDay(stamp int64) string {
	if stamp <= 0 {
		return ""
	}
	if stamp > 1e11 { // milliseconds
		stamp /= 1000
	}
	return time.Unix(stamp, 0).UTC().Format("2006-01-02")
}

// The panel asks per card, so results are cached per auth index: without this
// every dashboard reload would hit the account's billing API once per card.
type billingEntry struct {
	credits *creditsView
	checkin *checkinView
	at      time.Time
	err     error
}

var (
	billingMu    sync.Mutex
	billingCache = map[string]billingEntry{}
)

// fetchCredentialsFn is a seam: management tests replace it so no test reaches
// upstream.
var fetchCredentialsFn = fetchCredentials

// cachedCredentials returns an account's snapshot, fetching it when the cached
// one is missing or older than billingTTL. fresh skips the cache.
func cachedCredentials(sa *storedAuth, authIndex string, fresh bool) (*creditsView, *checkinView, error) {
	billingMu.Lock()
	if entry, ok := billingCache[authIndex]; ok && !fresh && time.Since(entry.at) < billingTTL {
		billingMu.Unlock()
		return entry.credits, entry.checkin, entry.err
	}
	billingMu.Unlock()

	credits, checkin, err := fetchCredentialsFn(sa)

	billingMu.Lock()
	billingCache[authIndex] = billingEntry{credits: credits, checkin: checkin, at: time.Now(), err: err}
	billingMu.Unlock()

	return credits, checkin, err
}

// peekCredentials returns the cached snapshot without fetching, so listing
// accounts stays a single round trip. The panel fills the gaps per card.
func peekCredentials(authIndex string) (*creditsView, *checkinView) {
	billingMu.Lock()
	defer billingMu.Unlock()
	entry, ok := billingCache[authIndex]
	if !ok {
		return nil, nil
	}
	return entry.credits, entry.checkin
}
