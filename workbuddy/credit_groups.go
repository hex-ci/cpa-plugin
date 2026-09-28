// credit_groups.go maps upstream package codes to the four display groups the
// official client uses in its usage panel (套餐基础 / 套餐赠送 / 平台奖励 /
// 购买积分), and aggregates per-package figures into those groups.
//
// The code→group table is the client's own constant set (COMMODITY_CODES +
// resolveCreditGroup), extracted verbatim. The client DROPS codes it does not
// recognise, which makes the group rows stop adding up to the total; this
// implementation routes unknown codes into an "other" group instead so the
// breakdown always reconciles with the main progress bar.
package main

import (
	"strings"
	"time"
)

// creditGroupKind identifies one display group. Values ride the wire to the
// panel (kind field); the display name travels alongside in Name.
type creditGroupKind string

const (
	groupPlanBase  creditGroupKind = "planBase"
	groupPlanBonus creditGroupKind = "planBonus"
	groupAddon     creditGroupKind = "addon"
	groupBenefit   creditGroupKind = "benefit"
	groupOther     creditGroupKind = "other"
)

// creditGroupDisplayNames are the panel labels, mirroring the client's zh
// strings. The "other" bucket is this plugin's addition (the client discards
// unknown codes; we surface them so rows still sum to the total).
var creditGroupDisplayNames = map[creditGroupKind]string{
	groupPlanBase:  "套餐基础积分",
	groupPlanBonus: "套餐赠送积分",
	groupAddon:     "购买积分",
	groupBenefit:   "平台奖励积分",
	groupOther:     "其他",
}

// creditGroupRowOrder is the order the client lists breakdown rows in.
var creditGroupRowOrder = []creditGroupKind{groupPlanBase, groupPlanBonus, groupAddon, groupBenefit, groupOther}

// Upstream package codes (client COMMODITY_CODES, verbatim).
const (
	pkgFree        = "TCACA_code_001_PqouKr6QWV"
	pkgProYear     = "TCACA_code_003_FAnt7lcmRT"
	pkgProMon      = "TCACA_code_002_AkiJS3ZHF5"
	pkgProMonPlus  = "TCACA_code_005_maRGyrHhw1"
	pkgGift        = "TCACA_code_006_DbXS0lrypC"
	pkgActivity    = "TCACA_code_007_nzdH5h4Nl0"
	pkgFreeMon     = "TCACA_code_008_cfWoLwvjU4"
	pkgExtra       = "TCACA_code_009_0XmEQc2xOf"
	pkgYouth       = "TCACA_code_023_4xbGhMrE6q"
	pkgAdvanced    = "TCACA_code_026_BaESVICNoi"
	pkgFlagship    = "TCACA_code_027_0FCGVA6vSa"
	pkgBonus28     = "TCACA_code_028_NtpWi0jzXs"
	pkgBonus29     = "TCACA_code_029_6wCGEWquYy"
	pkgBonus30     = "TCACA_code_030_BjSt89qTvr"
	pkgFreeMonIntl = "TCACA_code_035_ArVxJcGDsm"
	pkgExtraIntl   = "TCACA_code_036_lupO5WgNdG"
	pkgBonusIntl   = "TCACA_code_037_WxOD3MpI2o"
	pkgExtra38     = "TCACA_code_038_OhvqZtiPKr"
	pkgProTrialMon = "TCACA_code_039_KRcQj7wUat"
	pkgTrialYear   = "TCACA_code_040_mi9rCYg46x"
)

var planBaseCodes = map[string]struct{}{
	pkgProMon: {}, pkgProMonPlus: {}, pkgProYear: {}, pkgYouth: {},
	pkgAdvanced: {}, pkgFlagship: {}, pkgFreeMonIntl: {},
	pkgProTrialMon: {}, pkgTrialYear: {},
}
var planBonusCodes = map[string]struct{}{pkgBonus28: {}, pkgBonusIntl: {}}
var addonCodes = map[string]struct{}{pkgExtra: {}, pkgExtra38: {}, pkgExtraIntl: {}}
var benefitCodes = map[string]struct{}{pkgActivity: {}, pkgBonus29: {}, pkgBonus30: {}}

// trialPlanBaseCodes are the free-tier monthly packs. Their group resets
// automatically every period, so a nearing refresh is not an expiry warning
// (the client makes the same exception).
var trialPlanBaseCodes = map[string]struct{}{pkgFreeMon: {}, pkgFreeMonIntl: {}}

// isDailyPackageCode: the free daily pack's cycle end is just the end of the
// current day, not a validity horizon — it never contributes expiry times.
func isDailyPackageCode(code string) bool { return code == pkgFree }

// creditGroupFor resolves the display group for one package code.
// isIntl selects the client's international mapping: freeMon and gift count as
// 平台奖励 there, while on CN freeMon is the free tier's plan base.
func creditGroupFor(code string, isIntl bool) creditGroupKind {
	if _, ok := planBaseCodes[code]; ok {
		return groupPlanBase
	}
	if !isIntl && code == pkgFreeMon {
		return groupPlanBase
	}
	if _, ok := planBonusCodes[code]; ok {
		return groupPlanBonus
	}
	if _, ok := addonCodes[code]; ok {
		return groupAddon
	}
	if _, ok := benefitCodes[code]; ok {
		return groupBenefit
	}
	if isIntl && (code == pkgFreeMon || code == pkgGift) {
		return groupBenefit
	}
	return groupOther
}

