// oauth.go implements the browser login flow. The host drives it through
// StartLogin/PollLogin like any other provider, so no token is ever pasted into
// a UI. The callback lands on a listener this plugin opens on 127.0.0.1 because
// the TraeWork authorization page rejects every callback URL that is not
// http://127.0.0.1:<port>/authorize (verified against the live page: a
// host-served path is answered with "登录失败").
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginCtx is one in-flight login attempt, keyed by state in loginStates.
type loginCtx struct {
	state      string
	verifier   string
	machineID  string
	deviceID   string
	publicKey  string
	privateKey string
	expires    time.Time
	// hostCallback is the CPA host's own OAuth callback endpoint, handed to
	// StartLogin as BaseURL. The code captured on the loopback listener is
	// relayed there so the host owns the handoff (see relayToHost). It stays
	// empty for an attempt the panel starts, which has no host session.
	hostCallback string
	// authorizeURL and port are echoed to the panel so it can drive the same
	// attempt the host's OAuth entry started.
	authorizeURL string
	// callbackURL is the address the authorization page redirects to for this
	// attempt (loopback by default, the CPA's public resource route when one is
	// configured or learned).
	callbackURL string
	port        int

	mu        sync.Mutex
	code      string
	failure   string
	redeemed  *pluginapi.AuthData
	stored    *storedAuth
	lastError string
	redeeming bool
	server    *http.Server
}

// errRedeemInFlight means another driver is already exchanging this code.
var errRedeemInFlight = errors.New("redeem in flight")

var loginStates sync.Map // state(string) -> *loginCtx

// listenCallback binds a temporary loopback port for one login attempt. The port
// is never configured: it only matters when the browser runs on the same machine
// as the gateway, where the redirect lands on the listener and the login finishes
// without the operator pasting anything. Everywhere else it is irrelevant — the
// authorization page hands the code to the browser, which returns it through the
// panel's paste box.
func listenCallback() (net.Listener, int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return listener, listener.Addr().(*net.TCPAddr).Port, nil
}

// redeemOnce exchanges the captured code for tokens, saves the account and
// remembers the result, so every driver (host poll, panel poll, pasted
// callback) delivers the same login and the single-use code is spent once.
func (c *loginCtx) redeemOnce(carrier map[string]any) (pluginapi.AuthData, *storedAuth, error) {
	c.mu.Lock()
	if c.redeemed != nil {
		auth, sa := *c.redeemed, c.stored
		c.mu.Unlock()
		return auth, sa, nil
	}
	if c.redeeming {
		c.mu.Unlock()
		return pluginapi.AuthData{}, nil, errRedeemInFlight
	}
	code := c.code
	if code == "" {
		c.mu.Unlock()
		return pluginapi.AuthData{}, nil, errors.New("no authorization code captured yet")
	}
	c.redeeming = true
	c.mu.Unlock()

	auth, sa, err := redeemLoginCode(c, code, carrier)
	if err == nil {
		if _, errSave := saveAuthRecord(sa, carrier); errSave != nil {
			err = fmt.Errorf("保存认证文件失败：%w", errSave)
		}
	}

	c.mu.Lock()
	c.redeeming = false
	if err == nil {
		c.redeemed = &auth
		c.stored = sa
		if until := time.Now().Add(codeGrace); until.After(c.expires) {
			c.expires = until
		}
	} else {
		c.lastError = err.Error()
	}
	c.mu.Unlock()
	return auth, sa, err
}

func (c *loginCtx) completed() *pluginapi.AuthData {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.redeemed
}

func init() {
	go loginJanitor()
}

// loginJanitor drops abandoned login attempts so their listeners are released.
func loginJanitor() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		loginStates.Range(func(key, value any) bool {
			ctx, ok := value.(*loginCtx)
			if !ok {
				loginStates.Delete(key)
				return true
			}
			if now.After(ctx.deadline()) {
				ctx.close()
				loginStates.Delete(key)
			}
			return true
		})
	}
}

// close releases the loopback listener. Closing the server closes the listener
// it was handed, so there is no separate listener to track.
func (c *loginCtx) close() {
	c.mu.Lock()
	server := c.server
	c.server = nil
	c.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
}

func (c *loginCtx) hasCode() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code, c.failure
}

// deadline is when the attempt is swept; a captured code pushes it out by
// codeGrace because the code stays redeemable after the browser window closes.
func (c *loginCtx) deadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expires
}

