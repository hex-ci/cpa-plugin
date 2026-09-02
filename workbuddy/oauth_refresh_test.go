package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWithRefreshLockSerializesByAuthIndex(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		withRefreshLock("auth-1", func() {
			close(entered)
			<-release
		})
	}()
	<-entered
	blocked := make(chan struct{})
	go func() {
		withRefreshLock("auth-1", func() { close(blocked) })
	}()
	select {
	case <-blocked:
		t.Fatal("second refresh entered before first released")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("second refresh did not enter after release")
	}
}

func TestHandlePollLoginDeletesStateAfterAccountFailure(t *testing.T) {
	const state = "oauth-account-failure"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/auth/token") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(`{"code":500,"msg":"account unavailable"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	loginStates.Store(state, &loginCtx{client: client, expires: time.Now().Add(time.Minute)})

	_, err := handlePollLogin(mustJSON(map[string]any{"state": state}))
	if err == nil || !strings.Contains(err.Error(), "account lookup failed") {
		t.Fatalf("expected account lookup error, got %v", err)
	}
	if _, ok := loginStates.Load(state); ok {
		t.Fatal("failed account lookup must delete completed login state")
	}
}

func TestBuildLoginAuthRejectsMissingAccountUID(t *testing.T) {
	_, err := buildLoginStoredAuth(tokenData{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600}, accountData{})
	if err == nil || !strings.Contains(err.Error(), "account") {
		t.Fatalf("expected account identity error, got %v", err)
	}
}

func TestExpiryFromExpiresInPreservesOldWhenMissing(t *testing.T) {
	old := time.Now().Add(time.Hour).Unix()
	got := expiryFromExpiresIn(0, old)
	if got != old {
		t.Fatalf("missing expiresIn should preserve old expiry: got %d want %d", got, old)
	}
}

func TestBuildRefreshedAuthJSONPreservesTopLevelMetadata(t *testing.T) {
	phys := []byte(`{"type":"workbuddy","provider":"workbuddy","logo":"old-logo","disabled":true,"note":"keep me","auth":{"accessToken":"old","refreshToken":"rt","expiresAt":999},"account":{"uid":"u1","nickname":"nick"}}`)
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "new", RefreshToken: "new-rt", ExpiresAt: 1234},
		Account: storedAccount{UID: "u1", Nickname: "nick"},
	}
	raw, err := buildRefreshedAuthJSON(phys, sa)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"type", "provider", "logo", "disabled", "note"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("missing top-level %s in %s", key, raw)
		}
	}
	if doc["disabled"] != true || doc["note"] != "keep me" {
		t.Fatalf("metadata not preserved: %s", raw)
	}
	if _, ok := doc["auth"]; !ok {
		t.Fatalf("missing refreshed auth: %s", raw)
	}
}
