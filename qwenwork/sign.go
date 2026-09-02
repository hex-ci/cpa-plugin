// sign.go implements QwenWork's COSY request signing: RSA-wrapped AES session
// key + AES-128-CBC encrypted identity + MD5 request signature. The algorithm
// is the SAME Qoder scheme qoderwork uses (same RSA public key), but qwenwork
// passes a different cosyVersion/ideVersion, a 5-field identity, and a
// different header set (X-QwenWork-* + title-case Cosy-*). Pure-Go port of
// Buddy2api providers/qwenwork/cosy.py (which mirrors the official 0.1.8 asar
// generateAuthToken).
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// serverPubKeyPEM is Qoder's RSA public key, hardcoded in the desktop client
// and identical for qwenwork. Wraps the per-session AES key.
const serverPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

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

// qwenwork protocol constants (official QwenWorkCN 0.1.8).
const (
	cosyVersionQwen     = "1.1.18"
	ideVersionQwen      = "0.1.8"
	qwenReleaseVersion  = "0.1.8-26081406"
	qwenBuild           = "26081406"
	qwenClientType      = "6"
	qwenBusinessProduct = "qoder_work"
	qwenBusinessType    = "agent"
	qwenScene           = "qwork"
	qwenMachineOS       = "x86_64_win32"
)

// cosySession holds the per-process signing state. One session is created per
// auth credential at first use and reused until the process exits or the
// underlying token is refreshed (which rebuilds `info`).
type cosySession struct {
	MachineID    string `json:"machine_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
	TempKey      string `json:"temp_key"` // 16 ASCII chars, AES-128 key
	CosyKey      string `json:"cosy_key"` // base64(RSA(tempKey))
	Info         string `json:"info"`     // base64(AES-CBC(identityJSON, tempKey))
}

// cosyIdentity is the plaintext inside `info`. qwenwork carries 5 fields (no
// organization/user_type/yx_uid/refresh_token — unlike qoderwork).
type cosyIdentity struct {
	Name               string
	AID                string
	UID                string
	Email              string
	SecurityOauthToken string
}

// jsonSortedCompact marshals a map with sorted keys and no whitespace,
// matching the compact form the server expects for the signing payload.
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

// newCosySession builds a fresh session for the given identity.
func newCosySession(id cosyIdentity) (*cosySession, error) {
	machineID := uuid.NewString()
	seed := []byte(uuid.NewString() + uuid.NewString())
	if len(seed) > 50 {
		seed = seed[:50]
	}
	machineToken := base64.RawURLEncoding.EncodeToString(seed)
	machineType := strings.ReplaceAll(uuid.NewString(), "-", "")[:18]
	tempKey := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]

	cosyKeyBytes, err := rsa.EncryptPKCS1v15(rand.Reader, serverPubKey, []byte(tempKey))
	if err != nil {
		return nil, fmt.Errorf("rsa encrypt: %w", err)
	}
	cosyKey := base64.StdEncoding.EncodeToString(cosyKeyBytes)

	// info order is stablely sorted; it sits inside AES-CBC ciphertext so key
	// ordering has no signature effect — the server decrypts and JSON-parses.
	identityMap := map[string]string{
		"uid":                  id.UID,
		"aid":                  id.AID,
		"name":                 id.Name,
		"email":                id.Email,
		"security_oauth_token": id.SecurityOauthToken,
	}
	identityJSON := jsonSortedCompact(identityMap)
	infoBytes, err := aesCBCEncrypt(identityJSON, []byte(tempKey))
	if err != nil {
		return nil, fmt.Errorf("aes encrypt: %w", err)
	}
	info := base64.StdEncoding.EncodeToString(infoBytes)

	return &cosySession{
		MachineID:    machineID,
		MachineToken: machineToken,
		MachineType:  machineType,
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         info,
	}, nil
}

// buildBearer constructs the Authorization header for one request. pathSig is
// url.Path with the "/algo" prefix stripped; body is the raw (plain JSON)
// request body string — qwenwork signs the raw JSON directly (no QoderEncoding).
func (s *cosySession) buildBearer(body, rawURL string) (payloadB64, date, bearer string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", err
	}
	pathSig := u.Path
	if strings.HasPrefix(pathSig, "/algo") {
		pathSig = pathSig[len("/algo"):]
	}
	payload := map[string]string{
		"cosyVersion": cosyVersionQwen,
		"ideVersion":  ideVersionQwen,
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

// headers returns the full header set for one inference request. extra headers
// (x-model-key, x-model-source) are merged in by the caller.
func (s *cosySession) headers(uid, body, rawURL, accept string, sse bool) (map[string]string, error) {
	_, date, bearer, err := s.buildBearer(body, rawURL)
	if err != nil {
		return nil, err
	}
	h := map[string]string{
		"accept":                     accept,
		"content-type":               "application/json",
		"user-agent":                 clientUA,
		"x-request-id":               uuid.NewString(),
		"x-qwenwork-version":         ideVersionQwen,
		"x-qwenwork-release-version": qwenReleaseVersion,
		"x-qwenwork-build":           qwenBuild,
		"x-qwenwork-platform":        "win32",
		"x-qwenwork-arch":            "x64",
		"x-qwenwork-channel":         "stable",
		"cosy-version":               cosyVersionQwen,
		"cosy-clienttype":            qwenClientType,
		"cosy-business-product":      qwenBusinessProduct,
		"cosy-business-type":         qwenBusinessType,
		"cosy-scene":                 qwenScene,
		"cosy-machineos":             qwenMachineOS,
		"login-version":              "v2",
		"authorization":              bearer,
		"cosy-key":                   s.CosyKey,
		"cosy-user":                  uid,
		"cosy-date":                  date,
		"cosy-machineid":             s.MachineID,
		"accept-encoding":            "identity",
		"connection":                 "keep-alive",
		"cache-control":              "no-cache",
	}
	_ = sse // cache-control is always no-cache for SSE-style inference
	return h, nil
}
