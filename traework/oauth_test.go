package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPKCEChallengeIsS256OfVerifier(t *testing.T) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier) != 43 {
		t.Errorf("verifier length = %d, want 43 (32 random bytes, base64url)", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Errorf("challenge = %q, want S256(verifier) = %q", challenge, want)
	}
}

func TestAuthCodeFromQuery(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"authCodeInfo", "authCodeInfo=" + url.QueryEscape(`{"AuthCode":"abc123","ExpireAt":1}`), "abc123"},
		{"plain code", "code=xyz", "xyz"},
		{"snake", "auth_code=snake", "snake"},
		{"capital", "AuthCode=cap", "cap"},
		{"none", "state=only", ""},
		{"broken json", "authCodeInfo=" + url.QueryEscape(`{not json`), ""},
		{"lowercase key", "authCodeInfo=" + url.QueryEscape(`{"authCode":"low"}`), "low"},
		{"nested blob", "authCodeInfo=" + url.QueryEscape(`{"authCodeInfo":{"AuthCode":"nested"}}`), "nested"},
		{"double encoded", "authCodeInfo=" + url.QueryEscape(url.QueryEscape(`{"AuthCode":"twice"}`)), "twice"},
		{"non-string value", "authCodeInfo=" + url.QueryEscape(`{"AuthCode":42}`), ""},
	}
	for _, tc := range cases {
		values, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := authCodeFromQuery(values); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCallbackFailureDetection(t *testing.T) {
	values, _ := url.ParseQuery("error_code=1001&error_msg=" + url.QueryEscape("用户取消"))
	if got := callbackFailure(values); got != "用户取消" {
		t.Errorf("got %q, want the error_msg", got)
	}
	values, _ = url.ParseQuery("error_code=1001")
	if got := callbackFailure(values); !strings.Contains(got, "1001") {
		t.Errorf("got %q, want the error code surfaced", got)
	}
	values, _ = url.ParseQuery("error_code=0&authCodeInfo=" + url.QueryEscape(`{"AuthCode":"ok"}`))
	if got := callbackFailure(values); got != "" {
		t.Errorf("a successful callback must not report a failure, got %q", got)
	}
}

// The authorization page validates auth_callback_url: it must be loopback and
// pend /authorize, otherwise the browser lands on "登录失败".
func TestAuthorizeURLCarriesLoopbackAuthorizeCallback(t *testing.T) {
	callback := "http://127.0.0.1:38441/authorize"
	raw := buildAuthorizeURL(callback, "challenge-value", "trace-id", "machine-1", "device-1")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme+"://"+parsed.Host != "https://www.trae.cn" || parsed.Path != "/authorization" {
		t.Fatalf("unexpected authorize endpoint: %s", raw)
	}
	q := parsed.Query()
	for key, want := range map[string]string{
		"auth_callback_url":     callback,
		"code_challenge":        "challenge-value",
		"code_challenge_method": "S256",
		"client_id":             oauthClientID,
		"auth_from":             "solo",
		"login_trace_id":        "trace-id",
		"machine_id":            "machine-1",
		"device_id":             "device-1",
		"auth_type":             "local",
		"x_app_type":            appType,
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func decodeResult(t *testing.T, raw []byte, out any) {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope error: %+v", env.Error)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		t.Fatalf("bad result: %v", err)
	}
}

func fakeToken(uid string, exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{
		"data": map[string]any{"id": uid, "tenant_id": "tenant-1"},
		"exp":  exp.Unix(),
	})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return "header." + enc + ".signature"
}

type savedAuth struct {
	name    string
	payload []byte
}

// stubAuthSave captures the auth file a completed login writes: outside the
// gateway there is no host to save through.
func stubAuthSave(t *testing.T) *[]savedAuth {
	t.Helper()
	saved := &[]savedAuth{}
	original := authSaveFn
	authSaveFn = func(name string, payload []byte) (string, error) {
		*saved = append(*saved, savedAuth{name: name, payload: append([]byte(nil), payload...)})
		return name, nil
	}
	t.Cleanup(func() { authSaveFn = original })
	return saved
}

// authorizeURLOf reads the trae.cn authorization URL out of the start response.
// The response URL itself points at the panel's login page, which is where a
// browser that cannot reach the plugin's loopback port finishes the login.
func authorizeURLOf(t *testing.T, start pluginapi.AuthLoginStartResponse) string {
	t.Helper()
	raw, _ := start.Metadata["authorize_url"].(string)
	if strings.TrimSpace(raw) == "" {
		t.Fatalf("start response carries no authorize_url: %+v", start.Metadata)
	}
	return raw
}

func callbackURLOf(t *testing.T, start pluginapi.AuthLoginStartResponse) string {
	t.Helper()
	parsed, err := url.Parse(authorizeURLOf(t, start))
	if err != nil {
		t.Fatal(err)
	}
	target := parsed.Query().Get("auth_callback_url")
	if target == "" {
		t.Fatalf("authorize URL carries no auth_callback_url: %s", authorizeURLOf(t, start))
	}
	return target
}

func pollLoginState(t *testing.T, state string) pluginapi.AuthLoginPollResponse {
	t.Helper()
	raw, err := handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: state}))
	if err != nil {
		t.Fatal(err)
	}
	var poll pluginapi.AuthLoginPollResponse
	decodeResult(t, raw, &poll)
	return poll
}