func (c *loginCtx) storeCode(code string) {
	c.mu.Lock()
	c.code = code
	if until := time.Now().Add(codeGrace); until.After(c.expires) {
		c.expires = until
	}
	c.mu.Unlock()
}

func (c *loginCtx) storeFailure(reason string) {
	c.mu.Lock()
	c.failure = reason
	c.mu.Unlock()
}

func b64URLNoPad(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

// randomHex returns n random bytes as hex; used for request and tool-call ids.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "000000000000000000000000"
	}
	return hex.EncodeToString(buf)
}

func randomUUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// deviceIdentity is the identity one login registers with the server.
type deviceIdentity struct {
	privatePEM string
	publicPEM  string
	machineID  string
	deviceID   string
}

// storedDeviceIdentity is what an existing auth record can supply.
type storedDeviceIdentity struct {
	privatePEM string
	machineID  string
	deviceID   string
}

// deviceIdentityLookup is the seam that reads the host's auth store; tests
// replace it because no host is available there.
var deviceIdentityLookup = lookupStoredDeviceIdentity

func lookupStoredDeviceIdentity() *storedDeviceIdentity {
	files, err := hostAuthList()
	if err != nil {
		return nil
	}
	for _, file := range files {
		sa, _, err := hostAuthGetBundle(file.AuthIndex)
		if err != nil || sa.PrivateKey == "" || sa.MachineID == "" || sa.DeviceID == "" {
			continue
		}
		return &storedDeviceIdentity{
			privatePEM: sa.PrivateKey,
			machineID:  sa.MachineID,
			deviceID:   sa.DeviceID,
		}
	}
	return nil
}

// loadOrCreateDeviceIdentity reuses the identity an earlier login registered.
// The host keeps auth-file fields that already exist when it rewrites the file,
// so generating a fresh key pair on every login would leave the record holding a
// key that no longer matches the one the server bound the refresh token to. The
// public half is always derived from the private half for the same reason.
func loadOrCreateDeviceIdentity() (*deviceIdentity, error) {
	if stored := deviceIdentityLookup(); stored != nil {
		publicKey, err := publicKeyFromPrivate(stored.privatePEM)
		if err == nil {
			return &deviceIdentity{
				privatePEM: stored.privatePEM,
				publicPEM:  publicKey,
				machineID:  stored.machineID,
				deviceID:   stored.deviceID,
			}, nil
		}
	}
	pair, err := newDeviceKeyPair()
	if err != nil {
		return nil, errors.New("generate device key failed")
	}
	machineID, err := randomUUID()
	if err != nil {
		return nil, errors.New("generate machine id failed")
	}
	deviceID, err := randomUUID()
	if err != nil {
		return nil, errors.New("generate device id failed")
	}
	return &deviceIdentity{
		privatePEM: pair.privatePEM,
		publicPEM:  pair.publicPEM,
		machineID:  machineID,
		deviceID:   deviceID,
	}, nil
}

// newPKCE returns the S256 verifier/challenge pair.
func newPKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = b64URLNoPad(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, b64URLNoPad(sum[:]), nil
}

// buildAuthorizeURL mirrors the desktop client's login URL builder.
func buildAuthorizeURL(callback, challenge, loginTraceID, machineID, deviceID string) string {
	params := url.Values{}
	params.Set("login_version", "1")
	params.Set("auth_from", "solo")
	params.Set("login_channel", "native_ide")
	params.Set("plugin_version", ideVersion)
	params.Set("auth_type", "local")
	params.Set("client_id", oauthClientID)
	params.Set("redirect", "0")
	params.Set("login_trace_id", loginTraceID)
	params.Set("auth_callback_url", callback)
	params.Set("machine_id", machineID)
	params.Set("device_id", deviceID)
	params.Set("hide_saas_login", "true")
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("x_device_type", deviceType)
	params.Set("x_os_version", osVersion)
	params.Set("x_app_version", ideVersion)
	params.Set("x_app_type", appType)
	return authorizeBase + "?" + params.Encode()
}

