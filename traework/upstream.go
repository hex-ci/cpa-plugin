// upstream.go holds the TraeWork CN account API: the OAuth code exchange, the
// token refresh and the profile read. Every call goes through the host HTTP
// bridge (host_bridge.go) so plugin-level proxy setting and CPA routing apply.
package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// exchangeResult is the Result payload of ExchangeToken, shared by the code
// exchange and the refresh call.
type exchangeResult struct {
	Token               string `json:"Token"`
	RefreshToken        string `json:"RefreshToken"`
	TokenExpireAt       int64  `json:"TokenExpireAt"`       // milliseconds
	RefreshExpireAt     int64  `json:"RefreshExpireAt"`     // milliseconds
	TokenExpireDuration int64  `json:"TokenExpireDuration"` // milliseconds
	ClientID            string `json:"ClientID"`
	BoundDeviceID       string `json:"BoundDeviceID"`
	DeviceBindStatus    string `json:"DeviceBindStatus"`
}

// traeEnvelope is the {ResponseMetadata, Result} frame every api.trae.cn
// endpoint answers with.
type traeEnvelope struct {
	Result json.RawMessage `json:"Result"`
}

// ideHeaders builds the header set the CN client sends to api.trae.cn and to
// the agent gateway. The gateway routes on these headers, not on the token.
func ideHeaders(token, uid, machineID, deviceID string) map[string]string {
	return map[string]string{
		"Authorization":        "Cloud-IDE-JWT " + token,
		"X-Cloudide-Token":     token,
		"X-Ide-Token":          token,
		"X-Uid":                uid,
		"X-App-Id":             appID,
		"X-App-Version":        "default",
		"X-Ide-Version":        ideVersion,
		"X-Ide-Version-Code":   ideVersionCode,
		"X-App-Version-Code":   ideVersionCode,
		"X-Ide-Version-Type":   "stable",
		"X-Device-Type":        deviceType,
		"X-OS-Version":         osVersion,
		"X-Device-Brand":       deviceBrand,
		"Request-Traffic-Type": "prod",
		"X-Machine-Id":         machineID,
		"X-Device-Id":          deviceID,
		"User-Agent":           ideUserAgent,
	}
}

func postJSON(url string, headers map[string]string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hostHTTPDo(req)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("decode upstream response: %w", err)
	}
	return nil
}

// postJSONResult posts and unwraps the trae Result field into out.
func postJSONResult(url string, headers map[string]string, body any, out any) error {
	var env traeEnvelope
	if err := postJSON(url, headers, body, &env); err != nil {
		return err
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return errors.New("upstream response carried no Result")
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("decode upstream Result: %w", err)
	}
	return nil
}

// Network entry points are variables so the login state machine is testable
// without reaching upstream.
var (
	exchangeAuthCodeFn = exchangeAuthCode
	fetchProfileFn     = fetchProfile
	refreshTokenFn     = refreshAccessToken
)

// deviceKeyPair is the device identity registered with the server: the public
// half is sent at login, the private half signs the proof every refresh needs.
// The desktop client uses one pair per installation (ECDSA P-256, PKCS#8/SPKI
// PEM) and reuses it for every account.
type deviceKeyPair struct {
	privatePEM string
	publicPEM  string
}

// newDeviceKeyPair generates the pair the client generates.
func newDeviceKeyPair() (*deviceKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return keyPairFromPrivate(key)
}

// keyPairFromPrivate derives both PEM blocks from a private key, so the public
// half always matches the key that signs: a stored record can carry a stale
// public key (the host keeps existing fields on re-login), and the server
// verifies the proof against the key registered at login.
func keyPairFromPrivate(key crypto.Signer) (*deviceKeyPair, error) {
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, err
	}
	return &deviceKeyPair{
		privatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})),
		publicPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
	}, nil
}

// publicKeyFromPrivate returns the SPKI PEM matching a stored private key.
func publicKeyFromPrivate(privateKeyPEM string) (string, error) {
	key, err := parseDevicePrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})), nil
}

// parseDevicePrivateKey accepts the PKCS#8 form the client stores, plus PKCS#1
// RSA for records written by earlier builds of this plugin.
func parseDevicePrivateKey(privateKeyPEM string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, errors.New("device private key is not PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := parsed.(crypto.Signer); ok {
			return signer, nil
		}
	}
	if rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return rsaKey, nil
	}
	return nil, errors.New("device private key is not a supported key type")
}

// deviceProof is the signature block the refresh call carries.
type deviceProof struct {
	Signature string `json:"Signature"`
	Timestamp int64  `json:"Timestamp"`
	Nonce     string `json:"Nonce"`
}

// proofCanonical is the exact string the server verifies, newline-joined with no
// trailing separator (mirrors the desktop client's signer).
func proofCanonical(method, path, clientID, refreshToken string, timestamp int64, nonce string) string {
	return strings.Join([]string{method, path, clientID, refreshToken, strconv.FormatInt(timestamp, 10), nonce}, "\n")
}

