// sign.go implements Qoder's COSY request signing: RSA-wrapped AES session
// key + AES-128-CBC encrypted identity + MD5 request signature.
//
// This mirrors the official CLI's signing WASM: the payload, the five-part
// signature string and the header set are all reproduced byte for byte. What
// must be signed is what gets sent — the body passed to buildBearer is the
// encoded body that goes on the wire.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// serverPubKeyPEM is Qoder's RSA public key, hardcoded in the official client
// bundle. It wraps the per-session AES key.
const serverPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// cosyUserAgent is what the official CLI's inference calls carry: the signing
// WASM's header table has no User-Agent, and requests go out through its
// bundled undici fetch, which fills in its own name.
const cosyUserAgent = "undici"

var serverPubKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(serverPubKeyPEM))
	if block == nil {
		panic("bad server pubkey PEM")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	serverPubKey = k.(*rsa.PublicKey)
}

// cosySession holds the per-credential signing state. One session is created
// per auth credential at first use and reused until the access token rotates
// (which rebuilds `info`).
//
// The machine identity is derived from the account uid, never randomised:
// upstream treats a credential whose device fingerprint changes between
// requests as a different machine, which is exactly the signal that raises
// risk scores.
type cosySession struct {
	MachineID    string `json:"machine_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
	Hostname     string `json:"hostname"`
	MachineOS    string `json:"machine_os"`
	TempKey      string `json:"temp_key"` // 16 ASCII chars, AES-128 key
	CosyKey      string `json:"cosy_key"` // base64(RSA(tempKey))
	Info         string `json:"info"`     // base64(AES-CBC(identityJSON, tempKey))
}

// cosyIdentity is the plaintext inside `info`. All fields required by server.
type cosyIdentity struct {
	Name               string `json:"name"`
	AID                string `json:"aid"`
	UID                string `json:"uid"`
	YxUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOauthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

// jsonSortedCompact marshals a map with sorted keys and no whitespace,
// matching the compact form the server expects.
func jsonSortedCompact(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String())
}

// aesCBCEncrypt encrypts plain with AES-128-CBC, key=iv=tempKey (16 bytes).
func aesCBCEncrypt(plain, tempKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(tempKey)
	if err != nil {
		return nil, err
	}
	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+padLen)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, tempKey).CryptBlocks(out, padded)
	return out, nil
}

// deriveUUID produces a stable UUID-shaped identifier from a seed. Version and
// variant nibbles are forced so the value parses as a UUID everywhere.
func deriveUUID(seed string) string {
	sum := md5.Sum([]byte(seed))
	h := hex.EncodeToString(sum[:])
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

// deriveMachineToken produces the 43-character base64url machine token.
func deriveMachineToken(uid string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + uid))
	return strings.TrimRight(base64.URLEncoding.EncodeToString(sum[:]), "=")[:43]
}

// machineHostname produces a stable DESKTOP-XXXXXXX host name per account.
func machineHostname(uid string) string {
	sum := md5.Sum([]byte("hostname:" + uid))
	return "DESKTOP-" + strings.ToUpper(hex.EncodeToString(sum[:]))[:7]
}

// machineOSValue returns the cosy-machineos value: `{arch}_{platform}`.
// Config can pin it (a Linux deployment may prefer to look like the client
// build it mirrors); otherwise it follows the host.
func machineOSValue() string {
	if v := strings.TrimSpace(currentConfig().CosyMachineOS); v != "" {
		return v
	}
	arch := "x86_64"
	switch runtime.GOARCH {
	case "arm64":
		arch = "aarch64"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return arch + "_" + platform
}

// loginMachineID is the machine_id sent to the device-authorization page. No
// account is known yet, so it is derived from the host identity: stable across
// restarts, distinct between machines.
func loginMachineID() string {
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	return deriveUUID("qoder-login:" + host + ":" + user)
}

// randomHex returns n random bytes as lowercase hex (2n chars).
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	return hex.EncodeToString(buf)
}

// newCosySession builds a fresh session for the given identity.
func newCosySession(id cosyIdentity) (*cosySession, error) {
	uid := id.UID
	if uid == "" {
		uid = "anonymous"
	}
	tempKey := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]

	//lint:ignore SA1019 Cosy decrypts this field with PKCS#1 v1.5; OAEP would break login.
	cosyKeyBytes, err := rsa.EncryptPKCS1v15(rand.Reader, serverPubKey, []byte(tempKey))
	if err != nil {
		return nil, fmt.Errorf("rsa encrypt: %w", err)
	}
	cosyKey := base64.StdEncoding.EncodeToString(cosyKeyBytes)

	identityMap := map[string]string{
		"name":                 id.Name,
		"aid":                  id.AID,
		"uid":                  id.UID,
		"yx_uid":               id.YxUID,
		"organization_id":      id.OrganizationID,
		"organization_name":    id.OrganizationName,
		"user_type":            id.UserType,
		"security_oauth_token": id.SecurityOauthToken,
		"refresh_token":        id.RefreshToken,
	}
	identityJSON := jsonSortedCompact(identityMap)
	infoBytes, err := aesCBCEncrypt(identityJSON, []byte(tempKey))
	if err != nil {
		return nil, fmt.Errorf("aes encrypt: %w", err)
	}

	return &cosySession{
		MachineID:    deriveUUID("machine:" + uid),
		MachineToken: deriveMachineToken(uid),
		MachineType:  strings.ReplaceAll(deriveUUID("machinetype:"+uid), "-", "")[:18],
		Hostname:     machineHostname(uid),
		MachineOS:    machineOSValue(),
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         base64.StdEncoding.EncodeToString(infoBytes),
	}, nil
}

// buildBearer constructs the Authorization header for one request.
// The signed path drops the "/algo" prefix and excludes the query string;
// body is the encoded body that goes on the wire.
func (s *cosySession) buildBearer(body, rawURL string) (payloadB64, date, bearer string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", err
	}
	pathSig := strings.TrimPrefix(u.Path, "/algo")
	payload := map[string]string{
		"cosyVersion": cosyVersion(),
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   uuid.NewString(),
		"version":     "v1",
	}
	payloadB64 = base64.StdEncoding.EncodeToString(jsonSortedCompact(payload))
	date = fmt.Sprintf("%d", time.Now().Unix())
	sum := md5.Sum([]byte(payloadB64 + "\n" + s.CosyKey + "\n" + date + "\n" + body + "\n" + pathSig))
	sig := hex.EncodeToString(sum[:])
	bearer = "Bearer COSY." + payloadB64 + "." + sig
	return payloadB64, date, bearer, nil
}

// headers returns the full header set for one inference or catalog request.
// x-model-key / x-model-source are merged in by the caller.
func (s *cosySession) headers(uid, body, rawURL, accept string, sse bool) (map[string]string, error) {
	_, date, bearer, err := s.buildBearer(body, rawURL)
	if err != nil {
		return nil, err
	}
	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-business-product": "cli",
		"cosy-business-type":    "agent",
		"cosy-scene":            "assistant",
		"cosy-machinetype":      s.MachineType,
		"cosy-machineos":        s.MachineOS,
		"cosy-clienttype":       "5",
		"cosy-date":             date,
		"cosy-user":             uid,
		"cosy-key":              s.CosyKey,
		"accept":                accept,
		"cosy-clientip":         "169.254.198.161",
		"authorization":         bearer,
		"accept-encoding":       "identity", // gzip would break body/stream verification
		"cosy-version":          cosyVersion(),
		"cosy-machineid":        s.MachineID,
		"cosy-machinetoken":     s.MachineToken,
		"login-version":         "v2",
		"user-agent":            cosyUserAgent,
	}
	if sse {
		h["cache-control"] = "no-cache"
		h["connection"] = "keep-alive"
		// The host name is only sent on the inference stream; the catalog
		// request omits it.
		h["cosy-machinehostname"] = s.Hostname
	}
	return h, nil
}
