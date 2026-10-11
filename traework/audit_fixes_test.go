package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// YAML tags True/TRUE as !!bool too; those spellings used to fall through the
// literal "true" comparison and silently switch the feature off.
func TestConfigureAcceptsUppercaseYAMLBools(t *testing.T) {
	t.Cleanup(func() { _ = configureYAML(t, "management_key: k\n") })

	if err := configureYAML(t, "checkin_auto: True\n"); err != nil {
		t.Fatal(err)
	}
	if !checkinAutoEnabled() {
		t.Error("checkin_auto: True must enable the automatic claim")
	}
	if err := configureYAML(t, "checkin_auto: FALSE\n"); err != nil {
		t.Fatal(err)
	}
	if checkinAutoEnabled() {
		t.Error("checkin_auto: FALSE must disable the automatic claim")
	}
	if err := configureYAML(t, "desensitize: True\n"); err != nil {
		t.Fatal(err)
	}
	if !currentDesensitize().enabled {
		t.Error("desensitize: True must enable the term list")
	}
}

// The key was retired with the panel link it used to prefix; an operator config
// that still carries it must not be rejected.
func TestConfigureIgnoresTheRetiredPanelBaseURL(t *testing.T) {
	if err := configureYAML(t, "management_key: k\npanel_base_url: https://cpa.example.cn\n"); err != nil {
		t.Fatalf("a retired key must be ignored, not fatal: %v", err)
	}
	if got := loadString(loginCallbackOverride); got != "" {
		t.Errorf("login_callback_url = %q, want untouched", got)
	}
}

// proxy-url and proxy_url are the same setting; the underscored spelling used to
// be accepted by the parser and then dropped.
func TestConfigureAppliesUnderscoreProxyURL(t *testing.T) {
	t.Cleanup(func() { _ = configureYAML(t, "management_key: k\n") })

	if err := configureYAML(t, "proxy_url: socks5://127.0.0.1:1080\n"); err != nil {
		t.Fatal(err)
	}
	if got := currentProxyState().mode; got != proxyModeExplicit {
		t.Errorf("proxy mode = %v, want the configured proxy to be used", got)
	}
}

// Truncating an upstream error must not split a Chinese character in half.
func TestTruncateKeepsRuneBoundaries(t *testing.T) {
	const message = "签到失败：当前参与用户太多，请稍后再试"
	for cut := 1; cut <= len(message); cut++ {
		got := truncate(message, cut)
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(%d) produced invalid UTF-8: %q", cut, got)
		}
		if len(got) > cut {
			t.Fatalf("truncate(%d) returned %d bytes", cut, len(got))
		}
	}
}

// A snapshot older than the TTL is not a snapshot: the cards must not render
// numbers the upstream has long moved past.
func TestPeekCredentialsHonoursTheTTL(t *testing.T) {
	billingMu.Lock()
	billingCache = map[string]billingEntry{}
	billingCache["stale"] = billingEntry{credits: &creditsView{TotalRemain: 1}, at: time.Now().Add(-2 * billingTTL)}
	billingCache["fresh"] = billingEntry{credits: &creditsView{TotalRemain: 2}, at: time.Now()}
	billingMu.Unlock()
	t.Cleanup(func() {
		billingMu.Lock()
		billingCache = map[string]billingEntry{}
		billingMu.Unlock()
	})

	if credits, _ := peekCredentials("stale"); credits != nil {
		t.Errorf("a snapshot past its TTL must not be served: %+v", credits)
	}
	if credits, _ := peekCredentials("fresh"); credits == nil {
		t.Error("a snapshot inside its TTL must still be served")
	}
}

// A failed exchange spends the code. The panel polls the status every two
// seconds, so keeping the code would re-post a dead one to the upstream.
func TestAFailedRedeemIsNotRetriedByStatusPolls(t *testing.T) {
	stubAuthSave(t)
	exchanges := 0
	previous := exchangeAuthCodeFn
	exchangeAuthCodeFn = func(string, string, string, string, string) (*exchangeResult, error) {
		exchanges++
		return nil, errTestExchangeRejected
	}
	t.Cleanup(func() { exchangeAuthCodeFn = previous })

	ctx := &loginCtx{state: "state-poisoned", code: "spent", verifier: "v", expires: time.Now().Add(time.Minute)}
	// The first poll tries, the second and third must not reach upstream again.
	for i := 0; i < 3; i++ {
		status, message := ctx.progress("", nil)
		if status != pluginapi.AuthLoginStatusError {
			t.Fatalf("poll %d status = %v (%s), want a terminal error", i, status, message)
		}
		if !strings.Contains(message, "请重新发起") {
			t.Errorf("poll %d message = %q, want a hint to start over", i, message)
		}
	}
	if exchanges != 1 {
		t.Fatalf("exchange called %d times, want exactly one", exchanges)
	}
}

