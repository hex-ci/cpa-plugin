// credit_groups_test.go pins the client-shaped package→group breakdown: the
// code table is verbatim from the client's constants, grouping matches the
// client's realm rules, and the aggregated rows always sum to the totals.
package main

import (
	"testing"
	"time"
)

func TestCreditGroupFor(t *testing.T) {
	cases := []struct {
		code   string
		isIntl bool
		want   creditGroupKind
	}{
		// CN: the free monthly pack IS the free tier's plan base.
		{pkgFreeMon, false, groupPlanBase},
		// International: freeMon and gift are benefit (平台奖励) packs.
		{pkgFreeMon, true, groupBenefit},
		{pkgGift, true, groupBenefit},
		{pkgGift, false, groupOther},
		{pkgActivity, false, groupBenefit},
		{pkgActivity, true, groupBenefit},
		{pkgBonus28, false, groupPlanBonus},
		{pkgBonusIntl, true, groupPlanBonus},
		{pkgExtra, false, groupAddon},
		{pkgExtraIntl, true, groupAddon},
		{pkgProMon, false, groupPlanBase},
		{pkgFlagship, true, groupPlanBase},
		{pkgProTrialMon, true, groupPlanBase},
		// Unmapped codes never vanish: they land in "other".
		{"TCACA_code_999_future", false, groupOther},
	}
	for _, c := range cases {
		if got := creditGroupFor(c.code, c.isIntl); got != c.want {
			t.Errorf("creditGroupFor(%q, intl=%v) = %q, want %q", c.code, c.isIntl, got, c.want)
		}
	}
}

// Real measured shapes: the CN fission pack (TCACA_code_007) and the Global
// bonus pack (TCACA_code_035) both land in benefit and group-sum to the total.
func TestCreditGroupAccumulatorSumsToTotals(t *testing.T) {
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, softRateResetLoc)
	acc := newCreditGroupAccumulator(false, now)
	pkgs := []resourcePackage{
		{PackageName: "CodeBuddy个人版国内运营裂变包", PackageCode: pkgActivity, CycleCapacitySize: 5000, CycleCapacityRemain: 3742,
			CycleEndTime: "2026-10-11 00:31:29", DeductionEndTime: 1791649889000},
		{PackageName: "CodeBuddy个人版国内运营裂变包", PackageCode: pkgActivity, CycleCapacitySize: 100, CycleCapacityRemain: 100,
			CycleEndTime: "2026-10-01 09:00:02", DeductionEndTime: 1790926802000},
		{PackageName: "CodeBuddy个人体验版", PackageCode: pkgFreeMon, CycleCapacitySize: 500, CycleCapacityRemain: 0,
			CycleEndTime: "2026-09-30 23:59:59"},
	}
	wantTotal := int64(0)
	for _, p := range pkgs {
		remain, used, size := packageRemainUsed(p)
		wantTotal += size
		acc.add(p, remain, used, size)
	}
	rows := acc.rows()

	var sumTotal, sumUsed, sumRemain int64
	seen := map[creditGroupKind]bool{}
	for _, r := range rows {
		sumTotal += r.Total
		sumUsed += r.Used
		sumRemain += r.Remain
		seen[creditGroupKind(r.Kind)] = true
	}
	if sumTotal != wantTotal || sumRemain != int64(3842) || sumUsed != wantTotal-3842 {
		t.Fatalf("group sums = total %d used %d remain %d, want total %d remain 3842", sumTotal, sumUsed, sumRemain, wantTotal)
	}
	if !seen[groupBenefit] || !seen[groupPlanBase] {
		t.Fatalf("rows = %#v, want benefit + planBase", rows)
	}
	// Row order follows the client's listing order.
	if rows[0].Kind != string(groupPlanBase) || rows[1].Kind != string(groupBenefit) {
		t.Fatalf("row order = %s, %s; want planBase first", rows[0].Kind, rows[1].Kind)
	}
	// Names ride the row so the panel never maps kinds itself.
	if rows[0].Name != "套餐基础积分" || rows[1].Name != "平台奖励积分" {
		t.Fatalf("names = %q, %q", rows[0].Name, rows[1].Name)
	}
}

// An unmapped package code lands in "other" so the rows still reconcile with
// the account total — the deliberate divergence from the client, which drops
// unknown codes.
func TestCreditGroupUnknownCodeGoesToOther(t *testing.T) {
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, softRateResetLoc)
	acc := newCreditGroupAccumulator(false, now)
	acc.add(resourcePackage{PackageCode: "TCACA_code_999_future", CycleCapacitySize: 100, CycleCapacityRemain: 40,
		CycleEndTime: "2026-12-01 00:00:00"}, 40, 60, 100)
	rows := acc.rows()
	if len(rows) != 1 || rows[0].Kind != string(groupOther) || rows[0].Total != 100 || rows[0].Remain != 40 {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].Name != "其他" {
		t.Fatalf("other name = %q", rows[0].Name)
	}
}

