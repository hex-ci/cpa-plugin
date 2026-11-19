package main

import (
	"testing"
	"time"
)

// A restart after the reset hour must not cost the whole day: the timer only
// ever points at the next slot, so without a catch-up path a host that came up
// at 10:00 would skip check-in until tomorrow while manual check-in still
// worked — the exact shape of the reported bug.
func TestAfterFirstCheckinHour(t *testing.T) {
	old := checkinHours
	checkinHours = []int{9, 21}
	t.Cleanup(func() { checkinHours = old })

	loc := time.Local
	cases := []struct {
		hour int
		want bool
	}{
		{8, false},
		{9, true},
		{15, true},
		{23, true},
	}
	for _, tc := range cases {
		now := time.Date(2026, 11, 19, tc.hour, 30, 0, 0, loc)
		if got := afterFirstCheckinHour(now); got != tc.want {
			t.Fatalf("hour %d: afterFirstCheckinHour = %v, want %v", tc.hour, got, tc.want)
		}
	}
}

// The earliest slot decides the catch-up gate, not the first element of the
// list: schedules are edited by humans and order is not meaningful.
func TestAfterFirstCheckinHourUsesEarliestSlot(t *testing.T) {
	old := checkinHours
	checkinHours = []int{21, 9}
	t.Cleanup(func() { checkinHours = old })

	if !afterFirstCheckinHour(time.Date(2026, 11, 19, 9, 0, 0, 0, time.Local)) {
		t.Fatal("catch-up gate ignored the earliest slot")
	}
	if afterFirstCheckinHour(time.Date(2026, 11, 19, 8, 59, 0, 0, time.Local)) {
		t.Fatal("catch-up ran before the earliest slot")
	}
}

// markAutoCheckinDone is what makes catch-up idempotent: once today's run is
// recorded, a reconfigure must not replay it (that would check in again on a
// host that reloads config frequently).
func TestMarkAutoCheckinDoneBlocksRepeatRun(t *testing.T) {
	lastCheckinMu.Lock()
	prev := lastAutoCheckinDay
	lastAutoCheckinDay = ""
	lastCheckinMu.Unlock()
	t.Cleanup(func() {
		lastCheckinMu.Lock()
		lastAutoCheckinDay = prev
		lastCheckinMu.Unlock()
	})

	now := time.Now()
	lastCheckinMu.Lock()
	before := lastAutoCheckinDay == now.Format("2006-01-02")
	lastCheckinMu.Unlock()
	if before {
		t.Fatal("test precondition: day already marked")
	}

	markAutoCheckinDone(now)

	lastCheckinMu.Lock()
	after := lastAutoCheckinDay == now.Format("2006-01-02")
	lastCheckinMu.Unlock()
	if !after {
		t.Fatal("markAutoCheckinDone did not record today")
	}
}