// TestLoginStateMachine walks the whole flow the host drives: start -> poll
// (pending) -> browser callback -> poll (success), with upstream stubbed out.
func TestLoginStateMachine(t *testing.T) {
	originalExchange, originalProfile := exchangeAuthCodeFn, fetchProfileFn
	t.Cleanup(func() {
		exchangeAuthCodeFn, fetchProfileFn = originalExchange, originalProfile
	})
	saved := stubAuthSave(t)

	expires := time.Now().Add(14 * 24 * time.Hour)
	var seenCode, seenVerifier, seenPublicKey string
	exchanges := 0
	exchangeAuthCodeFn = func(code, verifier, publicKey, machineID, deviceID string) (*exchangeResult, error) {
		exchanges++
		seenCode, seenVerifier, seenPublicKey = code, verifier, publicKey
		if machineID == "" || deviceID == "" {
			t.Error("device identifiers must be sent")
		}
		return &exchangeResult{
			Token:           fakeToken("9123456789012345", expires),
			RefreshToken:    "refresh-value",
			TokenExpireAt:   expires.UnixMilli(),
			RefreshExpireAt: expires.Add(24 * time.Hour).UnixMilli(),
			ClientID:        oauthClientID,
		}, nil
	}
	fetchProfileFn = func(*storedAuth) (string, string) { return "Test User", "https://example.invalid/a.png" }

	startRaw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatal(err)
	}
	var start pluginapi.AuthLoginStartResponse
	decodeResult(t, startRaw, &start)
	if start.Provider != providerName || start.State == "" || start.URL == "" {
		t.Fatalf("bad start response: %+v", start)
	}
	if !start.ExpiresAt.After(time.Now()) {
		t.Errorf("ExpiresAt = %v, want a future deadline", start.ExpiresAt)
	}
	// The dialog shows the authorization link itself; the panel later adopts this
	// same attempt so a pasted code meets its own PKCE verifier.
	if !strings.Contains(start.URL, "https://www.trae.cn/authorization?") {
		t.Fatalf("start URL = %q, want the authorization link", start.URL)
	}
	if got := authorizeURLOf(t, start); got != start.URL {
		t.Errorf("authorize_url metadata = %q, want the same link", got)
	}
	callbackURL := callbackURLOf(t, start)
	if !strings.HasPrefix(callbackURL, "http://127.0.0.1:") || !strings.HasSuffix(callbackURL, "/authorize") {
		t.Fatalf("callback = %q, want loopback /authorize", callbackURL)
	}

	// Before the browser acts the host must see "pending", not an error.
	pollRaw, err := handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: start.State}))
	if err != nil {
		t.Fatal(err)
	}
	var poll pluginapi.AuthLoginPollResponse
	decodeResult(t, pollRaw, &poll)
	if poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending before the callback", poll.Status)
	}

	// The browser lands on the loopback listener.
	code := "ICqIi_test_code"
	page, err := http.Get(callbackURL + "?isRedirect=true&scope=solo&authCodeInfo=" +
		url.QueryEscape(fmt.Sprintf(`{"AuthCode":%q,"ExpireAt":%d}`, code, time.Now().Add(5*time.Minute).UnixMilli())))
	if err != nil {
		t.Fatalf("callback request failed: %v", err)
	}
	body, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body), "登录成功") {
		t.Fatalf("callback page = %d %s", page.StatusCode, truncate(string(body), 200))
	}

	pollRaw, err = handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: start.State}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, pollRaw, &poll)
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success (message %q)", poll.Status, poll.Message)
	}
	if seenCode != code {
		t.Errorf("exchange saw code %q, want %q", seenCode, code)
	}
	if seenVerifier == "" || !strings.Contains(seenPublicKey, "BEGIN PUBLIC KEY") {
		t.Errorf("exchange must receive the PKCE verifier and a device public key")
	}
	sa, err := parseStored(poll.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if sa.UID != "9123456789012345" || sa.Nickname != "Test User" || sa.RefreshToken != "refresh-value" {
		t.Fatalf("stored credential = %+v", sa)
	}
	if sa.ExpiresAt != expires.Unix() {
		t.Errorf("ExpiresAt = %d, want the millisecond field converted to seconds (%d)", sa.ExpiresAt, expires.Unix())
	}
	if sa.MachineID == "" || sa.DeviceID == "" {
		t.Error("device identifiers must be persisted with the credential")
	}

	// The credential is saved exactly once, and a repeated poll (the panel and
	// the dialog can both poll the same attempt) reports the same result
	// instead of redeeming the single-use code again.
	second := pollLoginState(t, start.State)
	if second.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("second poll = %+v, want the same success", second)
	}
	if exchanges != 1 {
		t.Errorf("exchanges = %d, want the code redeemed once", exchanges)
	}
	if len(*saved) != 1 {
		t.Fatalf("saved auth files = %d, want exactly one", len(*saved))
	}
	if (*saved)[0].name != providerName+"-9123456789012345.json" {
		t.Errorf("saved auth file = %q, want the uid-based name", (*saved)[0].name)
	}
	var onDisk map[string]any
	if err := json.Unmarshal((*saved)[0].payload, &onDisk); err != nil {
		t.Fatalf("saved auth file is not JSON: %v", err)
	}
	if onDisk["type"] != providerName || onDisk["refresh_token"] != "refresh-value" {
		t.Errorf("saved auth file = %+v, want the provider type and the refresh token", onDisk)
	}
}

