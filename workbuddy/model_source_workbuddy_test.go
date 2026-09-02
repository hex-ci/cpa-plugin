package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseWorkBuddyV3ConfigSelectsCompleteCLIList(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"editor","models":["ignored"]},{"name":"cli","models":["serve-alpha","serve-beta"]}]}}`)
	got, err := parseWorkBuddyV3Config(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "serve-alpha" || got[1].ID != "serve-beta" {
		t.Fatalf("models = %#v", got)
	}
}

func TestParseWorkBuddyLegacyModelsDropsDisabled(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha","name":"Alpha","disabled":false,"contextWindow":4096,"maxTokens":512},{"id":"serve-off","disabled":true}]}}`)
	got, err := parseWorkBuddyLegacyModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "serve-alpha" || got[0].ContextLength == nil || *got[0].ContextLength != 4096 {
		t.Fatalf("models = %#v", got)
	}
}

func TestParseWorkBuddyV3ConfigRejectsInvalidSnapshots(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "missing cli",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"editor","models":["ignored"]}]}}`),
		},
		{
			name: "empty list",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":[]}]}}`),
		},
		{
			name: "duplicate ID",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"," serve-alpha "]}]}}`),
		},
		{
			name: "whitespace-only ID",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["   "]}]}}`),
		},
		{
			name: "ID longer than 512 bytes",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["` + strings.Repeat("x", maxDiscoveredModelIDBytes+1) + `"]}]}}`),
		},
		{
			name: "malformed JSON",
			raw:  []byte(`{"code":`),
		},
		{
			name: "non-zero business code",
			raw:  []byte(`{"code":12,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`),
		},
		{
			name: "wrong field type",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":[123]}]}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := parseWorkBuddyV3Config(tt.raw); err == nil {
				t.Fatalf("models = %#v, want error", got)
			}
		})
	}
}

func TestParseWorkBuddyLegacyModelsRejectsInvalidSnapshots(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "empty list",
			raw:  []byte(`{"code":0,"data":{"models":[]}}`),
		},
		{
			name: "duplicate ID",
			raw:  []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha"},{"id":" serve-alpha "}]}}`),
		},
		{
			name: "whitespace-only ID",
			raw:  []byte(`{"code":0,"data":{"models":[{"id":"   "}]}}`),
		},
		{
			name: "ID longer than 512 bytes",
			raw:  []byte(`{"code":0,"data":{"models":[{"id":"` + strings.Repeat("x", maxDiscoveredModelIDBytes+1) + `"}]}}`),
		},
		{
			name: "negative contextWindow",
			raw:  []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha","contextWindow":-1}]}}`),
		},
		{
			name: "negative maxTokens",
			raw:  []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha","maxTokens":-1}]}}`),
		},
		{
			name: "invalid disabled entry",
			raw:  []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha"},{"id":"serve-off","disabled":true,"maxTokens":-1}]}}`),
		},
		{
			name: "malformed JSON",
			raw:  []byte(`{"code":`),
		},
		{
			name: "non-zero business code",
			raw:  []byte(`{"code":12,"data":{"models":[{"id":"serve-alpha"}]}}`),
		},
		{
			name: "wrong field type",
			raw:  []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha","contextWindow":"4096"}]}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := parseWorkBuddyLegacyModels(tt.raw); err == nil {
				t.Fatalf("models = %#v, want error", got)
			}
		})
	}
}

func TestParseWorkBuddyAcceptsAdditiveUnknownFields(t *testing.T) {
	v3 := []byte(`{"code":0,"future":true,"data":{"agents":[{"name":"cli","models":["serve-alpha"],"future":{"accepted":true}}],"future":true}}`)
	if got, err := parseWorkBuddyV3Config(v3); err != nil || len(got) != 1 || got[0].ID != "serve-alpha" {
		t.Fatalf("v3 models = %#v, err = %v", got, err)
	}

	legacy := []byte(`{"code":0,"future":true,"data":{"models":[{"id":"serve-alpha","future":{"accepted":true}}],"future":true}}`)
	if got, err := parseWorkBuddyLegacyModels(legacy); err != nil || len(got) != 1 || got[0].ID != "serve-alpha" {
		t.Fatalf("legacy models = %#v, err = %v", got, err)
	}
}

