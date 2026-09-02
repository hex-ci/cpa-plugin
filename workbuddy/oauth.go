// oauth.go implements the AuthProvider login flow: browser-driven OAuth via
// CodeBuddy's login endpoints (CN and Global), login state polling, and token
// refresh. Each login flow gets an isolated cookie jar so multi-account flows
// never cross-contaminate session state.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type authRefreshRequestWire struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// newLoginClient builds an isolated client with its own cookie jar and binds it
// to the routing snapshot active when the login starts.
func newLoginClient() (*http.Client, error) {
	state := currentProxyState()
	if state.mode == proxyModeBlocked || state.mode == proxyModeExplicit && state.client == nil {
		return nil, blockedProxyError()
	}
	transport := sharedHTTPClient().Transport
	if state.mode == proxyModeExplicit {
		transport = state.client.Transport
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     transport,
		Jar:           jar,
		CheckRedirect: rejectHTTPRedirect,
	}, nil
}

func loginClientForCurrentRouting(lc *loginCtx) (*http.Client, error) {
	if lc == nil || lc.client == nil {
		return nil, fmt.Errorf("login client unavailable")
	}
	routing := currentProxyState()
	if routing.mode == proxyModeBlocked || routing.mode == proxyModeExplicit && routing.client == nil {
		return nil, blockedProxyError()
	}
	client := *lc.client
	client.CheckRedirect = rejectHTTPRedirect
	if routing.mode == proxyModeExplicit {
		client.Transport = routing.client.Transport
	}
	return &client, nil
}

// doJSON sends method to fullURL with the given headers, parses the {code,msg,data}
// envelope, and returns the inner data payload. httpStatus is the upstream code.
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	return doJSONRequest(client, req)
}

func doJSONRequest(client *http.Client, req *http.Request) (json.RawMessage, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		// Redirects: Go's client follows them for GET, but a 3xx that lands
		// here (e.g. POST 307/308 not re-sent, or a new upstream gateway) would
		// otherwise surface as a misleading JSON "parse failed".
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, resp.StatusCode, nil
}

type oauthRequestProfile struct {
	mode      string
	stateURL  string
	userAgent string
	origin    string
}

func oauthProfileForMode(mode string) oauthRequestProfile {
	if mode == oauthClientModeWorkBuddy {
		return oauthRequestProfile{
			mode:      oauthClientModeWorkBuddy,
			stateURL:  upstreamBaseCN + "/v2/plugin/auth/state?platform=workbuddy",
			userAgent: "WorkBuddy/5.3.14 WorkBuddy/5.3.14 CLI/2.115.0",
			origin:    "https://www.workbuddy.cn",
		}
	}
	return oauthRequestProfile{
		mode:      oauthClientModeCLI,
		stateURL:  endpointAuthState,
		userAgent: clientUA,
		origin:    originReferer,
	}
}

func applyOAuthProfileHeaders(req *http.Request, profile oauthRequestProfile) {
	commonHeaders(req)
	if profile.mode != oauthClientModeWorkBuddy {
		return
	}
	req.Header.Set("User-Agent", profile.userAgent)
	req.Header.Set("Origin", profile.origin)
	req.Header.Set("Referer", profile.origin+"/")
}

func applyAnonymousOAuthHeaders(req *http.Request, profile oauthRequestProfile) {
	applyOAuthProfileHeaders(req, profile)
	if profile.mode != oauthClientModeWorkBuddy {
		return
	}
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")
}

func buildAuthStateRequest(profile oauthRequestProfile) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, profile.stateURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	applyAnonymousOAuthHeaders(req, profile)
	return req, nil
}

func buildAuthTokenRequest(profile oauthRequestProfile, state string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, endpointAuthToken+state, nil)
	if err != nil {
		return nil, err
	}
	applyAnonymousOAuthHeaders(req, profile)
	return req, nil
}

func buildLoginAccountRequest(profile oauthRequestProfile, state, accessToken string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, endpointLoginAcct+state, nil)
	if err != nil {
		return nil, err
	}
	applyOAuthProfileHeaders(req, profile)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if profile.mode == oauthClientModeWorkBuddy {
		req.Header.Set("X-No-User-Id", "true")
		req.Header.Set("X-No-Enterprise-Id", "true")
		req.Header.Set("X-No-Department-Info", "true")
	}
	return req, nil
}

func buildTokenRefreshRequest(profile oauthRequestProfile, sa *storedAuth) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, endpointTokenRefreshFor(sa), nil)
	if err != nil {
		return nil, err
	}
	applyOAuthProfileHeaders(req, profile)
	if profile.mode == oauthClientModeWorkBuddy {
		req.Header.Set("X-No-Authorization", "true")
		req.Header.Set("X-No-User-Id", "true")
		req.Header.Set("X-No-Department-Info", "true")
		if sa.Account.EnterpriseID == "" {
			req.Header.Set("X-No-Enterprise-Id", "true")
		}
	}
	req.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	return req, nil
}