// creditGroup is one aggregated display row inside creditsSummary.
type creditGroup struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
	Remain int64  `json:"remain"`
	// TimeText is the row's time annotation, client-shaped: plan groups show
	// 下次刷新, addon/benefit show 最近到期, "other" has none.
	TimeText string `json:"time_text,omitempty"`
	// ExpiryTag is "critical" (≤24h) or "warning" (≤3d) by the client's
	// thresholds. Empty when the group is exhausted, has no basis time, or is
	// a trial plan base (auto-resetting, never "about to expire").
	ExpiryTag string `json:"expiry_tag,omitempty"`
}

// creditGroupAccumulator builds the ordered group rows for one account.
type creditGroupAccumulator struct {
	isIntl bool
	now    time.Time
	// buckets holds per-kind totals plus the time basis inputs.
	buckets map[creditGroupKind]*creditGroupBucket
}

type creditGroupBucket struct {
	total, used, remain int64
	// refreshAt is the earliest cycle end across the group's packs (plan-type
	// time basis).
	refreshAt time.Time
	// nearestUsableExpire is the earliest expiry across packs that still have
	// remainder and are not daily (addon/benefit time basis).
	nearestUsableExpire time.Time
	hasRefresh          bool
	hasUsableExpire     bool
	allTrialPlanBase    bool
	seenAny             bool
}

func newCreditGroupAccumulator(isIntl bool, now time.Time) *creditGroupAccumulator {
	return &creditGroupAccumulator{isIntl: isIntl, now: now, buckets: make(map[creditGroupKind]*creditGroupBucket)}
}

// add folds one package into its group. times carry the client's semantics:
// cycleEnd feeds the plan-type refresh basis; expireAt (DeductionEndTime) and
// the remain figure feed the addon/benefit expiry basis.
func (a *creditGroupAccumulator) add(pkg resourcePackage, remain, used, size int64) {
	kind := creditGroupFor(pkg.PackageCode, a.isIntl)
	b := a.buckets[kind]
	if b == nil {
		b = &creditGroupBucket{allTrialPlanBase: true}
		a.buckets[kind] = b
	}
	b.seenAny = true
	b.total += size
	b.used += used
	b.remain += remain
	if kind == groupPlanBase {
		b.allTrialPlanBase = b.allTrialPlanBase && isTrialPlanBaseCode(pkg.PackageCode)
	}
	if cycleEnd, ok := parseBillingTime(pkg.CycleEndTime); ok {
		if !b.hasRefresh || cycleEnd.Before(b.refreshAt) {
			b.refreshAt = cycleEnd
			b.hasRefresh = true
		}
	}
	if !isDailyPackageCode(pkg.PackageCode) && remain > 0 {
		if expireAt, ok := packageExpireTime(pkg); ok && expireAt.After(a.now) {
			if !b.hasUsableExpire || expireAt.Before(b.nearestUsableExpire) {
				b.nearestUsableExpire = expireAt
				b.hasUsableExpire = true
			}
		}
	}
}

// rows renders the ordered display list. Empty groups are skipped; the total
// across rows always equals the account total because every package lands in
// exactly one bucket (unknown codes in "other").
func (a *creditGroupAccumulator) rows() []creditGroup {
	var out []creditGroup
	for _, kind := range creditGroupRowOrder {
		b := a.buckets[kind]
		if b == nil || !b.seenAny {
			continue
		}
		row := creditGroup{
			Kind:   string(kind),
			Name:   creditGroupDisplayNames[kind],
			Total:  b.total,
			Used:   b.used,
			Remain: b.remain,
		}
		switch kind {
		case groupAddon, groupBenefit:
			if b.hasUsableExpire {
				row.TimeText = "最近到期 " + b.nearestUsableExpire.In(softRateResetLoc).Format("2006-01-02")
				row.ExpiryTag = expiryTagFor(b.nearestUsableExpire, b.remain, a.now)
			}
		case groupPlanBase, groupPlanBonus:
			if b.hasRefresh {
				row.TimeText = "下次刷新 " + b.refreshAt.In(softRateResetLoc).Format("2006-01-02")
				if !(kind == groupPlanBase && b.allTrialPlanBase) {
					row.ExpiryTag = expiryTagFor(b.refreshAt, b.remain, a.now)
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// expiryTagFor applies the client's thresholds: ≤24h critical, ≤3d warning,
// nothing when the group is spent or the instant is missing/far away.
func expiryTagFor(basis time.Time, remain int64, now time.Time) string {
	if remain <= 0 || basis.IsZero() || !basis.After(now) {
		return ""
	}
	switch d := basis.Sub(now); {
	case d <= 24*time.Hour:
		return "critical"
	case d <= 3*24*time.Hour:
		return "warning"
	default:
		return ""
	}
}

func isTrialPlanBaseCode(code string) bool {
	_, ok := trialPlanBaseCodes[code]
	return ok
}

// packageExpireTime resolves one package's validity end: DeductionEndTime
// (epoch ms) when present, else the parsed CycleEndTime.
func packageExpireTime(pkg resourcePackage) (time.Time, bool) {
	if pkg.DeductionEndTime > 0 {
		return time.UnixMilli(pkg.DeductionEndTime), true
	}
	return parseBillingTime(pkg.CycleEndTime)
}

// parseBillingTime parses the billing API's wall-clock timestamps, which are
// UTC+8 regardless of host timezone (same zone the soft-rate reset parser
// already fixes).
func parseBillingTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