// authCodeFromQuery pulls the authorization code out of a callback query. The
// authorization page returns it JSON-encoded inside authCodeInfo (not as
// code=), so every shape that page has been seen to emit is accepted: a flat
// blob, a nested one, and a blob that was encoded twice.
func authCodeFromQuery(values url.Values) string {
	for _, raw := range values["authCodeInfo"] {
		if code := authCodeFromInfo(raw); code != "" {
			return code
		}
	}
	for _, key := range []string{"code", "auth_code", "AuthCode", "authCode"} {
		if code := strings.TrimSpace(values.Get(key)); code != "" {
			return code
		}
	}
	return ""
}

// authCodeFromInfo digs AuthCode out of the authCodeInfo payload. The key is
// matched case-insensitively because the page's field casing is not part of any
// contract, and the payload is re-decoded once in case it was double-encoded.
func authCodeFromInfo(raw string) string {
	candidate := strings.TrimSpace(raw)
	for attempt := 0; attempt < 2 && candidate != ""; attempt++ {
		var blob map[string]any
		if err := json.Unmarshal([]byte(candidate), &blob); err == nil {
			if code := authCodeKey(blob); code != "" {
				return code
			}
			for _, value := range blob {
				nested, ok := value.(map[string]any)
				if !ok {
					continue
				}
				if code := authCodeKey(nested); code != "" {
					return code
				}
			}
			return ""
		}
		decoded, err := url.QueryUnescape(candidate)
		if err != nil || decoded == candidate {
			return ""
		}
		candidate = decoded
	}
	return ""
}

func authCodeKey(blob map[string]any) string {
	for key, value := range blob {
		if !strings.EqualFold(key, "AuthCode") {
			continue
		}
		if text, ok := value.(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// handoffCode reads the host's standard OAuth handoff file for this state. The
// host writes .oauth-<provider>-<state>.oauth when a callback reaches its own
// endpoint — the path every provider (built-in or plugin) uses — so polling
// reads it exactly like the in-memory capture.
func handoffCode(authDir, state string) (string, string) {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" || state == "" {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(authDir, ".oauth-"+providerName+"-"+state+".oauth"))
	if err != nil {
		return "", ""
	}
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", ""
	}
	return strings.TrimSpace(payload.Code), strings.TrimSpace(payload.Error)
}

func callbackFailure(values url.Values) string {
	for _, key := range []string{"error_msg", "error", "error_description"} {
		if msg := strings.TrimSpace(values.Get(key)); msg != "" {
			return msg
		}
	}
	if code := strings.TrimSpace(values.Get("error_code")); code != "" && code != "0" {
		return "授权失败（error_code " + code + "）"
	}
	return ""
}

const callbackPage = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">` +
	`<title>TraeWork 登录</title></head><body style="font-family:system-ui;padding:48px;line-height:1.7">` +
	`<h2>%s</h2><p>%s</p></body></html>`

// newCallbackServer builds the loopback listener for one login attempt.
func newCallbackServer(ctx *loginCtx) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		if failure := callbackFailure(values); failure != "" {
			ctx.storeFailure(failure)
			writeCallbackPage(w, "登录失败", failure+"，请回到 CPA 重新发起登录。")
			return
		}
		code := authCodeFromQuery(values)
		if code == "" {
			// Not the redirect we wait for (favicon, a manual visit): leave the
			// attempt alive so the real callback can still land.
			hostLogInfo("traework: login callback arrived without an authorization code", map[string]any{
				"state": ctx.state,
				"keys":  callbackKeyNames(values),
			})
			writeCallbackPage(w, "等待登录回调", "本页是TraeWork的登录回调地址，请不要直接访问。")
			return
		}
		ctx.storeCode(code)
		hostLogInfo("traework: login code captured on the loopback callback", map[string]any{"state": ctx.state})
		relayToHost(ctx, code)
		writeCallbackPage(w, "登录成功", "授权码已收到，可以关闭本页并回到 CPA。")
		go func() {
			// Give the browser a moment to render before the port closes.
			time.Sleep(300 * time.Millisecond)
			ctx.close()
		}()
	})
	return &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

// relayToHost hands the captured code to the CPA host's own OAuth callback
// endpoint — the same route the host's built-in providers redirect to — so the
// host writes the standard handoff file and the login completes through host
// polling. The in-memory copy stays as the fallback when the relay is refused.
func relayToHost(ctx *loginCtx, code string) {
	target := strings.TrimSpace(ctx.hostCallback)
	if target == "" {
		return
	}
	parsed, err := url.Parse(target)
	if err != nil {
		hostLogInfo("traework: host callback url is unusable", map[string]any{"error": err.Error()})
		return
	}
	query := parsed.Query()
	query.Set("state", ctx.state)
	query.Set("code", code)
	parsed.RawQuery = query.Encode()

	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		hostLogInfo("traework: relaying the code to the host callback failed", map[string]any{
			"state": ctx.state,
			"error": err.Error(),
		})
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		hostLogInfo("traework: the host refused the relayed callback", map[string]any{
			"state":  ctx.state,
			"status": response.StatusCode,
		})
	}
}

