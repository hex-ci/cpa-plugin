// oauth.go implements qwenwork's device-authorization login. One flow only:
//
//  1. StartLogin: build the /device/selectAccounts PKCE URL (challenge +
//     nonce + machine_id + client_id + redirect_uri) and stash the verifier
//     under the returned state.
//  2. PollLogin: poll /api/v1/deviceToken/poll with nonce + verifier until the
//     grant lands (404/202 = pending). qwenwork has NO PAT/jobToken path
//     (jobToken/exchange returns 404) — device tokens only.
//
// Refresh (handleRefreshAuth) uses POST /api/v1/deviceToken/refresh with the
// ory_rt_ refresh token plus target:"c".
//
// Reverse-engineered from the official QwenWorkCN desktop client login URL
// (2026-09-02, live capture) — same Qoder device flow as qoderwork, but on
// gateway.qwenwork.cn with client_id e883ade2-… and redirect_uri qwenwork-cn://.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// doRawJSON sends method to fullURL with the given headers and returns the
// raw response body. Used for endpoints that return plain JSON (no envelope)
// like /api/v1/deviceToken/refresh and /api/v1/userinfo.
func doRawJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d body=%s", resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d (location: %s)", resp.StatusCode, resp.Header.Get("Location"))
	}
	return raw, resp.StatusCode, nil
}

// userInfoResponse is the minimal subset of GET /api/v1/userinfo we need to
// populate auth identity (uid, nickname, email).
type userInfoResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Avatar   string `json:"avatar"`
}

// fetchUserInfo queries /api/v1/userinfo with a device-token Bearer to populate
// identity fields (uid, nickname, email for COSY signing).
func fetchUserInfo(jt string) (*userInfoResponse, error) {
	req, err := http.NewRequest(http.MethodGet, endpointUserInfo, nil)
	if err != nil {
		return nil, err
	}
	commonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+jt)
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("userinfo: http %d body=%s", resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	var out userInfoResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("userinfo parse: %w (body=%s)", err, truncateRedacted(string(raw), 200))
	}
	return &out, nil
}

// -----------------------------------------------------------------------------
// Device-authorization login
// -----------------------------------------------------------------------------

const (
	qwenAuthBase    = "https://gateway.qwenwork.cn"
	qwenClientID    = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	qwenRedirectURI = "qwenwork-cn://"
)

