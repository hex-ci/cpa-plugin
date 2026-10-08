package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDeviceKeyPairPEMShapes(t *testing.T) {
	pair, err := newDeviceKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	privBlock, _ := pem.Decode([]byte(pair.privatePEM))
	if privBlock == nil || privBlock.Type != "PRIVATE KEY" {
		t.Fatalf("private key must be a PKCS#8 PEM block, got %+v", privBlock)
	}
	if _, err := x509.ParsePKCS8PrivateKey(privBlock.Bytes); err != nil {
		t.Fatalf("private key does not parse: %v", err)
	}
	pubBlock, _ := pem.Decode([]byte(pair.publicPEM))
	if pubBlock == nil || pubBlock.Type != "PUBLIC KEY" {
		t.Fatalf("public key must be an SPKI PEM block, got %+v", pubBlock)
	}
	pub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		t.Fatalf("public key does not parse: %v", err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key must be ECDSA (the client registers P-256), got %T", pub)
	}
	if ecPub.Curve != elliptic.P256() {
		t.Errorf("curve = %v, want P-256", ecPub.Curve)
	}
}

func TestProofCanonicalLayout(t *testing.T) {
	got := proofCanonical("POST", "/trae/api/v3/oauth/ExchangeToken", "client-1", "refresh-1", 1791444522, "abcdef")
	want := "POST\n/trae/api/v3/oauth/ExchangeToken\nclient-1\nrefresh-1\n1791444522\nabcdef"
	if got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
	if strings.HasSuffix(got, "\n") {
		t.Error("canonical must not end with a separator")
	}
}

// The proof is only useful if the server can verify it with the public key we
// registered, so the test verifies the signature the same way the server must.
func TestDeviceProofVerifiesWithRegisteredPublicKey(t *testing.T) {
	pair, err := newDeviceKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	const clientID, refreshToken = "client-1", "refresh-1"
	proof, err := newDeviceProof(pair.privatePEM, clientID, refreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Timestamp == 0 || int64(time.Now().Unix())-proof.Timestamp > 5 {
		t.Errorf("Timestamp = %d, want the current unix second", proof.Timestamp)
	}
	if len(proof.Nonce) != 32 {
		t.Errorf("Nonce = %q, want 16 random bytes as hex", proof.Nonce)
	}
	signature, err := base64.StdEncoding.DecodeString(proof.Signature)
	if err != nil {
		t.Fatalf("Signature is not base64: %v", err)
	}
	canonical := proofCanonical("POST", oauthExchangePath, clientID, refreshToken, proof.Timestamp, proof.Nonce)
	digest := sha256.Sum256([]byte(canonical))
	pubDER, _ := pem.Decode([]byte(pair.publicPEM))
	pubAny, err := x509.ParsePKIXPublicKey(pubDER.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ecPub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		t.Fatal("public key is not ECDSA")
	}
	if !ecdsa.VerifyASN1(ecPub, digest[:], signature) {
		t.Fatal("signature does not verify against the registered public key")
	}

	// The original signature must not validate a different canonical string
	// (another refresh token produces a different canonical string).
	otherCanonical := proofCanonical("POST", oauthExchangePath, clientID, "different-refresh-token", proof.Timestamp, proof.Nonce)
	otherDigest := sha256.Sum256([]byte(otherCanonical))
	if ecdsa.VerifyASN1(ecPub, otherDigest[:], signature) {
		t.Error("a signature must not validate for a different refresh token")
	}
}

// A host-rewritten record can keep an older public key, so both the login and the
// refresh derive the public half from the private key they sign with.
func TestPublicKeyIsDerivedFromTheSigningKey(t *testing.T) {
	pair, err := newDeviceKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	derived, err := publicKeyFromPrivate(pair.privatePEM)
	if err != nil {
		t.Fatal(err)
	}
	if derived != pair.publicPEM {
		t.Fatal("derived public key must equal the generated one")
	}
	// An RSA record from an earlier plugin build must still be usable.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaPair, err := keyPairFromPrivate(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publicKeyFromPrivate(rsaPair.privatePEM); err != nil {
		t.Fatalf("RSA records must keep working: %v", err)
	}
	if _, err := newDeviceProof(rsaPair.privatePEM, "c", "r"); err != nil {
		t.Fatalf("RSA records must still sign a proof: %v", err)
	}
}

func TestDeviceProofRejectsUnusableKey(t *testing.T) {
	if _, err := newDeviceProof("not a pem", "c", "r"); err == nil {
		t.Error("a non-PEM key must be refused")
	}
	if _, err := newDeviceProof("-----BEGIN PRIVATE KEY-----\nZm9v\n-----END PRIVATE KEY-----\n", "c", "r"); err == nil {
		t.Error("a non-RSA key must be refused")
	}
}

func TestRefreshRefusesRecordsWithoutDeviceKey(t *testing.T) {
	sa := sampleStored(t)
	sa.PrivateKey = ""
	if _, err := refreshAccessToken(sa); err == nil || !strings.Contains(err.Error(), "device key") {
		t.Fatalf("err = %v, want a clear instruction to log in again", err)
	}
}

// TestLiveRefresh is an opt-in end-to-end check against the real endpoint: the
// refresh token is bound to a device, so a wrong proof shows up as
// "20403 Token device not match". It needs a credential whose device private
// key is stored (any record produced by this plugin's login).
//
//	TW_LIVE_CRED=/path/to/auths/traework-<uid>.json go test -run TestLiveRefresh -v
func TestLiveRefresh(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("TW_LIVE_CRED"))
	if path == "" {
		t.Skip("set TW_LIVE_CRED=<auth file with a device_private_key> to run the live refresh")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if sa.PrivateKey == "" {
		t.Fatalf("%s has no device_private_key: log in through the plugin first", path)
	}
	res, err := refreshAccessToken(sa)
	if err != nil {
		t.Fatalf("live refresh failed: %v", err)
	}
	if strings.TrimSpace(res.Token) == "" {
		t.Fatal("live refresh returned no token")
	}
	t.Logf("live refresh ok: access token %d chars, rotated refresh token: %t, expires %s",
		len(res.Token), res.RefreshToken != "" && res.RefreshToken != sa.RefreshToken,
		time.Unix(msToUnixSeconds(res.TokenExpireAt), 0).Format("2006-01-02 15:04"))

	// Keep the credential usable: persist what the host would have written.
	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	doc["access_token"] = res.Token
	if res.RefreshToken != "" {
		doc["refresh_token"] = res.RefreshToken
	}
	if exp := msToUnixSeconds(res.TokenExpireAt); exp > 0 {
		doc["expires_at"] = exp
	}
	updated, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("updated %s with the rotated credential", path)
}