// handleCallbackResource serves the plugin's own resource route as the OAuth
// callback target. A public deployment already reaches
// /v0/resource/plugins/<id>/… (that is how the panel is shown), so pointing the
// authorization page here needs no extra reverse-proxy routing. Redeeming the
// captured code still needs the attempt's PKCE verifier, which never leaves the
// plugin, so a stray request here cannot turn into a credential.
func handleCallbackResource(query url.Values, headers http.Header) string {
	if failure := callbackFailure(query); failure != "" {
		if ctx := pendingAttemptFor(headers); ctx != nil {
			ctx.storeFailure(failure)
			hostLogInfo("traework: public callback reported a failure", map[string]any{
				"state": ctx.state,
				"keys":  callbackKeyNames(query),
			})
		}
		return fmt.Sprintf(callbackPage, "登录失败", failure+"，请回到 CPA 重新发起登录。")
	}
	code := authCodeFromQuery(query)
	if code == "" {
		// Not the redirect we wait for (a manual visit, a favicon probe).
		hostLogInfo("traework: public callback arrived without an authorization code", map[string]any{
			"keys": callbackKeyNames(query),
		})
		return fmt.Sprintf(callbackPage, "等待登录回调", "本页是TraeWork的登录回调地址，请不要直接访问。")
	}
	ctx := pendingAttemptFor(headers)
	if ctx == nil {
		hostLogInfo("traework: public callback arrived with no pending login", map[string]any{
			"keys": callbackKeyNames(query),
		})
		return fmt.Sprintf(callbackPage, "没有进行中的登录", "请先在 CPA 的 TraeWork 面板点「开始登录」，再回到浏览器完成授权。")
	}
	ctx.storeCode(code)
	hostLogInfo("traework: login code captured on the public callback route", map[string]any{
		"state": ctx.state,
		"keys":  callbackKeyNames(query),
	})
	return fmt.Sprintf(callbackPage, "登录成功", "授权码已收到，可以关闭本页并回到 CPA。")
}

// pendingAttemptFor picks the attempt a callback belongs to: the newest one
// still waiting for a code. A forwarded-host hint prefers an attempt whose
// callback URL was built for that host, so two concurrent logins on different
// origins cannot steal each other's code.
func pendingAttemptFor(headers http.Header) *loginCtx {
	host := forwardedHost(headers)
	var newest, matching *loginCtx
	loginStates.Range(func(_, value any) bool {
		ctx, ok := value.(*loginCtx)
		if !ok {
			return true
		}
		if code, failure := ctx.hasCode(); code != "" || failure != "" {
			return true
		}
		if ctx.completed() != nil || time.Now().After(ctx.deadline()) {
			return true
		}
		if newest == nil || ctx.deadline().After(newest.deadline()) {
			newest = ctx
		}
		if host != "" && strings.Contains(strings.ToLower(ctx.callbackTarget()), host) {
			if matching == nil || ctx.deadline().After(matching.deadline()) {
				matching = ctx
			}
		}
		return true
	})
	if matching != nil {
		return matching
	}
	return newest
}

// forwardedHost reads the host a reverse proxy saw, if it forwards one.
func forwardedHost(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, key := range []string{"X-Forwarded-Host", "X-Original-Host", "Forwarded"} {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			if index := strings.Index(value, "host="); index >= 0 {
				value = strings.Trim(value[index+len("host="):], "\" ")
			}
			if comma := strings.Index(value, ","); comma >= 0 {
				value = value[:comma]
			}
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// callbackTarget is the callback address this attempt was armed with.
func (c *loginCtx) callbackTarget() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callbackURL
}

// callbackKeyNames lists which query keys arrived, never their values: the code
// itself must not reach a log.
func callbackKeyNames(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// serveCallback serves the loopback listener until the redirect lands.
func serveCallback(server *http.Server, listener net.Listener) {
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("traework: login callback listener stopped: %v", err)
	}
}

func writeCallbackPage(w http.ResponseWriter, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, callbackPage, title, message)
}