// deviceTokenResponse mirrors /api/v1/deviceToken/{poll,refresh} payloads.
// Poll returns expires_at (RFC3339) AND expires_in (qwenwork expresses it in
// SECONDS, e.g. 604800 = 7d); refresh returns only expires_at. See
// deviceExpiryUnix for the second/ms magnitude handling.
type deviceTokenResponse struct {
	Token                 string `json:"token"`
	DeviceToken           string `json:"device_token"`
	RefreshToken          string `json:"refresh_token"`
	UserID                string `json:"user_id"`
	UserName              string `json:"user_name"`
	ExpiresAt             string `json:"expires_at"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
}

func (d *deviceTokenResponse) accessToken() string {
	if d.Token != "" {
		return d.Token
	}
	return d.DeviceToken
}

// makePKCE returns (verifier, challenge) per RFC 7636 (S256).
func makePKCE() (string, string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		s := uuid.NewString() + uuid.NewString()
		sum := sha256.Sum256([]byte(s))
		v := base64.RawURLEncoding.EncodeToString(sum[:])
		return v, v
	}
	verifier := make([]byte, 64)
	for i, b := range raw {
		verifier[i] = alphabet[int(b)%len(alphabet)]
	}
	sum := sha256.Sum256(verifier)
	return string(verifier), base64.RawURLEncoding.EncodeToString(sum[:])
}

// handleStartLogin implements AuthProvider.StartLogin: build the device
// authorization URL and stash the PKCE verifier under the returned state.
func handleStartLogin(raw []byte) ([]byte, error) {
	verifier, challenge := makePKCE()
	nonce := uuid.NewString()
	machineID := uuid.NewString()

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("machine_id", machineID)
	q.Set("client_id", qwenClientID)
	q.Set("redirect_uri", qwenRedirectURI)
	authURL := qwenAuthBase + "/device/selectAccounts?" + q.Encode()

	now := time.Now()
	state := fmt.Sprintf("qw-%d", now.UnixNano())
	loginStates.Store(state, &loginCtx{verifier: verifier, nonce: nonce, expires: now.Add(loginTTL), startedAt: now.UnixNano()})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       authURL,
		State:     state,
		ExpiresAt: now.Add(loginTTL).UTC(),
		Metadata: map[string]any{
			"logo":   pluginLogoURL,
			"prompt": "在打开的页面中登录并授权 QwenWork（千问办公，设备授权）。完成后此窗口会自动关闭。",
		},
	})
}

// pollDeviceToken performs one GET against /api/v1/deviceToken/poll.
// Returns (tok, pending, error): pending=true means the user hasn't finished
// authorizing yet (upstream 404/202) — the host should keep polling.
func pollDeviceToken(nonce, verifier string) (*deviceTokenResponse, bool, error) {
	q := url.Values{}
	q.Set("nonce", nonce)
	q.Set("verifier", verifier)
	q.Set("challenge_method", "S256")
	fullURL := upstreamBaseCN + "/api/v1/deviceToken/poll?" + q.Encode()

	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return nil, true, nil
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("deviceToken poll: http %d body=%s", resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	var out deviceTokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("deviceToken poll parse: %w", err)
	}
	if out.accessToken() == "" {
		return nil, true, nil // 200 without token yet — treat as pending
	}
	return &out, false, nil
}

// refreshDeviceToken calls POST /api/v1/deviceToken/refresh with the ory_rt_
// refresh token plus target:"c" (qwenwork's device-flow refresh contract).
func refreshDeviceToken(drt string) (*deviceTokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": drt, "target": "c"})
	data, _, err := doRawJSON(sharedHTTPClient(), http.MethodPost, upstreamBaseCN+"/api/v1/deviceToken/refresh", nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out deviceTokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	if out.accessToken() == "" || out.RefreshToken == "" {
		return nil, fmt.Errorf("deviceToken refresh: incomplete token pair in response")
	}
	return &out, nil
}

// handlePollLogin implements AuthProvider.PollLogin. Only the device-
// authorization poll — qwenwork has no PAT pasted-in path.
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
		return nil, fmt.Errorf("poll: unknown state (restart login)")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired (10 min timeout)")
	}

	tok, pending, err := pollDeviceToken(lc.nonce, lc.verifier)
	if err != nil {
		return nil, fmt.Errorf("device authorization poll: %w", err)
	}
	if !pending && tok != nil {
		// Auth file must land IMMEDIATELY — no upstream calls before return.
		// UserID is already in the poll response; nickname/email are filled
		// lazily by the panel / keepalive on later loads.
		sa := buildStoredAuthFromDeviceToken(tok, nil)
		loginStates.Delete(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status: pluginapi.AuthLoginStatusSuccess,
			Auth:   toAuthData(sa),
		})
	}

	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "等待浏览器完成 QwenWork 设备授权",
	})
}

// deviceExpiryUnix resolves the access-token expiry from a deviceToken
// poll/refresh response. Poll returns both expires_at (RFC3339) and
// expires_in; qwenwork expresses expires_in in SECONDS (e.g. 604800 = 7d),
// unlike qoderwork (milliseconds) — detect by magnitude. Default: 30 days.
func deviceExpiryUnix(tok *deviceTokenResponse) int64 {
	if tok.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, tok.ExpiresAt); err == nil {
			return t.Unix()
		}
	}
	if tok.ExpiresIn > 0 {
		if tok.ExpiresIn > 10_000_000 {
			return time.Now().Add(time.Duration(tok.ExpiresIn) * time.Millisecond).Unix()
		}
		return time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return time.Now().Add(30 * 24 * time.Hour).Unix()
}

// buildStoredAuthFromDeviceToken maps a device-token grant onto storedAuth.
// When ui is nil (fast path during PollLogin), UserID comes from the poll
// response and nickname/email are empty — the panel fills them lazily on
// later loads.
func buildStoredAuthFromDeviceToken(tok *deviceTokenResponse, ui *userInfoResponse) *storedAuth {
	expiresAt := deviceExpiryUnix(tok)
	uid := tok.UserID
	nickname := ""
	email := ""
	if ui != nil {
		if ui.ID != "" {
			uid = ui.ID
		}
		nickname = ui.Name
		email = ui.Email
	}
	if nickname == "" {
		if tok.UserName != "" {
			nickname = tok.UserName
		} else if len(uid) > 8 {
			nickname = "u" + uid[len(uid)-8:]
		} else {
			nickname = "u" + uid
		}
	}
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:   tok.accessToken(),
			RefreshToken:  tok.RefreshToken,
			PersonalToken: "", // qwenwork has no PAT
			ExpiresAt:     expiresAt,
			Domain:        "gateway.qwenwork.cn",
		},
		Account: storedAccount{UID: uid, Nickname: nickname, Email: email},
	}
}

// handleRefreshAuth implements AuthProvider.Refresh. qwenwork has a single
// credential family: POST /api/v1/deviceToken/refresh with the ory_rt_ token.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}

	tok, err := refreshDeviceToken(sa.Auth.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("refresh rejected: %w — deviceToken refresh failed; re-login via OAuth required", err)
	}
	sa.Auth.AccessToken = tok.accessToken()
	sa.Auth.RefreshToken = tok.RefreshToken
	sa.Auth.ExpiresAt = preserveExpiry(deviceExpiryUnix(tok), sa.Auth.ExpiresAt)
	invalidateCosySession(sa.Account.UID)
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthDataForRefresh(sa)})
}

// preserveExpiry reuses the previous token's expiresAt when the refresh
// response omits expiresIn. Zero would tell the host the credential is
// permanently expired and trigger a refresh storm on every request.
func preserveExpiry(newExpiry, oldExpiry int64) int64 {
	if newExpiry > 0 {
		return newExpiry
	}
	return oldExpiry
}

// toAuthDataForRefresh mirrors the workbuddy helper: blank out FileName and
// ID so the host backfills from the original auth path (prevents ID mismatch
// duplicate files when Refresh round-trips the record).
func toAuthDataForRefresh(sa *storedAuth) pluginapi.AuthData {
	ad := toAuthDataOpts(sa, nil, false)
	ad.FileName = "" // let host backfill original
	ad.ID = ""       // let host compute from path (prevents ID mismatch dupes)
	return ad
}
