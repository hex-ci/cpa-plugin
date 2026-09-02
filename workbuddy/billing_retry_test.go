package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestBillingCall_RetriesOn5xx verifies that a transient upstream 500 is
// retried and ultimately succeeds when the next attempt returns 200.
func TestBillingCall_RetriesOn5xx(t *testing.T) {
	orig := billingRetryDelays
	billingRetryDelays = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { billingRetryDelays = orig }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 { // first two attempts → 500
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"k":"v"}}`))
	}))
	defer srv.Close()

	// Temporarily override billingBase so the test server is used.
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	data, err := billingCall(sa, "/test", nil)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if string(data) != `{"k":"v"}` {
		t.Fatalf("unexpected data: %s", string(data))
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (2 retry), got %d", calls)
	}
}

// TestBillingCall_NoRetryOn4xx verifies that business-level errors (4xx,
// non-zero code) are not retried.
func TestBillingCall_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"msg":"bad request"}`))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	_, err := billingCall(sa, "/test", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Should not retry on 4xx — exactly 1 call.
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry on 4xx), got %d", calls)
	}
}

func TestFetchUserResourcePaginatesAllPackages(t *testing.T) {
	var pages []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PageNumber int `json:"PageNumber"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		pages = append(pages, req.PageNumber)
		count := 1
		if req.PageNumber == 1 {
			count = 100
		}
		accounts := make([]resourcePackage, count)
		for i := range accounts {
			accounts[i] = resourcePackage{
				PackageName:         "page-package",
				CycleCapacityRemain: 1,
				CycleCapacitySize:   2,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"Response": map[string]any{
					"Data": map[string]any{
						"TotalCount": 101,
						"Accounts":   accounts,
					},
				},
			},
		})
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	cr, err := fetchUserResource(&storedAuth{})
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	if cr.PackCount != 101 {
		t.Fatalf("pack count=%d, want 101", cr.PackCount)
	}
	if len(pages) != 2 || pages[0] != 1 || pages[1] != 2 {
		t.Fatalf("requested pages=%v, want [1 2]", pages)
	}
}

func TestIsTransientBillingErr(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("http 500 from /v2/billing: internal"), true},
		{errors.New("http 503 from /v2/billing: unavailable"), true},
		{errors.New("code=10000 msg=API request failed"), false}, // business code, not transient
		{errors.New("parse failed: unexpected EOF"), false},
	}
	for _, tt := range tests {
		if got := isTransientBillingErr(tt.err); got != tt.want {
			t.Errorf("isTransientBillingErr(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
