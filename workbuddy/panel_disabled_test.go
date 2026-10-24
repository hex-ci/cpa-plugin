package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// A disabled credential cannot answer the billing endpoint: the call comes back
// 401, and the dashboard used to hand that 401 to the panel as an account error,
// so a "已禁用" card also showed "错误 / 请求失败". The dashboard must skip the
// upstream query and surface the persisted disable reason instead.
func TestDashboardSkipsCreditsForDisabledAccount(t *testing.T) {
	oldList, oldGet, oldDetails := panelHostAuthList, panelHostAuthGetBundle, panelAccountDetails
	t.Cleanup(func() {
		panelHostAuthList, panelHostAuthGetBundle, panelAccountDetails = oldList, oldGet, oldDetails
	})

	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			AuthIndex: "idx-1",
			ID:        "workbuddy-dead.json",
			Name:      "workbuddy-dead.json",
			Disabled:  true,
		}}, nil
	}
	panelHostAuthGetBundle = func(string) (*storedAuth, *hostAuthPhysical, error) {
		return &storedAuth{}, &hostAuthPhysical{
			Name:     "workbuddy-dead.json",
			Disabled: true,
			JSON:     []byte(`{"type":"workbuddy","disabled":true,"note":"Session revoked (401): re-login required"}`),
		}, nil
	}
	detailsCalls := 0
	panelAccountDetails = func(string, *storedAuth, bool, string) (string, *checkinSummary, *creditsSummary, []string) {
		detailsCalls++
		return "", nil, nil, []string{"upstream 401: <html>401 Authorization Required</html>"}
	}

	dash := buildDashboardExWithCallback(true, true, "")
	if detailsCalls != 0 {
		t.Fatalf("credit query ran %d times for a disabled account, want 0", detailsCalls)
	}
	accounts, _ := dash["accounts"].([]wbAccount)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v", dash["accounts"])
	}
	got := accounts[0]
	if !got.Disabled {
		t.Fatal("account should report disabled")
	}
	if got.Error != "" {
		t.Fatalf("disabled account error = %q, want empty", got.Error)
	}
	if !strings.Contains(got.Note, "re-login required") {
		t.Fatalf("note = %q, want the persisted disable reason", got.Note)
	}
	if got.Credits != nil {
		t.Fatalf("credits = %#v, want nil when the balance was never fetched", got.Credits)
	}
}

// Every writer rebuilds the auth file from scratch, so the note is the one field
// that can silently lose a fault reason. A disabled account whose note says
// "re-login required" must keep that note even when a credits pass wants to
// write its own wording.
func TestBuildAuthFileJSONKeepsFaultNote(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "t"}, Account: storedAccount{UID: "u1"}}
	physical := []byte(`{"type":"workbuddy","disabled":true,"note":"Session revoked (401): re-login required"}`)

	raw, err := buildAuthFileJSON(physical, sa, true, "CN · 积分未知", nil)
	if err != nil {
		t.Fatalf("buildAuthFileJSON: %v", err)
	}
	if !strings.Contains(string(raw), "re-login required") {
		t.Fatalf("fault note lost: %s", raw)
	}

	// Re-enabling drops the fault note: the account works again.
	raw, err = buildAuthFileJSON(physical, sa, false, "CN · 可用 100", nil)
	if err != nil {
		t.Fatalf("buildAuthFileJSON: %v", err)
	}
	if strings.Contains(string(raw), "re-login required") {
		t.Fatalf("fault note survived re-enable: %s", raw)
	}
}

// Check-in is an authenticated call: a disabled credential would answer 401 and
// the panel reported it as "签到失败". The skip is answered without a host call.
func TestCheckinSkipsDisabledAccount(t *testing.T) {
	out := checkinOneAccountWithCallback(pluginapi.HostAuthFileEntry{
		AuthIndex: "idx-dead",
		Name:      "workbuddy-dead.json",
		Label:     "赵畅",
		Disabled:  true,
	}, "")
	if out["error"] != nil {
		t.Fatalf("disabled check-in returned error %v", out["error"])
	}
	if out["reason"] != "disabled" || out["skipped"] != true {
		t.Fatalf("result = %#v, want a skipped/disabled answer", out)
	}
	if out["nickname"] != "赵畅" {
		t.Fatalf("nickname = %v, want the file label", out["nickname"])
	}
}
