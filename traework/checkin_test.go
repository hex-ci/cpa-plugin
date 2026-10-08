package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// stubCheckinHost makes the host look like it holds one file per given index and
// returns a restore func. The registry is read through the seam the handler uses.
func stubCheckinHost(t *testing.T, accounts map[string]*storedAuth) {
	t.Helper()
	list := make([]pluginapi.HostAuthFileEntry, 0, len(accounts))
	for index := range accounts {
		list = append(list, pluginapi.HostAuthFileEntry{AuthIndex: index, Name: "traework-" + index + ".json"})
	}
	previousList, previousGet := checkinHostAuthList, checkinHostAuthGet
	checkinHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) { return list, nil }
	checkinHostAuthGet = func(index string) (*storedAuth, *hostAuthPhysical, error) {
		sa, ok := accounts[index]
		if !ok {
			return nil, nil, errors.New("unknown auth index")
		}
		return sa, &hostAuthPhysical{AuthIndex: index, Name: "traework-" + index + ".json"}, nil
	}
	t.Cleanup(func() {
		checkinHostAuthList, checkinHostAuthGet = previousList, previousGet
	})
}

// stubClaim replaces the upstream write and counts it.
func stubClaim(t *testing.T, outcome map[string]any) *int32 {
	t.Helper()
	var calls int32
	previous := checkinClaimFn
	checkinClaimFn = func(*storedAuth) map[string]any {
		atomic.AddInt32(&calls, 1)
		return outcome
	}
	t.Cleanup(func() { checkinClaimFn = previous })
	return &calls
}

// stubCheckinStatus answers the read path the claim consults first.
func stubCheckinStatus(t *testing.T, checkedIn bool, credits *creditsView) *int32 {
	t.Helper()
	var calls int32
	previous := fetchCredentialsFn
	fetchCredentialsFn = func(*storedAuth) (*creditsView, *checkinView, error) {
		atomic.AddInt32(&calls, 1)
		return credits, &checkinView{Enabled: true, TodayCheckedIn: checkedIn, Credits: 100}, nil
	}
	t.Cleanup(func() { fetchCredentialsFn = previous })
	return &calls
}

func TestClaimOutcomeClassifiesBusinessCodes(t *testing.T) {
	if got := claimOutcome(checkinClaimResult{}, errors.New("connect: refused")); got["success"] != false {
		t.Errorf("transport failure = %+v, want a failure", got)
	}
	ok := claimOutcome(checkinClaimResult{Code: 0, Credits: 200}, nil)
	if ok["success"] != true || ok["credits"] != int64(200) {
		t.Errorf("code 0 = %+v, want success with the granted credits", ok)
	}

	busy := claimOutcome(checkinClaimResult{Code: checkinBusyCode, Message: "当前参与用户太多，请稍后再试"}, nil)
	if busy["success"] != false || busy["retryable"] != true {
		t.Errorf("9074 = %+v, want a retryable busy result", busy)
	}
	if busy["message"] != "当前参与用户太多，请稍后再试" {
		t.Errorf("busy message = %v, want the upstream text", busy["message"])
	}

	// A code with no message still has to say something usable, and must not be
	// mistaken for the retryable busy window.
	hard := claimOutcome(checkinClaimResult{Code: 4001}, nil)
	if hard["retryable"] == true {
		t.Errorf("4001 = %+v, want a non-retryable failure", hard)
	}
	if hard["message"] != "签到失败" {
		t.Errorf("message = %v, want the fallback text", hard["message"])
	}
	if blank := claimOutcome(checkinClaimResult{Code: checkinBusyCode}, nil); blank["message"] == "" {
		t.Error("a busy result must carry a message even when upstream sends none")
	}
}

// Today's bonus already taken means no write at all.
func TestClaimCheckinForAccountSkipsWhenTodayIsTaken(t *testing.T) {
	resetBillingCache()
	stubCheckinHost(t, map[string]*storedAuth{"idx-1": {UID: "u1", AccessToken: "t"}})
	calls := stubClaim(t, map[string]any{"success": true})
	stubCheckinStatus(t, true, nil)

	out := claimCheckinForAccount("idx-1")
	if out["already"] != true || out["success"] != true {
		t.Fatalf("out = %+v, want already-taken", out)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("upstream claims = %d, want none", n)
	}
}

// A successful claim moves the balance, so the snapshot must be read again for
// the panel.
func TestClaimCheckinForAccountRefreshesTheSnapshotAfterAClaim(t *testing.T) {
	resetBillingCache()
	stubCheckinHost(t, map[string]*storedAuth{"idx-1": {UID: "u1", AccessToken: "t"}})
	if n := stubClaim(t, map[string]any{"success": true, "message": "签到成功"}); atomic.LoadInt32(n) != 0 {
		t.Fatal("claim stub ran too early")
	}
	reads := stubCheckinStatus(t, false, &creditsView{TotalRemain: 700})

	out := claimCheckinForAccount("idx-1")
	if out["success"] != true {
		t.Fatalf("out = %+v, want success", out)
	}
	if out["credits"] == nil {
		t.Error("a claimed bonus must hand the panel the refreshed quota")
	}
	if n := atomic.LoadInt32(reads); n < 2 {
		t.Errorf("status reads = %d, want one before and one after the claim", n)
	}
}