// Cancelling while the exchange is in flight must not leave the account behind.
func TestCancelDuringRedeemDropsTheAccount(t *testing.T) {
	saved := stubAuthSave(t)
	previous := exchangeAuthCodeFn
	var ctx *loginCtx
	exchangeAuthCodeFn = func(string, string, string, string, string) (*exchangeResult, error) {
		ctx.markCancelled()
		return &exchangeResult{
			Token:           fakeToken("9123456789012345", time.Now().Add(time.Hour)),
			RefreshToken:    "r",
			TokenExpireAt:   time.Now().Add(time.Hour).UnixMilli(),
			RefreshExpireAt: time.Now().Add(24 * time.Hour).UnixMilli(),
			ClientID:        oauthClientID,
		}, nil
	}
	t.Cleanup(func() { exchangeAuthCodeFn = previous })
	previousProfile := fetchProfileFn
	fetchProfileFn = func(*storedAuth) (string, string) { return "", "" }
	t.Cleanup(func() { fetchProfileFn = previousProfile })

	ctx = &loginCtx{state: "state-cancel", code: "code", verifier: "v", expires: time.Now().Add(time.Minute)}
	if status, _ := ctx.progress("", nil); status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %v, want a cancelled attempt to fail", status)
	}
	if len(*saved) != 0 {
		t.Fatalf("a cancelled login wrote %d auth file(s)", len(*saved))
	}
}

// Re-logging in from the panel must keep the settings the operator put on the
// existing record: the host replaces the whole file.
func TestPanelSaveKeepsExistingUserSettings(t *testing.T) {
	saved := stubAuthSave(t)
	previous := existingCarrierFn
	existingCarrierFn = func(*storedAuth) map[string]any {
		return map[string]any{"weight": float64(7), "prefix": "trae", "note": "我的备注"}
	}
	t.Cleanup(func() { existingCarrierFn = previous })

	sa := &storedAuth{UID: "9123456789012345", AccessToken: "t", RefreshToken: "r"}
	if _, err := saveAuthRecord(sa, nil); err != nil {
		t.Fatal(err)
	}
	if len(*saved) != 1 {
		t.Fatalf("saved %d records", len(*saved))
	}
	var doc map[string]any
	if err := json.Unmarshal((*saved)[0].payload, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["weight"] != float64(7) || doc["prefix"] != "trae" {
		t.Errorf("user settings were dropped: %+v", doc)
	}
	if doc["note"] != "我的备注" {
		t.Errorf("note = %v, want the operator's own text", doc["note"])
	}
}

// Backing off between retries must not hold the account lock: another claim on
// the same account has to get through while the first one waits.
func TestCheckinBackoffDoesNotHoldTheAccountLock(t *testing.T) {
	stubCheckinHost(t, map[string]*storedAuth{"idx": {UID: "9123456789012345", AccessToken: "t"}})
	stubCheckinStatus(t, false, nil)
	previousDelay := claimRetryDelay
	claimRetryDelay = 300 * time.Millisecond
	t.Cleanup(func() { claimRetryDelay = previousDelay })

	// The stub reports the moment each claim reaches upstream.
	claims := make(chan time.Time, 8)
	previousClaim := checkinClaimFn
	checkinClaimFn = func(*storedAuth) map[string]any {
		claims <- time.Now()
		return map[string]any{"success": false, "retryable": true, "message": "当前参与用户太多，请稍后再试"}
	}
	t.Cleanup(func() { checkinClaimFn = previousClaim })

	done := make(chan struct{}, 2)
	start := make(chan struct{})
	go func() { <-start; claimCheckinForAccount("idx"); done <- struct{}{} }()
	go func() { <-start; claimCheckinForAccount("idx"); done <- struct{}{} }()
	started := time.Now()
	close(start)

	// Both goroutines must reach the claim before the first backoff (300ms)
	// ends: that is only possible when the sleeping one is not holding the lock.
	for i := 0; i < 2; i++ {
		select {
		case at := <-claims:
			if waited := at.Sub(started); waited > 250*time.Millisecond {
				t.Errorf("claim %d only started after %v: the lock is held across the backoff", i+1, waited)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("claim %d never reached upstream", i+1)
		}
	}
	<-done
	<-done
}

var errTestExchangeRejected = &testError{"invalid_grant"}

type testError struct{ text string }

func (e *testError) Error() string { return e.text }
