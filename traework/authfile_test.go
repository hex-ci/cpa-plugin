package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleStored(t *testing.T) *storedAuth {
	t.Helper()
	return &storedAuth{
		AccessToken:  "access-token-value",
		RefreshToken: "refresh-token-value",
		ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
		APIHost:      apiBaseCN,
		UID:          "9123456789012345",
		Nickname:     "Test User",
		Region:       "CN",
		MachineID:    "machine-1",
		DeviceID:     "device-1",
		ClientID:     oauthClientID,
	}
}

func TestParseStoredRoundTrip(t *testing.T) {
	sa := sampleStored(t)
	raw, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	if got.UID != sa.UID || got.RefreshToken != sa.RefreshToken || got.ExpiresAt != sa.ExpiresAt {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestParseStoredRejectsForeignFiles(t *testing.T) {
	cases := map[string]string{
		"empty":     ``,
		"no token":  `{"uid":"1","expires_at":1}`,
		"not json":  `not json`,
		"workbuddy": `{"auth":{"accessToken":"x"},"account":{"uid":"1"}}`,
	}
	for name, raw := range cases {
		if _, err := parseStored([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestToAuthDataIdentification(t *testing.T) {
	sa := sampleStored(t)
	ad := toAuthData(sa, nil)
	if ad.Provider != providerName {
		t.Errorf("Provider = %q, want %q", ad.Provider, providerName)
	}
	if ad.ID != sa.UID {
		t.Errorf("ID = %q, want the uid %q", ad.ID, sa.UID)
	}
	if ad.FileName != providerName+"-"+sa.UID+".json" {
		t.Errorf("FileName = %q, want the uid-suffixed name", ad.FileName)
	}
	if ad.Label != "Test User" {
		t.Errorf("Label = %q, want the nickname", ad.Label)
	}
	if ad.Metadata["type"] != providerName {
		t.Errorf("metadata type = %v, want %q", ad.Metadata["type"], providerName)
	}
	if ad.Metadata["logo"] != pluginLogoURL {
		t.Errorf("metadata logo = %v, want the in-repo logo", ad.Metadata["logo"])
	}
	storage, err := parseStored(ad.StorageJSON)
	if err != nil || storage.RefreshToken != sa.RefreshToken {
		t.Fatalf("StorageJSON must carry the credential: %v", err)
	}
}

func TestToAuthDataKeepsUserMetadataAndOwnsItsKeys(t *testing.T) {
	sa := sampleStored(t)
	carrier := map[string]any{
		"weight": 7,
		"prefix": "tw",
		"note":   "user note",
		"type":   "something-else",
	}
	ad := toAuthData(sa, carrier)
	if ad.Metadata["weight"] != 7 || ad.Metadata["prefix"] != "tw" {
		t.Errorf("user metadata was dropped: %+v", ad.Metadata)
	}
	if ad.Metadata["type"] != providerName {
		t.Errorf("plugin-owned type was overwritten: %v", ad.Metadata["type"])
	}
	if ad.Metadata["note"] == "user note" {
		t.Error("plugin-owned note must win over a stale carrier value")
	}
}

func TestLabelAndNoteFallbacks(t *testing.T) {
	sa := sampleStored(t)
	sa.Nickname = ""
	ad := toAuthData(sa, nil)
	if !strings.Contains(ad.Label, sa.UID) {
		t.Errorf("Label = %q, want a uid fallback", ad.Label)
	}
	if note := displayNote(sa); !strings.Contains(note, "CN") || !strings.Contains(note, sa.UID) {
		t.Errorf("displayNote = %q, want region and uid", note)
	}
	if note := displayNote(nil); note != "" {
		t.Errorf("displayNote(nil) = %q, want empty", note)
	}
}

func TestSanitizeUIDForFileName(t *testing.T) {
	if got := sanitizeUIDForFileName(" 9123456789012345 "); got != "9123456789012345" {
		t.Errorf("got %q", got)
	}
	if got := sanitizeUIDForFileName("a/b\\c:d*e"); got != "abcde" {
		t.Errorf("path separators must be stripped, got %q", got)
	}
	if got := sanitizeUIDForFileName(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseAuthMetadataCarrierForwardsOnlyUserKeys(t *testing.T) {
	raw := []byte(`{"weight":3,"prefix":"tw","access_token":"secret","note":"n","type":"traework"}`)
	carrier := parseAuthMetadataCarrier(raw)
	if carrier["weight"] != float64(3) || carrier["prefix"] != "tw" || carrier["note"] != "n" {
		t.Fatalf("user keys missing: %+v", carrier)
	}
	if _, leaked := carrier["access_token"]; leaked {
		t.Error("credentials must never be forwarded as metadata")
	}
	if _, leaked := carrier["type"]; leaked {
		t.Error("type is not a user key")
	}
}

func TestParseDisabledFromAuthJSON(t *testing.T) {
	if !parseDisabledFromAuthJSON([]byte(`{"disabled":true}`)) {
		t.Error("disabled flag not read")
	}
	if parseDisabledFromAuthJSON([]byte(`{"disabled":false}`)) {
		t.Error("disabled flag misread")
	}
	if parseDisabledFromAuthJSON(nil) {
		t.Error("empty input must not mean disabled")
	}
}

func TestTokenFingerprintIsNotTheToken(t *testing.T) {
	token := "super-secret-token"
	fp := tokenFingerprint(token)
	if fp == token || strings.Contains(fp, token) {
		t.Fatal("fingerprint must not contain the raw token")
	}
	if fp != tokenFingerprint(token) {
		t.Fatal("fingerprint must be stable")
	}
	if tokenFingerprint("") != "-" {
		t.Fatal("empty token must render as a dash")
	}
}