func TestLoginCallbackReportsFailureToTheHost(t *testing.T) {
	startRaw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatal(err)
	}
	var start pluginapi.AuthLoginStartResponse
	decodeResult(t, startRaw, &start)
	target := callbackURLOf(t, start)

	resp, err := http.Get(target + "?error_code=1001&error_msg=" + url.QueryEscape("用户取消登录"))
	if err != nil {
		t.Fatalf("callback request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	pollRaw, err := handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: start.State}))
	if err != nil {
		t.Fatal(err)
	}
	var poll pluginapi.AuthLoginPollResponse
	decodeResult(t, pollRaw, &poll)
	if poll.Status != pluginapi.AuthLoginStatusError || !strings.Contains(poll.Message, "用户取消登录") {
		t.Fatalf("poll = %+v, want an error carrying the page's message", poll)
	}
}

func TestUnknownLoginStateIsReported(t *testing.T) {
	raw, err := handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: "does-not-exist"}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_state" {
		t.Fatalf("want an unknown_state error, got %s", raw)
	}
}

func TestParseAuthClaimsOwnership(t *testing.T) {
	foreign := []byte(`{"type":"workbuddy","accessToken":"x"}`)
	raw, err := handleParseAuth(mustJSON(pluginapi.AuthParseRequest{FileName: "workbuddy-1.json", RawJSON: foreign}))
	if err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.AuthParseResponse
	decodeResult(t, raw, &resp)
	if resp.Handled {
		t.Error("another provider's file must not be claimed")
	}

	sa := sampleStored(t)
	storage, _ := json.Marshal(sa)
	own := append([]byte(`{"type":"traework",`), storage[1:]...)
	raw, err = handleParseAuth(mustJSON(pluginapi.AuthParseRequest{FileName: "traework-1.json", RawJSON: own}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, &resp)
	if !resp.Handled {
		t.Fatal("our own file must be claimed")
	}
	if resp.Auth.ID != "" {
		t.Error("ID must stay empty so the host keys the record by file path")
	}
	if resp.Auth.FileName != "traework-1.json" {
		t.Errorf("FileName = %q, must be echoed back", resp.Auth.FileName)
	}

	// Type-less legacy file routed by name still loads.
	raw, err = handleParseAuth(mustJSON(pluginapi.AuthParseRequest{FileName: "traework-legacy.json", RawJSON: storage}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, &resp)
	if !resp.Handled {
		t.Error("a type-less file with our prefix must load")
	}

	// Type-less file with a foreign name is left alone.
	raw, err = handleParseAuth(mustJSON(pluginapi.AuthParseRequest{FileName: "random.json", RawJSON: storage}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, &resp)
	if resp.Handled {
		t.Error("a type-less foreign file must not be claimed by name")
	}
}

func TestRefreshUsesStoredRefreshToken(t *testing.T) {
	original := refreshTokenFn
	t.Cleanup(func() { refreshTokenFn = original })
	expires := time.Now().Add(14 * 24 * time.Hour)
	refreshTokenFn = func(sa *storedAuth) (*exchangeResult, error) {
		if sa.RefreshToken != "refresh-token-value" {
			t.Errorf("refresh used %q", sa.RefreshToken)
		}
		return &exchangeResult{
			Token:         fakeToken("9123456789012345", expires),
			TokenExpireAt: expires.UnixMilli(),
		}, nil
	}

	sa := sampleStored(t)
	storage, _ := json.Marshal(sa)
	raw, err := handleRefreshAuth(mustJSON(pluginapi.AuthRefreshRequest{AuthID: "id", StorageJSON: storage}))
	if err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.AuthRefreshResponse
	decodeResult(t, raw, &resp)
	refreshed, err := parseStored(resp.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken == sa.AccessToken {
		t.Error("access token was not rotated")
	}
	if refreshed.RefreshToken != sa.RefreshToken {
		t.Error("a refresh response without a new refresh token must keep the stored one")
	}
	if refreshed.Nickname != sa.Nickname {
		t.Error("profile fields must survive a refresh")
	}
	if !resp.NextRefreshAfter.After(time.Now()) {
		t.Errorf("NextRefreshAfter = %v, want a future time", resp.NextRefreshAfter)
	}
}

func TestRefreshWithoutRefreshTokenFailsClosed(t *testing.T) {
	sa := sampleStored(t)
	sa.RefreshToken = ""
	storage, _ := json.Marshal(sa)
	raw, err := handleRefreshAuth(mustJSON(pluginapi.AuthRefreshRequest{AuthID: "id", StorageJSON: storage}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "no_refresh_token" {
		t.Fatalf("want no_refresh_token, got %s", raw)
	}
}

func TestTimestampHelpers(t *testing.T) {
	if got := msToUnixSeconds(1893456000123); got != 1893456000 {
		t.Errorf("msToUnixSeconds = %d, want 1893456000", got)
	}
	if got := msToUnixSeconds(0); got != 0 {
		t.Errorf("msToUnixSeconds(0) = %d", got)
	}
	if !expiresSoon(0, time.Minute) {
		t.Error("an unknown expiry must be treated as needing a refresh")
	}
	if !expiresSoon(time.Now().Add(time.Minute).Unix(), 10*time.Minute) {
		t.Error("a token expiring inside the window must be refreshed")
	}
	if expiresSoon(time.Now().Add(2*time.Hour).Unix(), 10*time.Minute) {
		t.Error("a token outside the window must not be refreshed")
	}
	future := nextRefreshTime(time.Now().Add(24 * time.Hour).Unix())
	if !future.After(time.Now().Add(4 * time.Minute)) {
		t.Errorf("nextRefreshTime = %v, want at least the 5 minute floor", future)
	}
	if got := nextRefreshTime(0); !got.After(time.Now()) {
		t.Errorf("nextRefreshTime(0) = %v, want a future fallback", got)
	}
}

func TestAccountIdentityFromToken(t *testing.T) {
	token := fakeToken("uid-1", time.Now().Add(time.Hour))
	uid, tenant := accountIdentityFromToken(token)
	if uid != "uid-1" || tenant != "tenant-1" {
		t.Errorf("got uid=%q tenant=%q", uid, tenant)
	}
	if uid, tenant := accountIdentityFromToken("not-a-jwt"); uid != "" || tenant != "" {
		t.Error("a malformed token must yield empty claims, not a panic")
	}
	if exp := tokenExpiry(token); exp == 0 {
		t.Error("exp claim must be readable")
	}
}

// A completed browser login must survive a late poll: the code is still
// redeemable for its own 10-minute window, so a lapsed listener must not throw
// away a credential the user already authorised.
func TestCapturedCodeSurvivesALapsedLoginWindow(t *testing.T) {
	originalExchange, originalProfile := exchangeAuthCodeFn, fetchProfileFn
	t.Cleanup(func() {
		exchangeAuthCodeFn, fetchProfileFn = originalExchange, originalProfile
	})
	exchangeAuthCodeFn = func(code, verifier, publicKey, machineID, deviceID string) (*exchangeResult, error) {
		return &exchangeResult{
			Token:         fakeToken("9123456789012345", time.Now().Add(14*24*time.Hour)),
			RefreshToken:  "refresh-after-grace",
			TokenExpireAt: time.Now().Add(14 * 24 * time.Hour).UnixMilli(),
		}, nil
	}
	fetchProfileFn = func(*storedAuth) (string, string) { return "", "" }
	stubAuthSave(t)

	startRaw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatal(err)
	}
	var start pluginapi.AuthLoginStartResponse
	decodeResult(t, startRaw, &start)
	resp, err := http.Get(callbackURLOf(t, start) + "?code=late-code")
	if err != nil {
		t.Fatalf("callback request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// The browser window lapses before the host polls.
	value, ok := loginStates.Load(start.State)
	if !ok {
		t.Fatal("login state disappeared")
	}
	ctx := value.(*loginCtx)
	ctx.mu.Lock()
	ctx.expires = time.Now().Add(-time.Minute)
	ctx.mu.Unlock()

	pollRaw, err := handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: start.State}))
	if err != nil {
		t.Fatal(err)
	}
	var poll pluginapi.AuthLoginPollResponse
	decodeResult(t, pollRaw, &poll)
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q (message %q), want success from the captured code", poll.Status, poll.Message)
	}
	sa, err := parseStored(poll.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if sa.RefreshToken != "refresh-after-grace" {
		t.Fatalf("credential not stored: %+v", sa)
	}
}

// Without a captured code a lapsed window is still a timeout.
func TestBareLoginTimeoutIsReported(t *testing.T) {
	startRaw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatal(err)
	}
	var start pluginapi.AuthLoginStartResponse
	decodeResult(t, startRaw, &start)
	value, ok := loginStates.Load(start.State)
	if !ok {
		t.Fatal("login state disappeared")
	}
	ctx := value.(*loginCtx)
	ctx.mu.Lock()
	ctx.expires = time.Now().Add(-time.Minute)
	ctx.mu.Unlock()

	pollRaw, err := handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: start.State}))
	if err != nil {
		t.Fatal(err)
	}
	var poll pluginapi.AuthLoginPollResponse
	decodeResult(t, pollRaw, &poll)
	if poll.Status != pluginapi.AuthLoginStatusError || !strings.Contains(poll.Message, "超时") {
		t.Fatalf("poll = %+v, want a timeout error", poll)
	}
	// The attempt stays queryable so the panel keeps reporting the same reason;
	// its listener is released and the janitor sweeps the record.
	if _, still := loginStates.Load(start.State); !still {
		t.Fatal("a timed-out attempt must stay queryable for the caller")
	}
	ctx.mu.Lock()
	listenerClosed := ctx.server == nil
	ctx.mu.Unlock()
	if !listenerClosed {
		t.Error("a timed-out attempt must release its loopback listener")
	}
	if _, again := ctx.progress("", nil); again != poll.Message {
		t.Errorf("repeated status = %q, want the same timeout message", again)
	}
}

