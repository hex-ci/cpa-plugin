//go:build live

// Opt-in live probe for the catalog request shape. Skipped unless
// QODER_LIVE_AUTH points at an auth file; never runs in CI or `make test`.
//
// It answers one question the code cannot: which body the gateway expects on
// the catalog GET. Upstream is inconsistent across client generations — the
// CLI sends the encoded empty object and rejects a bare GET, older shells sign
// an empty body — and only a real credential can settle which one this account
// path accepts.
//
//	QODER_LIVE_AUTH=~/.cli-proxy-api/qoder-<uid>.json \
//	  go test -tags live -run TestLiveCatalog -v .

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveCatalogBodyModes(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("QODER_LIVE_AUTH"))
	if path == "" {
		t.Skip("QODER_LIVE_AUTH not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	r := realmOf(sa)
	t.Logf("realm=%s host=%s cosy=%s", r.ID, r.Gateway, cosyVersion())
	if os.Getenv("QODER_LIVE_FETCHKEYS") != "" {
		rawURL := r.modelsURL() + "&FetchKeys=llm_model_result,strategies,model_config"
		entries, ferr := fetchCatalogFrom(sa, rawURL)
		if ferr != nil {
			t.Logf("fetchkeys catalog error: %v", ferr)
		}
		for _, e := range entries {
			if e.Key == "efficient" || e.Key == "auto" {
				b, _ := json.Marshal(e)
				t.Logf("fetchkeys %s: %s", e.Key, b)
			}
		}
	}
	if os.Getenv("QODER_LIVE_DUMP_ALL") != "" {
		entries, ferr := fetchCatalogFrom(sa, r.modelsURL())
		if ferr != nil {
			t.Logf("catalog fetch error: %v", ferr)
		}
		for _, e := range entries {
			t.Logf("model key=%s enable=%t default=%t free=%t vl=%t reason=%t fmt=%s src=%s name=%s",
				e.Key, e.Enable, e.IsDefault, e.IsFree, e.IsVL, e.IsReasoning, e.Format, e.Source, e.DisplayName)
		}
	}
	if want := strings.TrimSpace(os.Getenv("QODER_LIVE_DUMP_MODEL")); want != "" {
		entries, ferr := fetchCatalogFrom(sa, r.modelsURL())
		if ferr != nil {
			t.Logf("catalog fetch error: %v", ferr)
		}
		for _, e := range entries {
			if e.Key == want {
				raw, _ := json.Marshal(e)
				t.Logf("entry %s: %s", want, raw)
			}
		}
	}

	for _, mode := range []string{modelListBodyKWM, modelListBodyEmpty} {
		body := ""
		if mode == modelListBodyKWM {
			body = qoderEncode([]byte("{}"))
		}
		url := r.modelsURL()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(http.MethodGet, url, reader)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if err := applyCosyHeaders(req, sa, body, url, "", false); err != nil {
			t.Fatalf("cosy: %v", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", cosyUserAgent)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("mode=%-5s transport error: %v", mode, err)
			continue
		}
		payload, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Logf("mode=%-5s HTTP %d: %s", mode, resp.StatusCode, truncateRedacted(string(payload), 200))
			continue
		}
		entries, err := parseCatalog(payload)
		if err != nil {
			t.Logf("mode=%-5s HTTP 200 but parse failed: %v", mode, err)
			continue
		}
		keys := make([]string, 0, len(entries))
		for _, e := range entries {
			keys = append(keys, e.Key)
		}
		t.Logf("mode=%-5s HTTP 200, %d chat models: %s", mode, len(entries), strings.Join(keys, ","))
	}
}