// progress advances one attempt as far as it can and reports the status the
// caller should surface. It is the single place a captured code becomes a saved
// account, so the host's poll, the panel's poll and a pasted callback all behave
// the same.
func (c *loginCtx) progress(authDir string, carrier map[string]any) (pluginapi.AuthLoginStatus, string) {
	if done := c.completed(); done != nil {
		return pluginapi.AuthLoginStatusSuccess, "登录成功"
	}
	code, failure := c.hasCode()
	if code == "" && failure == "" && authDir != "" {
		// The host writes its standard handoff file when the code arrives
		// through its own callback endpoint (the relayed loopback capture).
		if handoff, handoffError := handoffCode(authDir, c.state); handoff != "" {
			c.storeCode(handoff)
			code = handoff
			hostLogInfo("traework: login code read from the host handoff file", map[string]any{"state": c.state})
		} else if handoffError != "" {
			c.storeFailure(handoffError)
			failure = handoffError
		}
	}
	if failure != "" {
		// Terminal: the listener is done, the attempt stays queryable so the
		// caller (host poll or panel) keeps reporting the same reason.
		c.close()
		return pluginapi.AuthLoginStatusError, failure
	}
	if code == "" {
		// Only a bare timeout is refused: once the browser delivered a code
		// there is a redeemable credential, and discarding it because the
		// browser window lapsed would waste a completed login.
		if time.Now().After(c.deadline()) {
			c.close()
			return pluginapi.AuthLoginStatusError, "登录已超时：未收到浏览器回调（" + loginCallbackURL(c.port) + " 不可达或授权未完成），请重新发起"
		}
		return pluginapi.AuthLoginStatusPending, "等待浏览器回调 " + loginCallbackURL(c.port) + "：浏览器够不到它时，请把授权后地址栏的 URL 粘贴到面板的登录框"
	}
	if _, _, err := c.redeemOnce(carrier); err != nil {
		if errors.Is(err, errRedeemInFlight) {
			return pluginapi.AuthLoginStatusPending, "正在换取令牌…"
		}
		return pluginapi.AuthLoginStatusError, "换取令牌失败：" + err.Error()
	}
	return pluginapi.AuthLoginStatusSuccess, "登录成功"
}

// loginCtxByState finds one attempt.
func loginCtxByState(state string) (*loginCtx, error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return nil, errors.New("state is required")
	}
	value, ok := loginStates.Load(state)
	if !ok {
		return nil, errors.New("登录会话不存在或已过期，请重新发起")
	}
	ctx, ok := value.(*loginCtx)
	if !ok {
		loginStates.Delete(state)
		return nil, errors.New("登录会话不可用，请重新发起")
	}
	return ctx, nil
}

// panelPayload is the panel-facing view of one attempt.
func (c *loginCtx) panelPayload(status pluginapi.AuthLoginStatus, message string) map[string]any {
	out := map[string]any{
		"status":        string(status),
		"state":         c.state,
		"authorize_url": c.authorizeURL,
		"callback_url":  loginCallbackURL(c.port),
		"callback_port": c.port,
		"expires_at":    c.deadline().Unix(),
		"message":       message,
	}
	if done := c.completed(); done != nil {
		out["label"] = done.Label
		out["file_name"] = done.FileName
	}
	return out
}

// panelLoginStart begins an attempt on the panel's behalf: the same flow the
// host's OAuth entry uses, minus the host session the panel's poll replaces.
func panelLoginStart() (map[string]any, error) {
	ctx, err := startLoginFlow("")
	if err != nil {
		return nil, err
	}
	hostLogInfo("traework: panel login started", map[string]any{"state": ctx.state, "callback_port": ctx.port})
	return ctx.panelPayload(pluginapi.AuthLoginStatusPending, panelWaitingMessage()), nil
}

// panelLoginCurrent reports the attempt the operator most likely means: the
// newest one still waiting for a code. The panel adopts it instead of starting a
// second one, so a code captured from the authorization link the CPA dialog
// showed still lands on the attempt whose PKCE verifier can redeem it.
func panelLoginCurrent() map[string]any {
	ctx := pendingAttemptFor(nil)
	if ctx == nil {
		return map[string]any{"status": "none"}
	}
	status, message := ctx.progress("", nil)
	if status != pluginapi.AuthLoginStatusPending {
		// Terminal: nothing left to adopt.
		return map[string]any{"status": "none"}
	}
	hostLogInfo("traework: panel adopted the pending login", map[string]any{"state": ctx.state})
	return ctx.panelPayload(status, message)
}