// The device key pair is part of the credential: without it no refresh can pass
// the server's device check.
func TestLoginStoresTheDeviceKeyPair(t *testing.T) {
	originalExchange, originalProfile := exchangeAuthCodeFn, fetchProfileFn
	t.Cleanup(func() {
		exchangeAuthCodeFn, fetchProfileFn = originalExchange, originalProfile
	})
	exchangeAuthCodeFn = func(code, verifier, publicKey, machineID, deviceID string) (*exchangeResult, error) {
		if !strings.Contains(publicKey, "BEGIN PUBLIC KEY") {
			t.Errorf("the exchange must register a public key, got %q", truncate(publicKey, 40))
		}
		return &exchangeResult{
			Token:         fakeToken("uid-key", time.Now().Add(14*24*time.Hour)),
			RefreshToken:  "refresh-key",
			TokenExpireAt: time.Now().Add(14 * 24 * time.Hour).UnixMilli(),
		}, nil
	}
	fetchProfileFn = func(*storedAuth) (string, string) { return "", "" }
	stubAuthSave(t)

	startRaw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatal(err)
	}
	var start pluginapi.AuthLoginStartResponse
	decodeResult(t, startRaw, &start)
	resp, err := http.Get(callbackURLOf(t, start) + "?code=key-code")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	pollRaw, err := handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: start.State}))
	if err != nil {
		t.Fatal(err)
	}
	var poll pluginapi.AuthLoginPollResponse
	decodeResult(t, pollRaw, &poll)
	sa, err := parseStored(poll.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sa.PrivateKey, "BEGIN PRIVATE KEY") || !strings.Contains(sa.PublicKey, "BEGIN PUBLIC KEY") {
		t.Fatalf("credential must carry both key halves: private=%d public=%d bytes", len(sa.PrivateKey), len(sa.PublicKey))
	}
	if _, err := newDeviceProof(sa.PrivateKey, oauthClientID, sa.RefreshToken); err != nil {
		t.Fatalf("the stored private key must be usable for a refresh proof: %v", err)
	}
}