// newDeviceProof signs the canonical string with the stored device key. The
// client signs with ECDSA-SHA256 and sends the DER signature base64-encoded.
func newDeviceProof(privateKeyPEM, clientID, refreshToken string) (*deviceProof, error) {
	key, err := parseDevicePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, err
	}
	timestamp := time.Now().Unix()
	nonce := hex.EncodeToString(nonceBytes)
	canonical := proofCanonical(http.MethodPost, oauthExchangePath, clientID, refreshToken, timestamp, nonce)
	digest := sha256.Sum256([]byte(canonical))
	var signature []byte
	switch typed := key.(type) {
	case *ecdsa.PrivateKey:
		signature, err = ecdsa.SignASN1(rand.Reader, typed, digest[:])
	case *rsa.PrivateKey:
		signature, err = rsa.SignPKCS1v15(rand.Reader, typed, crypto.SHA256, digest[:])
	default:
		return nil, fmt.Errorf("unsupported device key type %T", key)
	}
	if err != nil {
		return nil, err
	}
	return &deviceProof{
		Signature: base64.StdEncoding.EncodeToString(signature),
		Timestamp: timestamp,
		Nonce:     nonce,
	}, nil
}

// deviceInfoPayload is the DeviceInfo block the client sends on both the code
// exchange and the refresh.
func deviceInfoPayload(publicKey, machineID, deviceID string) map[string]any {
	hostname, _ := os.Hostname()
	return map[string]any{
		"DeviceID":        deviceID,
		"MachineID":       machineID,
		"PlatformCode":    "SOLO_PC",
		"DeviceType":      "PC",
		"DeviceName":      hostname,
		"DeviceModel":     "",
		"ClientVersion":   ideVersion,
		"DevicePublicKey": publicKey,
		"DeviceBrand":     "",
		"DeviceCPU":       "",
		"OSInfo":          "Windows",
		"OSVersion":       osVersion,
	}
}

// exchangeAuthCode trades the browser authorization code for tokens. The PKCE
// verifier and the device public key must be the ones the authorize URL was
// built with.
func exchangeAuthCode(code, verifier, publicKey, machineID, deviceID string) (*exchangeResult, error) {
	body := map[string]any{
		"ClientID":     oauthClientID,
		"AuthCode":     code,
		"CodeVerifier": verifier,
		"IDEVersion":   ideVersion,
		"DeviceInfo":   deviceInfoPayload(publicKey, machineID, deviceID),
	}
	var res exchangeResult
	headers := map[string]string{"User-Agent": ideUserAgent}
	if err := postJSONResult(apiBaseCN+oauthExchangePath, headers, body, &res); err != nil {
		return nil, err
	}
	if strings.TrimSpace(res.Token) == "" {
		return nil, errors.New("ExchangeToken returned no access token")
	}
	return &res, nil
}

// refreshAccessToken rotates the access token. The refresh token is bound to the
// device that created it, so the request carries the device identity plus an
// RS256 proof signed with the stored device key; without that key the server
// answers 20403 "Token device not match".
func refreshAccessToken(sa *storedAuth) (*exchangeResult, error) {
	if strings.TrimSpace(sa.PrivateKey) == "" {
		return nil, errors.New("auth record has no device key; log in again to register one")
	}
	proof, err := newDeviceProof(sa.PrivateKey, oauthClientID, sa.RefreshToken)
	if err != nil {
		return nil, err
	}
	// The public key is derived from the signing key: a record rewritten by the
	// host can keep an older public key, and the server verifies the proof
	// against the key that login registered.
	publicKey, err := publicKeyFromPrivate(sa.PrivateKey)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"ClientID":     oauthClientID,
		"ClientSecret": "",
		"RefreshToken": sa.RefreshToken,
		"DeviceInfo":   deviceInfoPayload(publicKey, sa.MachineID, sa.DeviceID),
		"DeviceProof":  proof,
		"IDEVersion":   ideVersion,
	}
	var res exchangeResult
	// The client sends exactly these two headers on a refresh.
	headers := map[string]string{"x-cloudide-token": sa.AccessToken}
	if err := postJSONResult(apiBaseCN+oauthExchangePath, headers, body, &res); err != nil {
		return nil, err
	}
	if strings.TrimSpace(res.Token) == "" {
		return nil, errors.New("refresh returned no access token")
	}
	return &res, nil
}

// fetchProfile reads the account nickname and avatar. Best effort: the panel
// falls back to the uid when this fails.
func fetchProfile(sa *storedAuth) (nickname, avatar string) {
	if sa == nil || sa.AccessToken == "" {
		return "", ""
	}
	body := map[string]any{"IDEVersion": ideVersion, "ReqSource": "Lite"}
	var res struct {
		Nickname   string `json:"Nickname"`
		ScreenName string `json:"ScreenName"`
		AvatarURL  string `json:"AvatarUrl"`
	}
	headers := ideHeaders(sa.AccessToken, sa.UID, sa.MachineID, sa.DeviceID)
	if err := postJSONResult(apiBaseCN+oauthUserInfoPath, headers, body, &res); err != nil {
		return "", ""
	}
	name := strings.TrimSpace(res.Nickname)
	if name == "" {
		name = strings.TrimSpace(res.ScreenName)
	}
	return name, strings.TrimSpace(res.AvatarURL)
}

// accountIdentityFromToken reads the uid and tenant out of the access token
// payload. The payload is not verified: it only names the auth file and labels
// the account, and the token itself is what the gateway checks.
func accountIdentityFromToken(token string) (uid, tenantID string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Data struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", ""
	}
	return strings.TrimSpace(claims.Data.ID), strings.TrimSpace(claims.Data.TenantID)
}

// msToUnixSeconds converts the millisecond timestamps these endpoints use.
func msToUnixSeconds(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return ms / 1000
}

// expiresSoon reports whether the access token needs refreshing before use.
func expiresSoon(expiresAt int64, window time.Duration) bool {
	if expiresAt <= 0 {
		return true
	}
	return time.Now().Add(window).Unix() >= expiresAt
}
