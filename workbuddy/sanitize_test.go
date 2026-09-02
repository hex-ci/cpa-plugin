package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeBlockedTemplates_ClaudeCode(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude."
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace blocked template")
	}
	want := "You are Claude Code, Anthropic's official CLI tool for Claude."
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestSanitizeBlockedTemplates_MainBranch(t *testing.T) {
	in := "Main branch (you will usually use this for PRs)"
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace Main branch")
	}
}

func TestSanitizeBlockedTemplates_NoMatch(t *testing.T) {
	in := "Hello world"
	out := sanitizeBlockedTemplates(in)
	if out != in {
		t.Fatal("should pass through unchanged")
	}
}

func TestSanitizeBlockedTemplates_Fingerprints(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "partial template text remains unchanged",
			in:   "    You are Claude Code, but this is not the blocked template",
			want: "    You are Claude Code, but this is not the blocked template",
		},
		{
			name: "ordinary cc assignment",
			in:   "Set cc_library=foo and keep this sentence",
			want: "Set cc_library=foo and keep this sentence",
		},
		{
			name: "billing header value irrelevant",
			in:   "prefix x-anthropic-billing-header: arbitrary=value; suffix",
			want: "prefix suffix",
		},
		{
			name: "billing header case insensitive",
			in:   "X-Anthropic-Billing-Header: cc_version=1.0; useful",
			want: "useful",
		},
		{
			name: "cc fingerprint case insensitive",
			in:   "semantic; CC_VERSION=2.0; CC_ENTRYPOINT=cli; end",
			want: "semantic; end",
		},
		{
			name: "multiple trailing cc keys",
			in:   "semantic; cc_version=2.0; cc_entrypoint=cli; end",
			want: "semantic; end",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeBlockedTemplates(tt.in); got != tt.want {
				t.Fatalf("sanitizeBlockedTemplates(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPrepareUpstreamBodySanitizesFingerprintsAndPreservesReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"other-model","reasoning_effort":"low","messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli; keep me"}]}`)
	out := prepareUpstreamBody(body, nil, nil, "other-model")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want low", obj["reasoning_effort"])
	}
	messages := obj["messages"].([]any)
	content := messages[0].(map[string]any)["content"].(string)
	if strings.Contains(strings.ToLower(content), "x-anthropic-billing-header") || strings.Contains(strings.ToLower(content), "cc_") {
		t.Fatalf("fingerprints remain: %q", content)
	}
	if !strings.Contains(content, "keep me") {
		t.Fatalf("semantic text lost: %q", content)
	}
}

func TestPrepareUpstreamBodyPreservesCallerReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","reasoning_effort":"medium","messages":[]}`)
	out := prepareUpstreamBody(body, nil, nil, "serve-beta")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "serve-beta" || obj["reasoning_effort"] != "medium" {
		t.Fatalf("rewritten payload = %#v", obj)
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Fatal("short string should be unchanged")
	}
	if truncate("hello world", 5) != "hello" {
		t.Fatal("should truncate to 5 chars")
	}
	if truncate("", 5) != "" {
		t.Fatal("empty string")
	}
}
