package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestIsEnterpriseAccount covers the enterprise-member classification used to
// branch billing (get-enterprise-user-usage), check-in, lifecycle and the
// panel badge.
func TestIsEnterpriseAccount(t *testing.T) {
	if isEnterpriseAccount(nil) {
		t.Fatal("nil auth must not be enterprise")
	}
	if isEnterpriseAccount(&storedAuth{}) {
		t.Fatal("empty auth must not be enterprise")
	}
	if isEnterpriseAccount(&storedAuth{Account: storedAccount{EnterpriseID: "  "}}) {
		t.Fatal("whitespace enterpriseId must not be enterprise")
	}
	if !isEnterpriseAccount(&storedAuth{Account: storedAccount{EnterpriseID: "entA1B2C3"}}) {
		t.Fatal("non-empty enterpriseId must be enterprise")
	}
}

// TestFetchEnterpriseResource verifies the enterprise usage-pool endpoint:
// request carries X-Enterprise-Id, response maps limitNum/credit into
// remain/used/size (remain = limitNum − credit, credit is USED).
func TestFetchEnterpriseResource(t *testing.T) {
	var entHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/get-enterprise-user-usage" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		entHeader.Store(r.Header.Get("X-Enterprise-Id"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{
			"credit":125.5,
			"cycleStartTime":"2026-08-01 00:00:00",
			"cycleEndTime":"2026-09-01 23:59:59",
			"limitNum":500,
			"cycleResetTime":"2026-09-02 00:00:00"
		}}`))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{
		Auth:    storedTokens{Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", EnterpriseID: "entA1B2C3"},
	}
	cr, err := fetchEnterpriseResource(sa)
	if err != nil {
		t.Fatalf("fetchEnterpriseResource: %v", err)
	}
	if got := entHeader.Load().(string); got != "entA1B2C3" {
		t.Fatalf("X-Enterprise-Id header = %q, want entA1B2C3", got)
	}
	if cr.TotalRemain != 374 { // 500 − round(125.5)=126
		t.Errorf("TotalRemain = %d, want 374", cr.TotalRemain)
	}
	if cr.TotalUsed != 126 {
		t.Errorf("TotalUsed = %d, want 126", cr.TotalUsed)
	}
	if cr.TotalSize != 500 {
		t.Errorf("TotalSize = %d, want 500", cr.TotalSize)
	}
	if cr.PackCount != 1 || len(cr.Packages) != 1 {
		t.Fatalf("PackCount = %d, len(Packages) = %d, want 1/1", cr.PackCount, len(cr.Packages))
	}
	p := cr.Packages[0]
	if p.Name != "企业版周期额度" {
		t.Errorf("package name = %q, want 企业版周期额度", p.Name)
	}
	if p.CycleStart != "2026-08-01 00:00:00" || p.CycleEnd != "2026-09-01 23:59:59" {
		t.Errorf("cycle window = %s / %s", p.CycleStart, p.CycleEnd)
	}
}

// TestFetchUserResource_RoutesEnterprise verifies that an enterprise account
// hits the enterprise endpoint while a personal account hits the personal
// package endpoint — i.e. fetchUserResource dispatches by enterpriseId.
func TestFetchUserResource_RoutesEnterprise(t *testing.T) {
	var enterpriseHits, personalHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "get-enterprise-user-usage"):
			atomic.AddInt32(&enterpriseHits, 1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":500,"limitNum":1000}}`))
		case strings.HasSuffix(r.URL.Path, "get-user-resource"):
			atomic.AddInt32(&personalHits, 1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":0,"TotalDosage":0,"Accounts":null}},"RequestId":""}}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	ent := &storedAuth{Account: storedAccount{UID: "u1", EnterpriseID: "ent1"}}
	if _, err := fetchUserResource(ent); err != nil {
		t.Fatalf("enterprise fetch: %v", err)
	}
	if enterpriseHits != 1 || personalHits != 0 {
		t.Fatalf("enterprise hits=%d personal hits=%d, want 1/0", enterpriseHits, personalHits)
	}

	per := &storedAuth{Account: storedAccount{UID: "u2"}}
	cr, err := fetchUserResource(per)
	if err != nil {
		t.Fatalf("personal fetch: %v", err)
	}
	if personalHits != 1 || enterpriseHits != 1 {
		t.Fatalf("after personal: hits=%d/%d, want 1/1", enterpriseHits, personalHits)
	}
	if cr == nil || cr.TotalSize != 0 {
		t.Fatalf("personal empty response should yield empty summary, got %+v", cr)
	}
}

// TestFetchPaymentType_Enterprise returns the "enterprise" plan marker without
// an upstream round-trip.
func TestFetchPaymentType_Enterprise(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("enterprise payment type must not hit upstream, got %s", r.URL.Path)
	}))
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{Account: storedAccount{EnterpriseID: "ent1"}}
	if got := fetchPaymentType(sa); got != "enterprise" {
		t.Fatalf("fetchPaymentType(enterprise) = %q, want enterprise", got)
	}
}

// TestDisplayNote_Enterprise keeps the plain CN region tag (the plan badge
// identifies the enterprise account, not the note).
func TestDisplayNote_Enterprise(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{Domain: "www.codebuddy.cn"},
		Account: storedAccount{EnterpriseID: "ent1", UID: "u1"},
	}
	cr := &creditsSummary{TotalRemain: 374, TotalUsed: 126, TotalSize: 500}
	note := displayNote(sa, cr, false)
	if !strings.Contains(note, "CN · ") {
		t.Errorf("note = %q, want CN region marker", note)
	}
	if !strings.Contains(note, "余374") || !strings.Contains(note, "池500") {
		t.Errorf("note = %q, want remain/size values", note)
	}
}

// TestDisplayNameFor_EnterpriseGivenName verifies the real display name is
// decoded from the enterprise JWT given_name claim (personal accounts keep
// their nickname).
func TestDisplayNameFor_EnterpriseGivenName(t *testing.T) {
	// header.payload.signature with given_name + nickname-style family name.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"given_name":"Alice","family_name":"wb_masked_uid","name":"Alice wb_masked_uid"}`))
	jwt := "eyJhbGciOiJSUzI1NiJ9." + payload + ".sig"

	ent := &storedAuth{
		Auth:    storedTokens{AccessToken: jwt},
		Account: storedAccount{EnterpriseID: "ent1", Nickname: "wb_masked_uid"},
	}
	if got := displayNameFor(ent); got != "Alice" {
		t.Fatalf("displayNameFor(enterprise) = %q, want Alice", got)
	}
	if got := labelForAuth(ent); !strings.Contains(got, "Alice") || !strings.Contains(got, " [CN]") {
		t.Fatalf("labelForAuth(enterprise) = %q, want Alice + [CN]", got)
	}
	// Personal account: nickname unchanged, no given_name in JWT.
	personalJWT := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{"nickname":"user_abc"}`)) + ".sig"
	per := &storedAuth{
		Auth:    storedTokens{AccessToken: personalJWT, Domain: "www.codebuddy.cn"},
		Account: storedAccount{Nickname: "user_abc"},
	}
	if got := displayNameFor(per); got != "user_abc" {
		t.Fatalf("displayNameFor(personal) = %q, want user_abc", got)
	}
	// Malformed token / no given_name → nickname fallback.
	broken := &storedAuth{Account: storedAccount{EnterpriseID: "ent1", Nickname: "wb_masked_uid"}}
	if got := displayNameFor(broken); got != "wb_masked_uid" {
		t.Fatalf("displayNameFor(broken jwt) = %q, want nickname fallback", got)
	}
}
