package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// windowEntry builds a catalog entry shaped like the live Qoder one: three
// options with 200K marked default.
func windowEntry(maxInput int64) catalogEntry {
	return catalogEntry{
		Key:            "qmodel_38max",
		Enable:         true,
		IsReasoning:    true,
		MaxInputTokens: maxInput,
		ContextConfig: map[string]struct {
			TokenCount int64 `json:"token_count"`
			IsDefault  bool  `json:"is_default"`
		}{
			"200K": {TokenCount: 200000, IsDefault: true},
			"400K": {TokenCount: 400000},
			"1M":   {TokenCount: 1000000},
		},
	}
}

// default keeps upstream's is_default option, i.e. the historical behaviour.
func TestContextWindowDefault(t *testing.T) {
	if got := windowEntry(180000).contextWindowFor(contextWindowDefault); got != 200000 {
		t.Fatalf("default = %d, want 200000 (catalog is_default)", got)
	}
}

// max takes each model's largest option — 1M here, but 200K for models that
// only offer one window, which is why the value must be per model.
func TestContextWindowMaxIsPerModel(t *testing.T) {
	if got := windowEntry(180000).contextWindowFor(contextWindowMax); got != 1000000 {
		t.Fatalf("max = %d, want 1000000", got)
	}
	single := catalogEntry{
		Key:            "auto",
		MaxInputTokens: 200000,
		ContextConfig: map[string]struct {
			TokenCount int64 `json:"token_count"`
			IsDefault  bool  `json:"is_default"`
		}{
			"200K": {TokenCount: 200000, IsDefault: true},
		},
	}
	if got := single.contextWindowFor(contextWindowMax); got != 200000 {
		t.Fatalf("single-option model max = %d, want 200000", got)
	}
}

// An explicit token count is honoured but never exceeds what the catalog
// offers: advertising more than upstream publishes only invites rejections.
func TestContextWindowNumericIsClamped(t *testing.T) {
	if got := windowEntry(180000).contextWindowFor("400000"); got != 400000 {
		t.Fatalf("explicit 400000 = %d", got)
	}
	if got := windowEntry(180000).contextWindowFor("2000000"); got != 1000000 {
		t.Fatalf("over-large explicit value = %d, want clamp to 1000000", got)
	}
	if got := windowEntry(180000).contextWindowFor("not-a-number"); got != 200000 {
		t.Fatalf("garbage mode = %d, want default fallback", got)
	}
}

// InputTokenLimit must stay the catalog's hard input cap regardless of the
// window setting: clients use it to decide whether a request is safe.
func TestInputTokenLimitIgnoresContextWindowSetting(t *testing.T) {
	e := windowEntry(180000)
	cfgMu.Lock()
	prev := cfg.ContextWindow
	cfg.ContextWindow = contextWindowMax
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg.ContextWindow = prev
		cfgMu.Unlock()
	})

	info := e.modelInfo()
	if info.ContextLength != 1000000 {
		t.Fatalf("ContextLength = %d, want the configured max", info.ContextLength)
	}
	if info.InputTokenLimit != 180000 {
		t.Fatalf("InputTokenLimit = %d, want the catalog's max_input_tokens", info.InputTokenLimit)
	}
}

// The config value is validated at configure time so a typo fails loudly
// instead of silently keeping the default.
func TestContextWindowConfigValidation(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: contextWindowDefault},
		{in: "default", want: contextWindowDefault},
		{in: "MAX", want: contextWindowMax},
		{in: "400000", want: "400000"},
		{in: "0", wantErr: true},
		{in: "-5", wantErr: true},
		{in: "huge", wantErr: true},
	}
	for _, tc := range cases {
		c := defaultPluginConfig()
		c.ContextWindow = tc.in
		err := c.validate()
		if tc.wantErr {
			if err == nil {
				t.Fatalf("context_window %q accepted, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("context_window %q rejected: %v", tc.in, err)
		}
		if c.ContextWindow != tc.want {
			t.Fatalf("context_window %q normalised to %q, want %q", tc.in, c.ContextWindow, tc.want)
		}
	}
}

// A queue timeout that reaches the client must carry its 429 classification all
// the way through. queueWaitGate used to return a bare Go error, so the host
// fell back to 500/200-with-error-body and never failed over to another
// credential.
func TestQueueWaitGateReturnsClassifiedFailure(t *testing.T) {
	cfgMu.Lock()
	prev := cfg.QueueWait
	cfg.QueueWait = false // take the no-wait branch: deterministic, no sleeping
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg.QueueWait = prev
		cfgMu.Unlock()
	})

	err := queueWaitGate(&storedAuth{}, "qmodel_38max", queueSignal{RetryAfter: 30, Detail: "RESOURCE_LIMIT"}, 0)
	if err == nil {
		t.Fatal("queueWaitGate returned nil for a queue signal with waiting disabled")
	}
	raw, ok := failureEnvelope(err)
	if !ok {
		t.Fatalf("queueWaitGate error is not classified: %T %v", err, err)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		t.Fatalf("decode envelope: %v; raw=%s", uerr, raw)
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429; raw=%s", env.Error.HTTPStatus, raw)
	}
	if env.Error.Code != "transient_rate_limit" {
		t.Fatalf("code = %q, want transient_rate_limit", env.Error.Code)
	}
}