func decorateDesktopAuthURL(rawURL, loginSessionID string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	query := u.Query()
	query.Set("version", "5.3.14")
	query.Set("loginSessionId", loginSessionID)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func handleStartLogin(raw []byte) ([]byte, error) {
	client, err := newLoginClient()
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	mode := oauthClientModeCLI
	if features := currentFeatureRuntime(); features != nil {
		mode = features.oauthClientMode
	}
	profile := oauthProfileForMode(mode)
	stateReq, err := buildAuthStateRequest(profile)
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	data, _, err := doJSONRequest(client, stateReq)
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl — please restart the login flow")
	}
	loginSessionID := ""
	if profile.mode == oauthClientModeWorkBuddy {
		loginSessionID = randomHex(16)
		st.AuthURL, err = decorateDesktopAuthURL(st.AuthURL, loginSessionID)
		if err != nil {
			return nil, fmt.Errorf("auth state: invalid authUrl: %w", err)
		}
	}
	loginStates.Store(st.State, &loginCtx{
		client:         client,
		expires:        time.Now().Add(loginTTL),
		profile:        profile,
		loginSessionID: loginSessionID,
	})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       st.AuthURL,
		State:     st.State,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
		Metadata:  map[string]any{"logo": pluginLogoURL},
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login) — the login session was lost; please re-initiate login")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired (5 min timeout) — please re-initiate login and complete within 5 minutes")
	}
	profile := lc.profile
	if profile.mode == "" {
		profile = oauthProfileForMode(oauthClientModeCLI)
	}
	pollClient, err := loginClientForCurrentRouting(lc)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: %w", err)
	}

	// Single-shot poll per RPC: the host drives the polling cadence.
	// auth/token is the authoritative login-status endpoint: the application
	// layer returns a non-zero code ("login ing") while pending, and code 0
	// with the token bundle once complete. login/account sits behind the
	// openresty gateway and is rejected (401) until login finishes, so probe
	// token first and only fetch account once we hold a bearer.
	tokenReq, err := buildAuthTokenRequest(profile, state)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: token request: %w", err)
	}
	tokRaw, status, errTok := doJSONRequest(pollClient, tokenReq)
	if errTok != nil {
		// Transport-level failures and 5xx are real errors, not "still waiting":
		// surface them so the user sees a failure instead of polling until TTL.
		if status == 0 || status >= 500 {
			loginStates.Delete(state)
			return nil, fmt.Errorf("poll: token endpoint error: %w — upstream may be temporarily unavailable; retry in a few minutes", errTok)
		}
		// 4xx / business-code responses mean the login is still pending.
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	pollClient, err = loginClientForCurrentRouting(lc)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: %w", err)
	}

	var acct accountData
	accountReq, err := buildLoginAccountRequest(profile, state, tok.AccessToken)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: account request: %w", err)
	}
	acctRaw, _, errAcct := doJSONRequest(pollClient, accountReq)
	if errAcct != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: account lookup failed after token success: %w", errAcct)
	}
	if err := json.Unmarshal(acctRaw, &acct); err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: account lookup parse failed: %w", err)
	}
	sa, err := buildLoginStoredAuth(tok, acct)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: %w", err)
	}
	loginStates.Delete(state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthDataForLoginPoll(sa),
	})
}

func toAuthDataForLoginPoll(sa *storedAuth) pluginapi.AuthData {
	ad := toAuthData(sa)
	ad.ID = ""
	return ad
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req authRefreshRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	// Route via host.http.do so request-log captures the refresh call (H2
	// compliance: was doJSON(sharedHTTPClient()) — bypassed host transport
	// policy + logging for the X-Refresh-Token endpoint).
	data, raw2, status, err := refreshCallWithCallback(sa, req.HostCallbackID)
	if err != nil {
		if status >= 400 {
			return nil, fmt.Errorf("refresh rejected (HTTP %d)", status)
		}
		return nil, fmt.Errorf("refresh: %w", err)
	}
	_ = raw2
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken in response — the refresh token may be expired; re-login required")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = expiryFromExpiresIn(tok.ExpiresIn, sa.Auth.ExpiresAt)
	// No explicit host.auth.save here: the host's auth Manager persists the
	// refreshed credential itself after Refresh returns (conductor.go
	// refreshAuth → m.Update → persist). Writing from the plugin too would
	// double-write the file.
	response, err := okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthDataForRefresh(sa)})
	if err != nil {
		return nil, err
	}
	currentModelRuntime().markAuthNotStarted(req.AuthID)
	return response, nil
}

func buildLoginStoredAuth(tok tokenData, acct accountData) (*storedAuth, error) {
	if strings.TrimSpace(tok.AccessToken) == "" {
		return nil, fmt.Errorf("token response missing accessToken")
	}
	if strings.TrimSpace(acct.UID) == "" {
		return nil, fmt.Errorf("account lookup missing uid")
	}
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    expiryFromExpiresIn(tok.ExpiresIn, 0),
			Domain:       tok.Domain,
		},
		Account: storedAccount{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}, nil
}

func expiryFromExpiresIn(expiresIn, oldExpiry int64) int64 {
	if expiresIn <= 0 {
		return oldExpiry
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
}

// preserveExpiry reuses the previous token's expiresAt when the refresh
// response omits expiresIn (some CodeBuddy deployments return only the token
// pair). Zero would tell the host the credential is permanently expired and
// trigger a refresh storm on every request.
func preserveExpiry(newExpiry, oldExpiry int64) int64 {
	if newExpiry > 0 {
		return newExpiry
	}
	return oldExpiry
}

// toAuthDataForRefresh returns AuthData with FileName left EMPTY so the CPA
// host backfills the original auth.FileName (auth_provider.go:371).
//
// CPA uses FileName (relative to auth dir) as auth ID. If we set it to
// "workbuddy-<uid>.json" while the original file was "workbuddy.json"
// (legacy single-account name), the host treats it as a rename, writes a
// NEW file, and the old one stays → duplicate auth records.
//
// Returning empty FileName = "keep what you had" → no rename, no dup.
func toAuthDataForRefresh(sa *storedAuth) pluginapi.AuthData {
	ad := toAuthDataOpts(sa, nil, false)
	ad.FileName = "" // let host backfill original
	ad.ID = ""       // let host compute from path (prevents ID mismatch dupes)
	return ad
}
