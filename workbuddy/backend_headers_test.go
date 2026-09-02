package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestBackendHeadersAddsClientIdentity(t *testing.T) {
	makeHeaders := func() http.Header {
		req := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
		backendHeaders(req, &storedAuth{})
		return req.Header
	}

	first := makeHeaders()
	for name, want := range map[string]string{
		"X-IDE-Type":     "CLI",
		"X-IDE-Name":     "CLI",
		"X-IDE-Version":  "2.63.2",
		"X-Agent-Intent": "craft",
	} {
		if got := first.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	idPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	idHeaders := []string{
		"X-Request-ID",
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
	}
	seen := make(map[string]string, len(idHeaders))
	for _, name := range idHeaders {
		value := first.Get(name)
		if !idPattern.MatchString(value) {
			t.Errorf("%s = %q, want 32 lowercase hex characters", name, value)
		}
		if previous, exists := seen[value]; exists {
			t.Errorf("%s duplicates %s with value %q", name, previous, value)
		}
		seen[value] = name
	}

	second := makeHeaders()
	for _, name := range idHeaders {
		if first.Get(name) == second.Get(name) {
			t.Errorf("%s was reused across requests: %q", name, first.Get(name))
		}
	}
}
