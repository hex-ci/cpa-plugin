package main

import (
	"encoding/json"
	"testing"
)

func TestRefreshMetadataPreservesUserConfiguredFields(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", RefreshToken: "drt-test", Domain: "api.qoder.com"},
		Account: storedAccount{UID: "u1", Nickname: "nick"},
	}
	hostMetadata := map[string]any{
		"type":          "qoder",
		"note":          "stale note from host",
		"disabled":      false,
		"weight":        float64(7),
		"priority":      float64(3),
		"proxy_url":     "socks5://127.0.0.1:7897",
		"prefix":        "qw",
		"websockets":    true,
		"request_retry": float64(2),
	}
	ad := toAuthDataForRefresh(sa, hostMetadata)

	if got, want := ad.Metadata["weight"], float64(7); got != want {
		t.Errorf("weight = %#v, want %#v", got, want)
	}
	if got, want := ad.Metadata["priority"], float64(3); got != want {
		t.Errorf("priority = %#v, want %#v", got, want)
	}
	if got, want := ad.Metadata["proxy_url"], "socks5://127.0.0.1:7897"; got != want {
		t.Errorf("proxy_url = %#v, want %#v", got, want)
	}
	if got, want := ad.Metadata["prefix"], "qw"; got != want {
		t.Errorf("prefix = %#v, want %#v", got, want)
	}
	if got, want := ad.Metadata["websockets"], true; got != want {
		t.Errorf("websockets = %#v, want %#v", got, want)
	}
	if got := ad.Metadata["note"]; got == "stale note from host" {
		t.Errorf("note was not refreshed: %#v", got)
	}
	if got := ad.Metadata["type"]; got != providerName {
		t.Errorf("type = %#v, want %q", got, providerName)
	}
	if got, ok := ad.Metadata["disabled"]; !ok || got != false {
		t.Errorf("disabled = %#v, want false", got)
	}
	if ad.FileName != "" || ad.ID != "" {
		t.Errorf("refresh identity must stay empty: fileName=%q id=%q", ad.FileName, ad.ID)
	}
}

func TestRefreshWithoutHostMetadataStillBuildsAuthData(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "at"}, Account: storedAccount{UID: "u1"}}
	ad := toAuthDataForRefresh(sa, nil)
	if ad.Metadata["type"] != providerName {
		t.Fatalf("metadata = %#v", ad.Metadata)
	}
	if _, ok := ad.Metadata["weight"]; ok {
		t.Fatal("weight must not be invented when the host sent none")
	}
}

func TestParseAuthCarrierKeepsOnlyUserOwnedKeys(t *testing.T) {
	raw := []byte(`{
		"type":"qoder","provider":"qoder","logo":"l","disabled":false,
		"note":"CN · 余1 已用2 池3",
		"weight":5,"priority":1,"proxy_url":"http://p:1","prefix":"qw",
		"headers":{"X-Test":"1"},"request_retry":3,"websockets":true,
		"auth":{"accessToken":"secret","refreshToken":"secret"},"account":{"uid":"u1"}
	}`)
	carrier := parseAuthMetadataCarrier(raw)
	for _, key := range []string{"weight", "priority", "proxy_url", "prefix", "headers", "request_retry", "websockets", "note"} {
		if _, ok := carrier[key]; !ok {
			t.Errorf("carrier lost %q", key)
		}
	}
	for _, key := range []string{"auth", "account", "type", "provider", "logo", "disabled"} {
		if _, ok := carrier[key]; ok {
			t.Errorf("carrier must not carry %q", key)
		}
	}
}

func TestParseAuthCarrierRoundTripsThroughAuthData(t *testing.T) {
	raw := []byte(`{"type":"qoder","note":"n","weight":9,"prefix":"qw","proxy_url":"http://p:2","auth":{"accessToken":"at","refreshToken":"rt"},"account":{"uid":"u1"}}`)
	carrier := parseAuthMetadataCarrier(raw)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatal(err)
	}
	ad := toAuthDataOpts(sa, nil, false, carrier)
	encoded, err := json.Marshal(ad.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	second := parseAuthMetadataCarrier(encoded)
	if second["weight"] != float64(9) || second["prefix"] != "qw" || second["proxy_url"] != "http://p:2" {
		t.Fatalf("second pass lost user config: %#v", second)
	}
}

func TestBuildAuthFileJSONPreservesExistingPhysicalFields(t *testing.T) {
	physical := []byte(`{"type":"qoder","prefix":"qw","weight":5,"proxy_url":"socks5://127.0.0.1:1080","auth":{"accessToken":"old_at","refreshToken":"old_rt"},"account":{"uid":"u1"}}`)
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "new_at", RefreshToken: "new_rt"},
		Account: storedAccount{UID: "u1"},
	}
	raw, err := buildAuthFileJSON(physical, sa, false, "CN · note", nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc["prefix"]; got != "qw" {
		t.Errorf("prefix = %v, want qw", got)
	}
	if got := doc["weight"]; got != float64(5) {
		t.Errorf("weight = %v, want 5", got)
	}
	if got := doc["proxy_url"]; got != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy_url = %v, want socks5://127.0.0.1:1080", got)
	}
}

func TestBuildRefreshedAuthJSONPreservesExistingPhysicalFields(t *testing.T) {
	physical := []byte(`{"type":"qoder","prefix":"qw","weight":5,"proxy_url":"socks5://127.0.0.1:1080","auth":{"accessToken":"old_at","refreshToken":"old_rt"},"account":{"uid":"u1"}}`)
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "new_at", RefreshToken: "new_rt"},
		Account: storedAccount{UID: "u1"},
	}
	raw, err := buildRefreshedAuthJSON(physical, sa)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc["prefix"]; got != "qw" {
		t.Errorf("prefix = %v, want qw", got)
	}
	if got := doc["weight"]; got != float64(5) {
		t.Errorf("weight = %v, want 5", got)
	}
}