func TestExpiryTagFor(t *testing.T) {
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, softRateResetLoc)
	cases := []struct {
		name   string
		basis  time.Time
		remain int64
		want   string
	}{
		{"24h内到期", now.Add(10 * time.Hour), 5, "critical"},
		{"3天内到期", now.Add(2 * 24 * time.Hour), 5, "warning"},
		{"远期不标", now.Add(30 * 24 * time.Hour), 5, ""},
		{"已用完不标", now.Add(10 * time.Hour), 0, ""},
		{"已过期不标", now.Add(-time.Hour), 5, ""},
		{"零值不标", time.Time{}, 5, ""},
	}
	for _, c := range cases {
		if got := expiryTagFor(c.basis, c.remain, now); got != c.want {
			t.Errorf("%s: expiryTagFor = %q, want %q", c.name, got, c.want)
		}
	}
}

// The free tier's plan base resets on its own; a nearing refresh must NOT
// raise the client's expiry warning (the same exception the client makes).
func TestTrialPlanBaseNeverWarns(t *testing.T) {
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, softRateResetLoc)
	acc := newCreditGroupAccumulator(false, now)
	acc.add(resourcePackage{PackageCode: pkgFreeMon, CycleCapacitySize: 500, CycleCapacityRemain: 200,
		CycleEndTime: now.Add(6 * time.Hour).Format("2006-01-02 15:04:05")}, 200, 300, 500)
	rows := acc.rows()
	if len(rows) != 1 || rows[0].Kind != string(groupPlanBase) {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].ExpiryTag != "" {
		t.Fatalf("trial plan base raised %q, want no tag", rows[0].ExpiryTag)
	}
	// The refresh time annotation still renders.
	if rows[0].TimeText == "" {
		t.Fatal("trial plan base lost its refresh time")
	}
}

// An addon pack nearing expiry gets the client's critical tag.
func TestAddonNearExpiryTagsCritical(t *testing.T) {
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, softRateResetLoc)
	acc := newCreditGroupAccumulator(false, now)
	acc.add(resourcePackage{PackageCode: pkgExtra, CycleCapacitySize: 100, CycleCapacityRemain: 50,
		CycleEndTime:     now.Add(48 * time.Hour).Format("2006-01-02 15:04:05"),
		DeductionEndTime: now.Add(10 * time.Hour).UnixMilli()}, 50, 50, 100)
	rows := acc.rows()
	if len(rows) != 1 || rows[0].Kind != string(groupAddon) {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].ExpiryTag != "critical" {
		t.Fatalf("tag = %q, want critical", rows[0].ExpiryTag)
	}
	// The addon basis is the deduction end (recent), not the cycle end.
	if rows[0].TimeText != "最近到期 "+now.Add(10*time.Hour).In(softRateResetLoc).Format("2006-01-02") {
		t.Fatalf("time text = %q", rows[0].TimeText)
	}
}

// Exhausted packs never contribute the group's "nearest usable expiry": a
// spent pack must not make the row look about to expire.
func TestExhaustedPackSkippedForExpiryBasis(t *testing.T) {
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, softRateResetLoc)
	acc := newCreditGroupAccumulator(false, now)
	// Spent pack expiring within 24h; healthy pack expiring far later.
	acc.add(resourcePackage{PackageCode: pkgActivity, CycleCapacitySize: 100, CycleCapacityRemain: 0,
		DeductionEndTime: now.Add(5 * time.Hour).UnixMilli()}, 0, 100, 100)
	acc.add(resourcePackage{PackageCode: pkgActivity, CycleCapacitySize: 200, CycleCapacityRemain: 150,
		DeductionEndTime: now.Add(20 * 24 * time.Hour).UnixMilli()}, 150, 50, 200)
	rows := acc.rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].ExpiryTag != "" {
		t.Fatalf("tag = %q, want none (only far-future healthy pack)", rows[0].ExpiryTag)
	}
	if rows[0].Remain != 150 || rows[0].Total != 300 {
		t.Fatalf("sums = %#v", rows[0])
	}
}

func TestParseBillingTime(t *testing.T) {
	if _, ok := parseBillingTime(""); ok {
		t.Fatal("empty parsed ok")
	}
	if _, ok := parseBillingTime("not-a-time"); ok {
		t.Fatal("garbage parsed ok")
	}
	got, ok := parseBillingTime("2026-10-11 00:31:29")
	if !ok {
		t.Fatal("valid time failed")
	}
	if y := got.In(softRateResetLoc).Year(); y != 2026 {
		t.Fatalf("year = %d", y)
	}
}