func TestValidateWorkBuddyModelFactsNormalizesAndCopies(t *testing.T) {
	inputModalities := []string{"text", "image"}
	outputModalities := []string{"text"}
	got, err := validateModelFacts([]modelFacts{{
		ID:                        " serve-alpha ",
		Name:                      " Alpha ",
		SupportedInputModalities:  inputModalities,
		SupportedOutputModalities: outputModalities,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "serve-alpha" || got[0].Name != "Alpha" {
		t.Fatalf("models = %#v", got)
	}
	inputModalities[0] = "changed"
	outputModalities[0] = "changed"
	if got[0].SupportedInputModalities[0] != "text" || got[0].SupportedOutputModalities[0] != "text" {
		t.Fatalf("model slices alias input: %#v", got[0])
	}
}

func TestWorkBuddyRealmFromAccessToken(t *testing.T) {
	tests := []struct {
		issuer string
		want   workBuddyRealm
	}{
		{issuer: "https://codebuddy.cn/realms/cli", want: workBuddyRealmCN},
		{issuer: "https://www.codebuddy.cn/auth/realms/copilot", want: workBuddyRealmCN},
		{issuer: "https://copilot.tencent.com/realms/cli", want: workBuddyRealmCN},
		{issuer: "https://workbuddy.ai/realms/cli", want: workBuddyRealmGlobal},
	}
	for _, tt := range tests {
		t.Run(tt.issuer, func(t *testing.T) {
			got, err := workBuddyRealmFromAccessToken(syntheticAccessToken(t, tt.issuer))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("realm = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkBuddyRealmFromAccessTokenRejectsMalformedAndUnknownIssuers(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	tests := []struct {
		name  string
		token string
	}{
		{name: "not JWT", token: "broken"},
		{name: "invalid base64", token: header + ".***.signature"},
		{name: "invalid JSON", token: header + "." + base64.RawURLEncoding.EncodeToString([]byte(`not json`)) + ".signature"},
		{name: "missing issuer", token: header + "." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".signature"},
		{name: "issuer is not URL", token: syntheticAccessToken(t, "codebuddy.cn")},
		{name: "unknown issuer", token: syntheticAccessToken(t, "https://unknown.example/realms/cli")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := workBuddyRealmFromAccessToken(tt.token); err == nil {
				t.Fatalf("realm = %q, want error", got)
			}
		})
	}
}

func TestFetchWorkBuddyCatalogFallsBackOnlyOn404Or405(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				calls++
				if calls == 1 {
					return &hostHTTPResponse{StatusCode: status, Headers: make(http.Header)}, nil
				}
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha"}]}}`)}, nil
			}
			got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "callback-1", do)
			if err != nil || calls != 2 || got.Endpoint != workBuddyEndpointLegacyPersonalModels {
				t.Fatalf("catalog=%#v calls=%d err=%v", got, calls, err)
			}
		})
	}
}

