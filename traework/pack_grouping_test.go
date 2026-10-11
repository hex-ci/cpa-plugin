package main

import (
	"testing"
	"time"
)

// day is the epoch (UTC midnight) of a date, so the fixtures read as dates.
func day(year int, month time.Month, dayOfMonth int) int64 {
	return time.Date(year, month, dayOfMonth, 0, 0, 0, 0, time.UTC).Unix()
}

// The daily check-in bonus adds one package per claim, so the card must show a
// single row per name — with what the group holds in total and the expiry that
// arrives first.
func TestPacksFromFoldsRepeatedPackagesIntoOneRow(t *testing.T) {
	packs := []entPack{
		packFixture("签到奖励", 100, 0, day(2026, 11, 9)),
		packFixture("每月登录赠送", 500, 88.09, day(2026, 10, 31)),
		packFixture("签到奖励", 100, 0, day(2026, 11, 11)),
		packFixture("签到奖励", 100, 0, day(2026, 11, 10)),
	}

	rows := packsFrom(packs)
	if len(rows) != 2 {
		t.Fatalf("rows = %d (%+v), want one row per name", len(rows), rows)
	}
	if rows[0].Name != "签到奖励" || rows[0].Limit != 300 {
		t.Fatalf("first row = %+v, want the three check-in bonuses summed", rows[0])
	}
	if rows[0].Expires != "2026-11-09" {
		t.Errorf("expiry = %q, want the nearest one regardless of input order", rows[0].Expires)
	}
	if rows[1].Name != "每月登录赠送" || rows[1].Limit != 500 {
		t.Errorf("second row = %+v, want the other package untouched", rows[1])
	}
}

// A group whose entries report no expiry keeps an empty one instead of picking a
// date out of nowhere.
func TestPacksFromKeepsMissingExpiryEmpty(t *testing.T) {
	rows := packsFrom([]entPack{
		packFixture("签到奖励", 100, 0, 0),
		packFixture("签到奖励", 100, 0, day(2026, 11, 10)),
	})
	if len(rows) != 1 || rows[0].Limit != 200 {
		t.Fatalf("rows = %+v, want the two summed", rows)
	}
	if rows[0].Expires != "2026-11-10" {
		t.Errorf("expiry = %q, want the only date the group has", rows[0].Expires)
	}
}

// The reported row count is what the card renders, so it must count groups.
func TestCreditsFromCountsFoldedRows(t *testing.T) {
	var ent entUsageResponse
	ent.UsageSummary.TotalAmount = 800
	ent.UsageSummary.ConsumedAmount = 88.09
	ent.UserEntitlementPackList = []entPack{
		packFixture("签到奖励", 100, 0, 1794163200),
		packFixture("签到奖励", 100, 0, 1794249600),
		packFixture("每月登录赠送", 500, 88.09, day(2026, 10, 31)),
		packFixture("免费", 0, 0, 1793462399),
	}

	view := creditsFrom(ent, payStatusResponse{})
	if view.PackCount != len(view.Packs) || view.PackCount != 3 {
		t.Fatalf("pack_count = %d, rows = %d, want 3 folded rows", view.PackCount, len(view.Packs))
	}
	if view.Packs[0].Limit != 200 {
		t.Errorf("first row = %+v, want the two check-ins summed", view.Packs[0])
	}
}
