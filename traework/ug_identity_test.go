package main

import (
	"os"
	"regexp"
	"testing"
)

// TestMain keeps the package's retry backoff out of the test run: the retry
// behaviour is what is under test, not the wall-clock wait.
func TestMain(m *testing.M) {
	claimRetryDelay = 0
	os.Exit(m.Run())
}

// The user-growth endpoints reject the UUID the OAuth login registers, and they
// reject it with the generic busy code — so the plugin must present a numeric
// device id there, stable for the same account.
func TestUGDeviceIDIsNumericAndStable(t *testing.T) {
	sa := &storedAuth{DeviceID: "60fb0f1b-39f2-4187-b04f-46a7ca528528", UID: "9123456789012345"}
	first := ugDeviceID(sa)
	if !regexp.MustCompile(`^[0-9]{13}$`).MatchString(first) {
		t.Fatalf("ugDeviceID = %q, want 13 digits", first)
	}
	if again := ugDeviceID(sa); again != first {
		t.Errorf("ugDeviceID is not stable: %q then %q", first, again)
	}
	other := ugDeviceID(&storedAuth{DeviceID: "11111111-2222-3333-4444-555555555555"})
	if other == first {
		t.Error("different devices must not share one id")
	}
	// No device id recorded: the uid still yields a usable, stable numeric id.
	fallback := ugDeviceID(&storedAuth{UID: "9123456789012345"})
	if !regexp.MustCompile(`^[0-9]{13}$`).MatchString(fallback) {
		t.Errorf("uid fallback = %q, want 13 digits", fallback)
	}
	if ugDeviceID(&storedAuth{UID: "9123456789012345"}) != fallback {
		t.Error("the uid fallback must be stable too")
	}
	if got := ugDeviceID(&storedAuth{}); got != "" {
		t.Errorf("empty account = %q, want no device id", got)
	}
}

func TestUGHeadersCarryTheNumericIdentity(t *testing.T) {
	sa := &storedAuth{AccessToken: "tok", DeviceID: "60fb0f1b-39f2-4187-b04f-46a7ca528528"}
	headers := ugHeaders(sa)
	if headers["X-User-Region"] != "CN" {
		t.Errorf("X-User-Region = %q, want CN", headers["X-User-Region"])
	}
	if headers["X-Device-Id"] != ugDeviceID(sa) {
		t.Errorf("X-Device-Id = %q, want the numeric id", headers["X-Device-Id"])
	}
	if headers["Authorization"] != "Cloud-IDE-JWT tok" {
		t.Errorf("Authorization = %q", headers["Authorization"])
	}
	// The IDE-only headers belong to the chat path, not here.
	for _, unwanted := range []string{"X-Ide-Token", "X-Machine-Id", "Request-Traffic-Type", "X-Cloudide-Token"} {
		if _, ok := headers[unwanted]; ok {
			t.Errorf("%s must not be sent to the user-growth endpoints", unwanted)
		}
	}
	if body := ugBody(); body["req_source"] != ugReqSourceLite {
		t.Errorf("body = %+v, want req_source %d", body, ugReqSourceLite)
	}
}

// A busy window that clears on the next attempt must not be reported as a
// failure: the claim is retried before the verdict is returned.
func TestClaimRetriesABusyWindow(t *testing.T) {
	resetBillingCache()
	stubCheckinHost(t, map[string]*storedAuth{"idx-1": {UID: "u1", AccessToken: "t"}})
	stubCheckinStatus(t, false, nil)

	var calls int32
	previous := checkinClaimFn
	checkinClaimFn = func(*storedAuth) map[string]any {
		if atomicAdd(&calls) <= 2 {
			return map[string]any{"success": false, "retryable": true, "message": "当前参与用户太多，请稍后再试"}
		}
		return map[string]any{"success": true, "message": "签到成功"}
	}
	t.Cleanup(func() { checkinClaimFn = previous })

	previousDelay := claimRetryDelay
	claimRetryDelay = 0
	t.Cleanup(func() { claimRetryDelay = previousDelay })

	out := claimCheckinForAccount("idx-1")
	if out["success"] != true {
		t.Fatalf("out = %+v, want the retry to succeed", out)
	}
	if n := loadInt32(&calls); n != 3 {
		t.Errorf("claim attempts = %d, want 2 busy + 1 success", n)
	}

	// A window that stays busy is reported as retryable, after the retries.
	calls = 0
	checkinClaimFn = func(*storedAuth) map[string]any {
		atomicAdd(&calls)
		return map[string]any{"success": false, "retryable": true, "message": "当前参与用户太多，请稍后再试"}
	}
	out = claimCheckinForAccount("idx-1")
	if out["retryable"] != true || out["success"] == true {
		t.Errorf("out = %+v, want it still reported as retryable", out)
	}
	if n := loadInt32(&calls); n != 3 {
		t.Errorf("claim attempts = %d, want 3 before giving up", n)
	}
}

func atomicAdd(counter *int32) int32 {
	*counter++
	return *counter
}

func loadInt32(counter *int32) int32 {
	return *counter
}