func TestHandleCheckinClaimTargetsOneOrAll(t *testing.T) {
	resetBillingCache()
	stubCheckinHost(t, map[string]*storedAuth{
		"idx-a": {UID: "a", AccessToken: "t"},
		"idx-b": {UID: "b", AccessToken: "t"},
	})
	calls := stubClaim(t, map[string]any{"success": true})
	stubCheckinStatus(t, false, nil)

	single := handleCheckinClaim(pluginapi.ManagementRequest{Body: []byte(`{"auth_index":"idx-b"}`)})
	if summary := single["summary"].(map[string]any); summary["total"] != 1 || summary["claimed"] != 1 {
		t.Fatalf("single summary = %+v, want only idx-b", summary)
	}
	results := single["results"].([]map[string]any)
	if results[0]["auth_index"] != "idx-b" {
		t.Errorf("targeted %v, want idx-b", results[0]["auth_index"])
	}

	all := handleCheckinClaim(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if summary := all["summary"].(map[string]any); summary["total"] != 2 || summary["claimed"] != 2 {
		t.Fatalf("all summary = %+v, want both accounts", summary)
	}
	if n := atomic.LoadInt32(calls); n != 3 {
		t.Errorf("claims = %d, want 1 + 2", n)
	}

	missing := handleCheckinClaim(pluginapi.ManagementRequest{Body: []byte(`{"auth_index":"nope"}`)})
	if missing["error"] == nil {
		t.Errorf("unknown auth_index = %+v, want an error", missing)
	}
}

// The busy window is an upstream fact, not a plugin failure: the summary keeps
// it in its own bucket so the panel can say "try again later".
func TestHandleCheckinClaimReportsBusySeparately(t *testing.T) {
	resetBillingCache()
	stubCheckinHost(t, map[string]*storedAuth{"idx-a": {UID: "a", AccessToken: "t"}})
	stubClaim(t, map[string]any{"success": false, "retryable": true, "message": "当前参与用户太多，请稍后再试"})
	stubCheckinStatus(t, false, nil)

	payload := handleCheckinClaim(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	summary := payload["summary"].(map[string]any)
	if summary["busy"] != 1 || summary["fail"] != 0 || summary["claimed"] != 0 {
		t.Fatalf("summary = %+v, want the busy bucket filled and fail empty", summary)
	}
}

func TestHandleCheckinConfigTogglesAtRuntime(t *testing.T) {
	previous := checkinAutoEnabled()
	t.Cleanup(func() { setCheckinAuto(previous) })

	if out := handleCheckinConfig(pluginapi.ManagementRequest{Body: []byte(`{"enabled":false}`)}); out["checkin_auto"] != false {
		t.Fatalf("out = %+v, want the toggle off", out)
	}
	if checkinAutoEnabled() {
		t.Error("the handler must change the running value")
	}
	// A body without the field reports the current value instead of flipping it.
	if out := handleCheckinConfig(pluginapi.ManagementRequest{Body: []byte(`{}`)}); out["checkin_auto"] != false {
		t.Errorf("out = %+v, want the value unchanged", out)
	}
	if out := handleCheckinConfig(pluginapi.ManagementRequest{Body: []byte(`{"enabled":true}`)}); out["checkin_auto"] != true || out["persistent"] != false {
		t.Errorf("out = %+v, want the toggle on and flagged as runtime-only", out)
	}
}

func TestRunAutoCheckinRespectsTheToggle(t *testing.T) {
	resetBillingCache()
	stubCheckinHost(t, map[string]*storedAuth{"idx-a": {UID: "a", AccessToken: "t"}})
	calls := stubClaim(t, map[string]any{"success": true})
	stubCheckinStatus(t, false, nil)
	previous := checkinAutoEnabled()
	t.Cleanup(func() { setCheckinAuto(previous) })

	setCheckinAuto(false)
	runAutoCheckin()
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("claims = %d, want the automatic pass to stay quiet when off", n)
	}

	setCheckinAuto(true)
	runAutoCheckin()
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("claims = %d, want one account claimed", n)
	}
}

func TestCheckinScheduleWindowAndNextSlot(t *testing.T) {
	loc := time.Local
	for _, tc := range []struct {
		hour, minute int
		want         bool
	}{
		{9, 0, true}, {9, 59, true}, {10, 0, false}, {12, 0, false}, {21, 30, true}, {23, 0, false},
	} {
		now := time.Date(2026, 10, 8, tc.hour, tc.minute, 0, 0, loc)
		if got := inCheckinHour(now); got != tc.want {
			t.Errorf("inCheckinHour(%02d:%02d) = %t, want %t", tc.hour, tc.minute, got, tc.want)
		}
	}

	morning := time.Date(2026, 10, 8, 8, 0, 0, 0, loc)
	if got := nextCheckinTime(morning); got != time.Date(2026, 10, 8, 9, 0, 0, 0, loc) {
		t.Errorf("next slot from 08:00 = %v, want today 09:00", got)
	}
	night := time.Date(2026, 10, 8, 22, 0, 0, 0, loc)
	if got := nextCheckinTime(night); got != time.Date(2026, 10, 9, 9, 0, 0, 0, loc) {
		t.Errorf("next slot from 22:00 = %v, want tomorrow 09:00", got)
	}
}

// The lock is what keeps two browser tabs from claiming the same day twice.
func TestCheckinLocksAreScopedPerAccount(t *testing.T) {
	first, again := checkinLockFor("idx-a"), checkinLockFor("idx-a")
	if first != again {
		t.Error("the same account must share one lock")
	}
	if other := checkinLockFor("idx-b"); first == other {
		t.Error("different accounts must not block each other")
	}
}