// panelLoginStatus reports one attempt and completes it once a code is waiting.
func panelLoginStatus(state string) (map[string]any, error) {
	ctx, err := loginCtxByState(state)
	if err != nil {
		return nil, err
	}
	status, message := ctx.progress("", nil)
	if status == pluginapi.AuthLoginStatusSuccess {
		if done := ctx.completed(); done != nil {
			hostLogInfo("traework: panel login completed", map[string]any{"state": ctx.state, "label": done.Label})
		}
	}
	return ctx.panelPayload(status, message), nil
}

// panelLoginSubmit takes the operator's pasted callback URL or bare auth code.
func panelLoginSubmit(state, input string) (map[string]any, error) {
	ctx, err := loginCtxByState(state)
	if err != nil {
		return nil, err
	}
	code, failure := authCodeFromInput(input)
	if failure != "" {
		// The pasted URL is the page reporting its own failure (cancel, deny):
		// that ends the attempt instead of leaving it waiting.
		ctx.storeFailure(failure)
		status, message := ctx.progress("", nil)
		return ctx.panelPayload(status, message), nil
	}
	if code == "" {
		return ctx.panelPayload(pluginapi.AuthLoginStatusPending, "没有识别到授权码：请粘贴授权后地址栏里的完整 URL，或其中的 AuthCode"), nil
	}
	ctx.storeCode(code)
	hostLogInfo("traework: panel received a pasted authorization code", map[string]any{"state": ctx.state})
	status, message := ctx.progress("", nil)
	return ctx.panelPayload(status, message), nil
}

func panelLoginCancel(state string) (map[string]any, error) {
	ctx, err := loginCtxByState(state)
	if err != nil {
		return nil, err
	}
	ctx.close()
	loginStates.Delete(state)
	return map[string]any{"status": "cancelled"}, nil
}

func panelWaitingMessage() string {
	return "在浏览器里打开授权链接完成登录；浏览器不在网关主机时，把授权后地址栏的 URL 粘贴到下面的输入框"
}

// authCodeFromInput extracts the code from whatever the operator pasted: the
// full callback URL, the authCodeInfo JSON on its own, or the bare AuthCode. A
// non-empty failure means the page itself reported an error instead.
func authCodeFromInput(raw string) (string, string) {
	input := strings.TrimSpace(raw)
	if input == "" {
		return "", ""
	}
	values := url.Values{}
	if strings.Contains(input, "=") {
		if parsed, err := url.Parse(input); err == nil {
			values = parsed.Query()
			if fragment := parsed.Fragment; fragment != "" {
				if fromFragment, errFragment := url.ParseQuery(fragment); errFragment == nil {
					for key, list := range fromFragment {
						values[key] = append(values[key], list...)
					}
				}
			}
		}
	}
	if failure := callbackFailure(values); failure != "" {
		return "", failure
	}
	if code := authCodeFromQuery(values); code != "" {
		return code, ""
	}
	if code := authCodeFromInfo(input); code != "" {
		return code, ""
	}
	if looksLikeAuthCode(input) {
		return input, ""
	}
	return "", ""
}

// looksLikeAuthCode keeps prose and URLs out of the code slot: the page prints
// an opaque token inside authCodeInfo, so a bare paste is accepted only when it
// could be one.
func looksLikeAuthCode(value string) bool {
	if len(value) < 8 || len(value) > 512 || strings.ContainsAny(value, " \t\r\n/?&#") {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '~', r == '+', r == '=', r == ':':
		default:
			return false
		}
	}
	return true
}

// loginCallbackURL is where the authorization page sends the browser. Loopback
// by default; login_callback_url lets an operator point it at a public address
// they reverse-proxy onto the plugin's listener, which removes the copy/paste
// step for browsers that cannot reach the gateway's loopback.
func loginCallbackURL(port int) string {
	if configured := strings.TrimSpace(loadString(loginCallbackOverride)); configured != "" {
		return configured
	}
	return fmt.Sprintf("http://127.0.0.1:%d/authorize", port)
}

