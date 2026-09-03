package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// refreshBody must NOT carry target:"c": that value pins the refreshed
// identity to the personal account and silently rewrites enterprise
// credentials (verified live 2026-09-03: enterprise rt + target:"c" returned
// the personal user's token; omitting target preserved is_biz=true).
func TestRefreshBodyOmitsTargetField(t *testing.T) {
	raw := refreshBody("ory_rt_test")
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not JSON: %s", raw)
	}
	if _, ok := body["target"]; ok {
		t.Fatalf("refresh body must not contain target: %s", raw)
	}
	if len(body) != 1 {
		t.Fatalf("body should contain exactly one field: %s", raw)
	}
	if got := body["refresh_token"]; got != "ory_rt_test" {
		t.Fatalf("refresh_token = %v", got)
	}
	if strings.Contains(string(raw), `"target"`) {
		t.Fatalf("target field present in raw body: %s", raw)
	}
}
