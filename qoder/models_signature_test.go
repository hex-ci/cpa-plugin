package main

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The catalog request's COSY signature covers the body, so the bytes signed
// must be the bytes sent. Two client generations disagree about what that body
// is: the CLI sends the encoded empty object ("$kwm") and rejects a bare GET,
// older shells sign an empty body. model_list_body selects which one, and these
// tests pin both — plus the invariant that a mismatch is never silently
// accepted.

func stubCatalogServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func testStoredAuth() *storedAuth {
	sa := &storedAuth{}
	sa.Auth.AccessToken = "dt-test-header.dt-test-payload.dt-test-signature"
	sa.Account.UID = "uid-signature-test"
	return sa
}

func catalogRequestURL(server *httptest.Server) string {
	return server.URL + "/algo/api/v2/model/list?Encode=1"
}

// withModelListBody pins the catalog body mode for one test and restores it.
func withModelListBody(t *testing.T, mode string) {
	t.Helper()
	cfgMu.Lock()
	old := cfg.ModelListBody
	cfg.ModelListBody = mode
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg.ModelListBody = old
		cfgMu.Unlock()
	})
}

// verifyCatalogSignature recomputes the header signature over the body the
// server actually received.
func verifyCatalogSignature(t *testing.T, sa *storedAuth, gotAuth, gotDate, sentBody, path string) {
	t.Helper()
	sess, err := cosySessionFor(sa)
	if err != nil {
		t.Fatalf("cosy session: %v", err)
	}
	parts := strings.Split(gotAuth, ".")
	if len(parts) != 3 || parts[0] != "Bearer COSY" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("authorization shape = %q", gotAuth)
	}
	sum := md5.Sum([]byte(parts[1] + "\n" + sess.CosyKey + "\n" + gotDate + "\n" + sentBody + "\n" + path))
	if want := hex.EncodeToString(sum[:]); want != parts[2] {
		t.Fatalf("signature over the sent body = %s, header carries %s", want, parts[2])
	}
}

func TestCatalogSignatureCoversKWMBody(t *testing.T) {
	withModelListBody(t, modelListBodyKWM)

	var sentBody, gotAuth, gotDate, gotPath string
	server := stubCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			sentBody = string(raw)
		}
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("cosy-date")
		gotPath = strings.TrimPrefix(r.URL.Path, "/algo")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":[{"key":"fresh-model","display_name":"Fresh","enable":true,"max_input_tokens":180000}]}`))
	})

	sa := testStoredAuth()
	entries, err := fetchCatalogFrom(sa, catalogRequestURL(server))
	if err != nil {
		t.Fatalf("fetchCatalogFrom: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != "fresh-model" {
		t.Fatalf("entries = %#v", entries)
	}
	if sentBody != qoderEncode([]byte("{}")) {
		t.Fatalf("catalog body = %q, want the encoded empty object", sentBody)
	}
	verifyCatalogSignature(t, sa, gotAuth, gotDate, sentBody, gotPath)
}

func TestCatalogSignatureCoversEmptyBodyWhenConfigured(t *testing.T) {
	withModelListBody(t, modelListBodyEmpty)

	var sentBody, gotAuth, gotDate, gotPath string
	server := stubCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			sentBody = string(raw)
		}
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("cosy-date")
		gotPath = strings.TrimPrefix(r.URL.Path, "/algo")
		_, _ = w.Write([]byte(`{"chat":[{"key":"enabled-model","display_name":"Enabled","enable":true,"max_input_tokens":200000}]}`))
	})

	sa := testStoredAuth()
	if _, err := fetchCatalogFrom(sa, catalogRequestURL(server)); err != nil {
		t.Fatalf("fetchCatalogFrom: %v", err)
	}
	if sentBody != "" {
		t.Fatalf("catalog body = %q, want nothing sent in empty mode", sentBody)
	}
	verifyCatalogSignature(t, sa, gotAuth, gotDate, sentBody, gotPath)
}

func TestCatalogFailsWhenSignatureIsRejected(t *testing.T) {
	server := stubCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"101","message":"Signature invalid"}`))
	})

	if _, err := fetchCatalogFrom(testStoredAuth(), catalogRequestURL(server)); err == nil {
		t.Fatal("a rejected signature was reported as success")
	}
}

func TestCatalogParseDropsDisabledAndNonChat(t *testing.T) {
	server := stubCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"chat":[` +
			`{"key":"enabled-model","display_name":"Enabled","enable":true,"max_input_tokens":200000},` +
			`{"key":"disabled-model","display_name":"Disabled","enable":false,"max_input_tokens":200000},` +
			`{"key":"lite","display_name":"Lite","enable":true},` +
			`{"key":"codewise-autocomplete","enable":true},` +
			`{"key":"foo-image-alpha","enable":true}]}`))
	})

	entries, err := fetchCatalogFrom(testStoredAuth(), catalogRequestURL(server))
	if err != nil {
		t.Fatalf("fetchCatalogFrom: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v, want the enabled chat model and the disabled chat model", entries)
	}
	if entries[0].Key != "enabled-model" || entries[1].Key != "disabled-model" {
		t.Fatalf("entries = %#v", entries)
	}
	if !entries[1].Disabled {
		t.Fatal("an upstream-disabled model must be flagged, not dropped silently")
	}
	if info := entries[1].modelInfo(); info.ID != "disabled-model" {
		t.Fatalf("modelInfo = %#v", info)
	}
}

func TestCatalogEntriesExposeThinkingLevels(t *testing.T) {
	raw := `{"chat":[{"key":"qmodel_38max","display_name":"Qwen3.8-Max","enable":true,
		"is_reasoning":true,"max_input_tokens":180000,
		"context_config":{"200K":{"token_count":200000,"is_default":true},"1M":{"token_count":1000000}},
		"thinking_config":{"disabled":{},"enabled":{"efforts":{"low":{},"medium":{"is_default":true},"xhigh":{}}}}}]}`
	entries, err := parseCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	e := entries[0]
	levels, def := e.thinkingLevels()
	if strings.Join(levels, ",") != "low,medium,xhigh" {
		t.Fatalf("levels = %v", levels)
	}
	if def != "medium" {
		t.Fatalf("default level = %q, want medium", def)
	}
	if e.contextWindow() != 200000 {
		t.Fatalf("context window = %d, want the default 200K", e.contextWindow())
	}
	info := e.modelInfo()
	if info.Thinking == nil || !info.Thinking.ZeroAllowed {
		t.Fatalf("thinking support = %#v, want toggleable levels", info.Thinking)
	}
}