// A login must reuse the device identity an earlier one registered: the host
// keeps existing auth-file fields, so a fresh key pair would not match the key
// the server bound the refresh token to.
func TestLoginReusesRegisteredDeviceIdentity(t *testing.T) {
	pair, err := newDeviceKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	original := deviceIdentityLookup
	deviceIdentityLookup = func() *storedDeviceIdentity {
		return &storedDeviceIdentity{
			privatePEM: pair.privatePEM,
			machineID:  "machine-from-record",
			deviceID:   "device-from-record",
		}
	}
	t.Cleanup(func() { deviceIdentityLookup = original })
	stubAuthSave(t)

	startRaw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatal(err)
	}
	var start pluginapi.AuthLoginStartResponse
	decodeResult(t, startRaw, &start)
	parsed, err := url.Parse(authorizeURLOf(t, start))
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("machine_id") != "machine-from-record" || q.Get("device_id") != "device-from-record" {
		t.Fatalf("authorize URL must carry the stored identity, got machine=%q device=%q",
			q.Get("machine_id"), q.Get("device_id"))
	}

	// The stored private key is the one that will sign, and the exchange must
	// register the public half derived from it.
	originalExchange := exchangeAuthCodeFn
	exchangeAuthCodeFn = func(code, verifier, publicKey, machineID, deviceID string) (*exchangeResult, error) {
		derived, err := publicKeyFromPrivate(pair.privatePEM)
		if err != nil {
			t.Fatal(err)
		}
		if publicKey != derived {
			t.Errorf("registered public key must be derived from the signing key")
		}
		if machineID != "machine-from-record" || deviceID != "device-from-record" {
			t.Errorf("exchange saw machine=%q device=%q", machineID, deviceID)
		}
		return &exchangeResult{Token: fakeToken("uid-reuse", time.Now().Add(time.Hour)), RefreshToken: "r", TokenExpireAt: time.Now().Add(time.Hour).UnixMilli()}, nil
	}
	t.Cleanup(func() { exchangeAuthCodeFn = originalExchange })

	callback, _ := url.Parse(q.Get("auth_callback_url"))
	resp, err := http.Get(callback.String() + "?code=reuse-code")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	pollRaw, err := handlePollLogin(mustJSON(pluginapi.AuthLoginPollRequest{Provider: providerName, State: start.State}))
	if err != nil {
		t.Fatal(err)
	}
	var poll pluginapi.AuthLoginPollResponse
	decodeResult(t, pollRaw, &poll)
	sa, err := parseStored(poll.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if sa.PrivateKey != pair.privatePEM {
		t.Error("the stored credential must keep the reused private key")
	}
}