// startLoginFlow creates one attempt — PKCE, device identity, loopback listener
// and authorization URL — for both drivers.
func startLoginFlow(baseURL string) (*loginCtx, error) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return nil, errors.New("generate PKCE failed")
	}
	device, err := loadOrCreateDeviceIdentity()
	if err != nil {
		return nil, err
	}
	listener, port, err := listenCallback()
	if err != nil {
		return nil, errors.New("no free loopback port for the login callback")
	}
	loginTraceID, err := randomUUID()
	if err != nil {
		_ = listener.Close()
		return nil, errors.New("generate login trace id failed")
	}
	state, err := randomUUID()
	if err != nil {
		_ = listener.Close()
		return nil, errors.New("generate login state failed")
	}

	callback := loginCallbackURL(port)
	ctx := &loginCtx{
		state:        state,
		verifier:     verifier,
		machineID:    device.machineID,
		deviceID:     device.deviceID,
		publicKey:    device.publicPEM,
		privateKey:   device.privatePEM,
		expires:      time.Now().Add(loginTTL),
		hostCallback: strings.TrimSpace(baseURL),
		port:         port,
	}
	ctx.callbackURL = callback
	ctx.authorizeURL = buildAuthorizeURL(callback, challenge, loginTraceID, device.machineID, device.deviceID)
	ctx.server = newCallbackServer(ctx)
	loginStates.Store(state, ctx)
	go serveCallback(ctx.server, listener)
	return ctx, nil
}

func handleStartLogin(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	_ = json.Unmarshal(request, &req)

	ctx, err := startLoginFlow(req.BaseURL)
	if err != nil {
		return errorEnvelope("internal_error", err.Error()), nil
	}
	hostLogInfo("traework: login started", map[string]any{
		"state":         ctx.state,
		"callback_port": ctx.port,
		"host_callback": ctx.hostCallback != "",
	})

	// The dialog hands the operator the authorization link itself. The page then
	// redirects to the plugin's loopback listener, which a remote browser cannot
	// reach — that login is finished in the panel, which adopts this very attempt
	// (see panelLoginCurrent) so the pasted code meets its own PKCE verifier.
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       ctx.authorizeURL,
		State:     ctx.state,
		ExpiresAt: ctx.deadline(),
		Metadata: map[string]any{
			"logo":          pluginLogoURL,
			"authorize_url": ctx.authorizeURL,
			"callback_port": ctx.port,
		},
	}), nil
}

func handlePollLogin(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error()), nil
	}
	ctx, err := loginCtxByState(req.State)
	if err != nil {
		return errorEnvelope("unknown_state", err.Error()), nil
	}
	status, message := ctx.progress(req.Host.AuthDir, req.Metadata)
	if status != pluginapi.AuthLoginStatusSuccess {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: status, Message: message}), nil
	}
	done := ctx.completed()
	if done == nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录结果不可用，请重新发起",
		}), nil
	}
	hostLogInfo("traework: login delivered to the host poll", map[string]any{"state": ctx.state, "label": done.Label})
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: message,
		Auth:    *done,
	}), nil
}

// redeemLoginCode exchanges a captured authorization code and builds the auth
// record. Both drivers — host polling and a pasted callback URL — go through it
// so a login behaves identically whichever way the code arrives.
func redeemLoginCode(ctx *loginCtx, code string, carrier map[string]any) (pluginapi.AuthData, *storedAuth, error) {
	res, err := exchangeAuthCodeFn(code, ctx.verifier, ctx.publicKey, ctx.machineID, ctx.deviceID)
	if err != nil {
		return pluginapi.AuthData{}, nil, err
	}
	sa := storedAuthFromExchange(res, ctx)
	if name, avatar := fetchProfileFn(sa); name != "" || avatar != "" {
		sa.Nickname = name
		sa.AvatarURL = avatar
	}
	return toAuthData(sa, carrier), sa, nil
}

// storedAuthFromExchange maps one ExchangeToken result onto the stored record.
func storedAuthFromExchange(res *exchangeResult, ctx *loginCtx) *storedAuth {
	sa := &storedAuth{
		AccessToken:  res.Token,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    msToUnixSeconds(res.TokenExpireAt),
		APIHost:      apiBaseCN,
		Region:       "CN",
		ClientID:     oauthClientID,
		PublicKey:    ctx.publicKey,
		PrivateKey:   ctx.privateKey,
		MachineID:    ctx.machineID,
		DeviceID:     ctx.deviceID,
	}
	if sa.ExpiresAt == 0 {
		sa.ExpiresAt = tokenExpiry(res.Token)
	}
	if uid, _ := accountIdentityFromToken(res.Token); uid != "" {
		sa.UID = uid
	}
	if sa.ClientID == "" {
		sa.ClientID = res.ClientID
	}
	return sa
}