func TestFetchWorkBuddyCatalogRoutesByJWTRealm(t *testing.T) {
	tests := []struct {
		realm  workBuddyRealm
		base   string
		origin string
	}{
		{realm: workBuddyRealmCN, base: upstreamBaseCN, origin: originReferer},
		{realm: workBuddyRealmGlobal, base: upstreamBaseGlobal, origin: originRefererGlobal},
	}
	for _, tt := range tests {
		t.Run(string(tt.realm), func(t *testing.T) {
			sa := syntheticStoredAuth(t, tt.realm)
			var method, requestURL, callbackID string
			var headers http.Header
			deadlineOK := false
			do := func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
				method = req.Method
				requestURL = req.URL.String()
				callbackID = gotCallbackID
				headers = req.Header.Clone()
				if deadline, ok := req.Context().Deadline(); ok {
					remaining := time.Until(deadline)
					deadlineOK = remaining > 14*time.Second && remaining <= modelSourceRequestTimeout
				}
				return &hostHTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    make(http.Header),
					Body:       []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`),
				}, nil
			}

			got, err := fetchWorkBuddyCatalog(sa, "callback-route", do)
			if err != nil {
				t.Fatal(err)
			}
			if got.Realm != tt.realm || got.Endpoint != workBuddyEndpointV3Config || len(got.Models) != 1 || got.Models[0].ID != "serve-alpha" {
				t.Fatalf("catalog = %#v", got)
			}
			if method != http.MethodGet || requestURL != tt.base+"/v3/config" {
				t.Fatalf("request = %s %s", method, requestURL)
			}
			if callbackID != "callback-route" {
				t.Fatalf("callback ID = %q", callbackID)
			}
			if !deadlineOK {
				t.Fatal("request deadline is not approximately 15 seconds")
			}
			wantHeaders := map[string]string{
				"Authorization":   "Bearer " + sa.Auth.AccessToken,
				"Accept":          "application/json",
				"Origin":          tt.origin,
				"Referer":         tt.origin + "/",
				"User-Agent":      clientUA,
				"X-User-Id":       "uid-1",
				"X-Enterprise-Id": "enterprise-1",
				"X-Product":       "SaaS",
				"X-IDE-Type":      "CLI",
				"X-IDE-Name":      "CLI",
				"X-IDE-Version":   "2.63.2",
				"X-Agent-Intent":  "craft",
			}
			for key, want := range wantHeaders {
				if value := headers.Get(key); value != want {
					t.Errorf("%s = %q, want %q", key, value, want)
				}
			}
		})
	}
}

func TestFetchWorkBuddyCatalogLegacyRequestPreservesRealmRouting(t *testing.T) {
	tests := []struct {
		realm  workBuddyRealm
		base   string
		origin string
	}{
		{realm: workBuddyRealmCN, base: upstreamBaseCN, origin: originReferer},
		{realm: workBuddyRealmGlobal, base: upstreamBaseGlobal, origin: originRefererGlobal},
	}
	for _, tt := range tests {
		t.Run(string(tt.realm), func(t *testing.T) {
			var urls, callbacks []string
			var legacyHeaders http.Header
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				urls = append(urls, req.URL.String())
				callbacks = append(callbacks, callbackID)
				if len(urls) == 1 {
					return &hostHTTPResponse{StatusCode: http.StatusNotFound, Headers: make(http.Header)}, nil
				}
				legacyHeaders = req.Header.Clone()
				return &hostHTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    make(http.Header),
					Body:       []byte(`{"code":0,"data":{"models":[{"id":"serve-alpha"}]}}`),
				}, nil
			}

			got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, tt.realm), "callback-legacy", do)
			if err != nil {
				t.Fatal(err)
			}
			if got.Endpoint != workBuddyEndpointLegacyPersonalModels || len(urls) != 2 {
				t.Fatalf("catalog = %#v, URLs = %#v", got, urls)
			}
			if urls[0] != tt.base+"/v3/config" || urls[1] != tt.base+"/console/enterprises/personal/models" {
				t.Fatalf("URLs = %#v", urls)
			}
			if callbacks[0] != "callback-legacy" || callbacks[1] != "callback-legacy" {
				t.Fatalf("callback IDs = %#v", callbacks)
			}
			if legacyHeaders.Get("Accept") != "application/json" || legacyHeaders.Get("Origin") != tt.origin || legacyHeaders.Get("Referer") != tt.origin+"/" {
				t.Fatalf("legacy headers = %#v", legacyHeaders)
			}
		})
	}
}

func TestFetchWorkBuddyCatalogDoesNotFallbackOnOtherFailures(t *testing.T) {
	transportErr := errors.New("secret host transport detail")
	tests := []struct {
		name       string
		response   *hostHTTPResponse
		doErr      error
		wantKind   modelSourceFailureKind
		wantStatus int
		wantError  string
	}{
		{
			name:       "401",
			response:   &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header)},
			wantKind:   modelSourceHTTPFailure,
			wantStatus: http.StatusUnauthorized,
			wantError:  "model source HTTP 401",
		},
		{
			name:       "403",
			response:   &hostHTTPResponse{StatusCode: http.StatusForbidden, Headers: make(http.Header)},
			wantKind:   modelSourceHTTPFailure,
			wantStatus: http.StatusForbidden,
			wantError:  "model source HTTP 403",
		},
		{
			name:       "500",
			response:   &hostHTTPResponse{StatusCode: http.StatusInternalServerError, Headers: make(http.Header)},
			wantKind:   modelSourceHTTPFailure,
			wantStatus: http.StatusInternalServerError,
			wantError:  "model source HTTP 500",
		},
		{
			name:      "transport error",
			doErr:     transportErr,
			wantKind:  modelSourceTransportFailure,
			wantError: "model source transport failure",
		},
		{
			name:      "non-zero business code",
			response:  &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":9,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)},
			wantKind:  modelSourceSchemaFailure,
			wantError: "model source schema failure",
		},
		{
			name:      "empty body",
			response:  &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header)},
			wantKind:  modelSourceSchemaFailure,
			wantError: "model source schema failure",
		},
		{
			name:      "malformed body",
			response:  &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":`)},
			wantKind:  modelSourceSchemaFailure,
			wantError: "model source schema failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				calls++
				return tt.response, tt.doErr
			}
			got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "callback-1", do)
			if err == nil {
				t.Fatalf("catalog = %#v, want error", got)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1", calls)
			}
			if err.Error() != tt.wantError {
				t.Fatalf("error = %q, want %q", err, tt.wantError)
			}
			var sourceErr *modelSourceError
			if !errors.As(err, &sourceErr) || sourceErr.Kind != tt.wantKind || sourceErr.StatusCode != tt.wantStatus {
				t.Fatalf("source error = %#v", sourceErr)
			}
			if tt.doErr != nil && !errors.Is(err, tt.doErr) {
				t.Fatalf("error does not unwrap transport cause: %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked transport detail: %v", err)
			}
		})
	}
}

func TestFetchWorkBuddyCatalogRejectsInvalidRealmBeforeRequest(t *testing.T) {
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	sa.Auth.AccessToken = "malformed"
	calls := 0
	_, err := fetchWorkBuddyCatalog(sa, "callback-1", func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		calls++
		return nil, nil
	})
	if err == nil || err.Error() != "model source schema failure" || calls != 0 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func syntheticAccessToken(t *testing.T, issuer string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]string{"iss": issuer})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func syntheticStoredAuth(t *testing.T, realm workBuddyRealm) *storedAuth {
	t.Helper()
	issuer := "https://codebuddy.cn/realms/cli"
	domain := "codebuddy.cn"
	if realm == workBuddyRealmGlobal {
		issuer = "https://workbuddy.ai/realms/cli"
		domain = "workbuddy.ai"
	}
	return &storedAuth{
		Auth:    storedTokens{AccessToken: syntheticAccessToken(t, issuer), Domain: domain},
		Account: storedAccount{UID: "uid-1", EnterpriseID: "enterprise-1"},
	}
}