// The authorization page insists on a loopback callback, so the plugin's own
// listener is the only place the code can land; the relay hands it to the host
// so the host stays the owner of the handoff (its standard callback route).
func TestRelayToHostSendsStateAndCode(t *testing.T) {
	type received struct {
		state string
		code  string
		path  string
	}
	got := make(chan received, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		got <- received{state: query.Get("state"), code: query.Get("code"), path: r.URL.Path}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := &loginCtx{state: "state-abc", hostCallback: server.URL + "/v0/management/oauth-callback"}
	relayToHost(ctx, "code-xyz")

	select {
	case received := <-got:
		if received.state != "state-abc" || received.code != "code-xyz" {
			t.Errorf("relayed state/code = %q/%q", received.state, received.code)
		}
		if received.path != "/v0/management/oauth-callback" {
			t.Errorf("relayed path = %q", received.path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the host callback never received the relay")
	}
}

func TestRelayToHostWithoutTargetIsANoop(t *testing.T) {
	relayToHost(&loginCtx{state: "s"}, "code") // no host callback: must not panic
}

// The host writes .oauth-<provider>-<state>.oauth when the code reaches its own
// callback endpoint; polling reads that file so both delivery paths converge.
func TestHandoffCodeReadsHostFile(t *testing.T) {
	dir := t.TempDir()
	state := "state-file"
	payload := `{"code":"from-host","state":"` + state + `","error":""}`
	if err := os.WriteFile(filepath.Join(dir, ".oauth-"+providerName+"-"+state+".oauth"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	code, failure := handoffCode(dir, state)
	if code != "from-host" || failure != "" {
		t.Errorf("handoffCode = %q/%q, want from-host and no failure", code, failure)
	}
	if code, failure := handoffCode(dir, "other-state"); code != "" || failure != "" {
		t.Errorf("a foreign state must not read another attempt: %q/%q", code, failure)
	}
	if err := os.WriteFile(filepath.Join(dir, ".oauth-"+providerName+"-"+state+".oauth"), []byte(`{"error":"access_denied"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, failure := handoffCode(dir, state); failure != "access_denied" {
		t.Errorf("failure = %q, want access_denied", failure)
	}
	if code, failure := handoffCode("", state); code != "" || failure != "" {
		t.Errorf("an empty auth dir must be a no-op: %q/%q", code, failure)
	}
}

// The callback listener is not configurable any more: it takes whatever loopback
// port is free, so two concurrent logins never fight over a port.
func TestListenCallbackBindsATemporaryLoopbackPort(t *testing.T) {
	first, firstPort, err := listenCallback()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if firstPort <= 0 {
		t.Fatalf("port = %d, want a usable loopback port", firstPort)
	}
	if addr := first.Addr().(*net.TCPAddr); !addr.IP.IsLoopback() {
		t.Errorf("listener bound %s, want loopback only", addr.IP)
	}

	second, secondPort, err := listenCallback()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if secondPort == firstPort {
		t.Errorf("two attempts shared port %d", firstPort)
	}
}

/* ---------- 面板登录（唯一入口） ---------- */

// The panel asks which attempt is pending and adopts it: the authorization code
// belongs to that attempt's PKCE verifier, so a second attempt could not redeem it.
func TestPanelLoginCurrentAdoptsThePendingAttempt(t *testing.T) {
	stubPanelExchange(t, "adopted-code", nil)
	stubAuthSave(t)

	if got := panelLoginCurrent(); got["status"] != "none" {
		t.Fatalf("no attempt should be pending yet, got %+v", got)
	}

	state, _ := panelAttempt(t)
	current := panelLoginCurrent()
	if current["status"] != string(pluginapi.AuthLoginStatusPending) {
		t.Fatalf("current = %+v, want the pending attempt", current)
	}
	if current["state"] != state {
		t.Errorf("current state = %v, want %q", current["state"], state)
	}

	// Completing it through the loopback callback clears "current".
	resp, err := http.Get(callbackFromAuthorize(t, current["authorize_url"].(string)) + "?code=adopted-code")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if payload, err := panelLoginStatus(state); err != nil || payload["status"] != string(pluginapi.AuthLoginStatusSuccess) {
		t.Fatalf("status = %+v (err %v)", payload, err)
	}
	if got := panelLoginCurrent(); got["status"] != "none" {
		t.Errorf("a completed attempt must not stay current: %+v", got)
	}
}

func TestAuthCodeFromInputShapes(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"callback url with authCodeInfo", "http://127.0.0.1:38471/authorize?isRedirect=true&authCodeInfo=" + url.QueryEscape(`{"AuthCode":"info-code","ExpireAt":1}`), "info-code"},
		{"callback url with code", "http://127.0.0.1:38471/authorize?code=plain-code&state=s-1", "plain-code"},
		{"code in the fragment", "http://127.0.0.1:38471/authorize#code=frag-code", "frag-code"},
		{"authCodeInfo json alone", `{"AuthCode":"json-code"}`, "json-code"},
		{"bare auth code", "ICqIiXk7qWbC2z1sT_9a-aa", "ICqIiXk7qWbC2z1sT_9a-aa"},
		{"prose", "我把授权页面的地址复制过来了", ""},
		{"empty", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, failure := authCodeFromInput(tc.input)
			if failure != "" {
				t.Fatalf("unexpected failure %q", failure)
			}
			if got != tc.want {
				t.Errorf("code = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAuthCodeFromInputReportsPageFailures(t *testing.T) {
	input := "http://127.0.0.1:38471/authorize?error_code=1001&error_msg=" + url.QueryEscape("用户取消登录")
	code, failure := authCodeFromInput(input)
	if code != "" {
		t.Errorf("code = %q, want none", code)
	}
	if !strings.Contains(failure, "用户取消登录") {
		t.Errorf("failure = %q, want the page's message", failure)
	}
}

func stubPanelExchange(t *testing.T, wantCode string, seen *string) {
	t.Helper()
	originalExchange, originalProfile := exchangeAuthCodeFn, fetchProfileFn
	t.Cleanup(func() { exchangeAuthCodeFn, fetchProfileFn = originalExchange, originalProfile })
	exchangeAuthCodeFn = func(code, verifier, publicKey, machineID, deviceID string) (*exchangeResult, error) {
		if seen != nil {
			*seen = code
		}
		if wantCode != "" && code != wantCode {
			t.Errorf("exchange saw code %q, want %q", code, wantCode)
		}
		return &exchangeResult{
			Token:         fakeToken("uid-panel", time.Now().Add(time.Hour)),
			RefreshToken:  "refresh-panel",
			TokenExpireAt: time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}
	fetchProfileFn = func(*storedAuth) (string, string) { return "Test User", "" }
}

func panelAttempt(t *testing.T) (string, string) {
	t.Helper()
	payload, err := panelLoginStart()
	if err != nil {
		t.Fatal(err)
	}
	state, _ := payload["state"].(string)
	authorize, _ := payload["authorize_url"].(string)
	if state == "" || authorize == "" {
		t.Fatalf("panel login start = %+v", payload)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusPending) {
		t.Fatalf("panel login status = %v, want pending", payload["status"])
	}
	return state, authorize
}

// The panel drives a login with no host session behind it: the loopback capture
// completes it and the plugin writes the auth file itself.
func TestPanelLoginCompletesThroughTheLoopbackAndSaves(t *testing.T) {
	stubPanelExchange(t, "panel-code", nil)
	saved := stubAuthSave(t)

	state, authorize := panelAttempt(t)
	callback := callbackFromAuthorize(t, authorize)
	resp, err := http.Get(callback + "?authCodeInfo=" + url.QueryEscape(`{"AuthCode":"panel-code"}`))
	if err != nil {
		t.Fatalf("callback request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	payload, err := panelLoginStatus(state)
	if err != nil {
		t.Fatal(err)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusSuccess) {
		t.Fatalf("panel status = %+v, want success", payload)
	}
	if payload["file_name"] != providerName+"-uid-panel.json" {
		t.Errorf("file_name = %v, want the uid-based auth file", payload["file_name"])
	}
	if len(*saved) != 1 {
		t.Fatalf("saved auth files = %d, want one", len(*saved))
	}
	// Polling again reports the same login rather than redeeming twice.
	again, err := panelLoginStatus(state)
	if err != nil {
		t.Fatal(err)
	}
	if again["status"] != string(pluginapi.AuthLoginStatusSuccess) || len(*saved) != 1 {
		t.Fatalf("repeated status = %+v with %d saved files", again, len(*saved))
	}
}

func TestPanelLoginSubmitAcceptsEveryPasteShape(t *testing.T) {
	cases := []struct {
		name  string
		input string
		code  string
	}{
		{"callback url", "http://127.0.0.1:38471/authorize?code=pasted-url-code&state=s", "pasted-url-code"},
		{"authCodeInfo json", `{"AuthCode":"pasted-json-code"}`, "pasted-json-code"},
		{"bare code", "ICqIiPastedBareCode123", "ICqIiPastedBareCode123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			stubPanelExchange(t, tc.code, &seen)
			stubAuthSave(t)
			state, _ := panelAttempt(t)

			payload, err := panelLoginSubmit(state, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if payload["status"] != string(pluginapi.AuthLoginStatusSuccess) {
				t.Fatalf("panel submit = %+v, want success", payload)
			}
			if seen != tc.code {
				t.Errorf("exchange saw %q, want %q", seen, tc.code)
			}
		})
	}
}

func TestPanelLoginSubmitKeepsTheAttemptOnUnusableInput(t *testing.T) {
	stubPanelExchange(t, "later-code", nil)
	stubAuthSave(t)
	state, _ := panelAttempt(t)

	payload, err := panelLoginSubmit(state, "这个不是我该粘的东西")
	if err != nil {
		t.Fatal(err)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusPending) {
		t.Fatalf("panel submit = %+v, want the attempt to stay pending", payload)
	}
	if !strings.Contains(payload["message"].(string), "没有识别到") {
		t.Errorf("message = %v, want a hint about what to paste", payload["message"])
	}
	// A usable paste afterwards still completes it.
	done, err := panelLoginSubmit(state, "later-code")
	if err != nil {
		t.Fatal(err)
	}
	if done["status"] != string(pluginapi.AuthLoginStatusSuccess) {
		t.Fatalf("panel submit after a bad paste = %+v", done)
	}
}

func TestPanelLoginSubmitEndsTheAttemptOnAPageFailure(t *testing.T) {
	state, _ := panelAttempt(t)
	input := "http://127.0.0.1:38471/authorize?error_code=1001&error_msg=" + url.QueryEscape("用户取消登录")
	payload, err := panelLoginSubmit(state, input)
	if err != nil {
		t.Fatal(err)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusError) {
		t.Fatalf("panel submit = %+v, want an error status", payload)
	}
	if !strings.Contains(payload["message"].(string), "用户取消登录") {
		t.Errorf("message = %v, want the page's reason", payload["message"])
	}
}

func TestPanelLoginCancelAndUnknownState(t *testing.T) {
	state, _ := panelAttempt(t)
	if _, err := panelLoginCancel(state); err != nil {
		t.Fatal(err)
	}
	if _, still := loginStates.Load(state); still {
		t.Error("a cancelled attempt must be dropped")
	}
	if _, err := panelLoginStatus(state); err == nil {
		t.Error("a cancelled attempt must not report a status")
	}
	if _, err := panelLoginStatus(""); err == nil {
		t.Error("a missing state must be refused")
	}
}

func callbackFromAuthorize(t *testing.T, authorize string) string {
	t.Helper()
	parsed, err := url.Parse(authorize)
	if err != nil {
		t.Fatal(err)
	}
	callback := parsed.Query().Get("auth_callback_url")
	if callback == "" {
		t.Fatalf("authorize URL carries no callback: %s", authorize)
	}
	return callback
}

func TestLoginCallbackURLIsConfigurable(t *testing.T) {
	t.Cleanup(func() { loginCallbackOverride.Store("") })

	loginCallbackOverride.Store("")
	if got, want := loginCallbackURL(38471), "http://127.0.0.1:38471/authorize"; got != want {
		t.Errorf("default callback = %q, want %q", got, want)
	}

	loginCallbackOverride.Store("https://cpa.example.cn/authorize")
	if got := loginCallbackURL(38471); got != "https://cpa.example.cn/authorize" {
		t.Errorf("configured callback = %q", got)
	}

	// The authorization URL must carry it.
	ctx, err := startLoginFlow("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx.close()
		loginStates.Delete(ctx.state)
		loginCallbackOverride.Store("")
	})
	if !strings.Contains(ctx.authorizeURL, url.QueryEscape("https://cpa.example.cn/authorize")) {
		t.Errorf("authorize URL must carry the configured callback: %s", truncate(ctx.authorizeURL, 120))
	}
}
