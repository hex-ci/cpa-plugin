package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The pass must only touch a token that is about to expire: every exchange
// invalidates the old refresh token, so rotating a healthy two-week token every
// night would add risk without keeping anything alive.
func TestShouldRotateOnlyNearExpiry(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		sa   *storedAuth
		want bool
	}{
		{name: "nil record", sa: nil, want: false},
		{name: "no refresh token", sa: &storedAuth{ExpiresAt: now.Add(-time.Hour).Unix()}, want: false},
		{name: "no expiry", sa: &storedAuth{RefreshToken: "rt"}, want: false},
		{name: "already expired", sa: &storedAuth{RefreshToken: "rt", ExpiresAt: now.Add(-2 * time.Hour).Unix()}, want: true},
		{name: "inside the lead time", sa: &storedAuth{RefreshToken: "rt", ExpiresAt: now.Add(6 * time.Hour).Unix()}, want: true},
		{name: "just outside the lead time", sa: &storedAuth{RefreshToken: "rt", ExpiresAt: now.Add(48 * time.Hour).Unix()}, want: false},
	}
	for _, tc := range cases {
		if got := shouldRotate(tc.sa, now); got != tc.want {
			t.Errorf("%s: shouldRotate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The daily slot is the configured evening hour, local time, and never in the
// past.
func TestNextKeepaliveTimeIsTheNextEveningSlot(t *testing.T) {
	before := time.Date(2026, 10, 24, 9, 0, 0, 0, time.Local)
	if got := nextKeepaliveTime(before); got.Hour() != keepaliveHour || !got.After(before) || got.Day() != 24 {
		t.Fatalf("nextKeepaliveTime(09:00) = %v, want today %02d:00", got, keepaliveHour)
	}
	after := time.Date(2026, 10, 24, 23, 30, 0, 0, time.Local)
	if got := nextKeepaliveTime(after); got.Day() != 25 || got.Hour() != keepaliveHour {
		t.Fatalf("nextKeepaliveTime(23:30) = %v, want tomorrow %02d:00", got, keepaliveHour)
	}
}

// A pass rotates only the due accounts, skips disabled ones and counts the rest,
// so the operator can tell "nothing to do" from "everything failed".
func TestRunKeepaliveRotatesOnlyDueAccounts(t *testing.T) {
	now := time.Now()
	accounts := map[string]*storedAuth{
		"due":      {UID: "u-due", RefreshToken: "rt", ExpiresAt: now.Add(-time.Hour).Unix()},
		"healthy":  {UID: "u-healthy", RefreshToken: "rt", ExpiresAt: now.Add(240 * time.Hour).Unix()},
		"disabled": {UID: "u-disabled", RefreshToken: "rt", ExpiresAt: now.Add(-time.Hour).Unix()},
		"broken":   {UID: "u-broken", RefreshToken: "rt", ExpiresAt: now.Add(-time.Hour).Unix()},
	}
	previousList, previousGet, previousRefresh := keepaliveAccountsFn, keepaliveAccountGetFn, keepaliveRefreshFn
	t.Cleanup(func() {
		keepaliveAccountsFn, keepaliveAccountGetFn, keepaliveRefreshFn = previousList, previousGet, previousRefresh
	})

	keepaliveAccountsFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{AuthIndex: "due", Name: "traework-due.json"},
			{AuthIndex: "healthy", Name: "traework-healthy.json"},
			{AuthIndex: "disabled", Name: "traework-disabled.json", Disabled: true},
			{AuthIndex: "broken", Name: "traework-broken.json"},
		}, nil
	}
	keepaliveAccountGetFn = func(authIndex string) (*storedAuth, error) {
		sa, ok := accounts[authIndex]
		if !ok {
			return nil, errors.New("unknown auth index")
		}
		return sa, nil
	}
	var refreshed int32
	keepaliveRefreshFn = func(sa *storedAuth) error {
		atomic.AddInt32(&refreshed, 1)
		if sa.UID == "u-broken" {
			return errors.New("refresh rejected")
		}
		return nil
	}

	run := runKeepalive()
	if run.Refreshed != 1 || run.Skipped != 2 || run.Failed != 1 {
		t.Fatalf("run = %+v, want 1 refreshed / 2 skipped / 1 failed", run)
	}
	if n := atomic.LoadInt32(&refreshed); n != 2 {
		t.Fatalf("refresh calls = %d, want 2 (due + broken)", n)
	}
	if run.Detail == "" {
		t.Fatal("a failed rotation must leave a detail behind")
	}
	if _, ok := lastKeepaliveRun(); !ok {
		t.Fatal("the pass must be recorded for the status endpoint")
	}
}

// The status payload is what the operator reads to confirm the pass is armed.
func TestKeepaliveStatusReportsScheduleAndLastRun(t *testing.T) {
	previousEnabled := tokenKeepaliveEnabled()
	t.Cleanup(func() { setKeepaliveEnabled(previousEnabled) })
	setKeepaliveEnabled(true)

	status := keepaliveStatus()
	if status["enabled"] != true {
		t.Fatalf("enabled = %v", status["enabled"])
	}
	schedule, ok := status["schedule"].([]string)
	if !ok || len(schedule) != 1 || schedule[0] != "22:00" {
		t.Fatalf("schedule = %v, want [22:00]", status["schedule"])
	}
	if status["lead_hours"] != 24 {
		t.Fatalf("lead_hours = %v, want 24", status["lead_hours"])
	}
}