// tokenExpiry reads the exp claim as a fallback for a missing TokenExpireAt.
func tokenExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.Exp
}

func handleRefreshAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error()), nil
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_storage", err.Error()), nil
	}
	if strings.TrimSpace(sa.RefreshToken) == "" {
		return errorEnvelope("no_refresh_token", "auth record has no refresh token; log in again"), nil
	}
	res, err := refreshTokenFn(sa)
	if err != nil {
		return errorEnvelope("refresh_failed", err.Error()), nil
	}
	sa.AccessToken = res.Token
	if strings.TrimSpace(res.RefreshToken) != "" {
		sa.RefreshToken = res.RefreshToken
	}
	if expiresAt := msToUnixSeconds(res.TokenExpireAt); expiresAt > 0 {
		sa.ExpiresAt = expiresAt
	} else if expiresAt := tokenExpiry(res.Token); expiresAt > 0 {
		sa.ExpiresAt = expiresAt
	}
	if uid, _ := accountIdentityFromToken(res.Token); uid != "" {
		sa.UID = uid
	}
	if sa.APIHost == "" {
		sa.APIHost = apiBaseCN
	}
	if sa.Region == "" {
		sa.Region = "CN"
	}
	if sa.Nickname == "" {
		if name, avatar := fetchProfileFn(sa); name != "" || avatar != "" {
			sa.Nickname = name
			sa.AvatarURL = avatar
		}
	}

	next := nextRefreshTime(sa.ExpiresAt)
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             toAuthData(sa, req.Metadata),
		NextRefreshAfter: next,
	}), nil
}

// nextRefreshTime refreshes 30 minutes before expiry, never sooner than 5
// minutes from now, so a token that reports a short life does not spin.
func nextRefreshTime(expiresAt int64) time.Time {
	if expiresAt <= 0 {
		return time.Now().Add(30 * time.Minute)
	}
	at := time.Unix(expiresAt, 0).Add(-30 * time.Minute)
	if min := time.Now().Add(5 * time.Minute); at.Before(min) {
		return min
	}
	return at
}

// authMetadataUserKeys are the top-level auth-file fields the host treats as
// user configuration. They must survive every plugin-driven rewrite, so a
// carrier built from an auth file only forwards these keys.
var authMetadataUserKeys = []string{
	"weight",
	"priority",
	"proxy_url",
	"proxy-url",
	"prefix",
	"headers",
	"request_retry",
	"request-retry",
	"excluded_models",
	"excluded-models",
	"model_aliases",
	"model-aliases",
	"disable_cooling",
	"disable-cooling",
	"websockets",
	"note",
}

func parseAuthMetadataCarrier(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	carrier := make(map[string]any, len(authMetadataUserKeys))
	for _, key := range authMetadataUserKeys {
		if value, ok := doc[key]; ok {
			carrier[key] = value
		}
	}
	if len(carrier) == 0 {
		return nil
	}
	return carrier
}

// handleParseAuth claims an auth file for this provider. The host routes by the
// file's top-level `type`; type-less files fall back to provider/file-name
// matching so legacy files still load.
func handleParseAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error()), nil
	}
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.RawJSON, &probe)
	declared := strings.ToLower(strings.TrimSpace(probe.Type))
	if declared != "" && declared != providerName {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	if declared == "" {
		routed := strings.EqualFold(strings.TrimSpace(req.Provider), providerName)
		prefixed := strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.FileName)), providerName+"-")
		if !routed && !prefixed {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false}), nil
		}
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		// Not a TraeWork credential: let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	// ID stays empty and FileName is echoed back: the host derives the record
	// identity from the file path, so any other ID would create a duplicate
	// record for the same file on the next watcher pass.
	ad := toAuthData(sa, parseAuthMetadataCarrier(req.RawJSON))
	ad.ID = ""
	if fileName := strings.TrimSpace(req.FileName); fileName != "" {
		ad.FileName = fileName
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: ad}), nil
}
