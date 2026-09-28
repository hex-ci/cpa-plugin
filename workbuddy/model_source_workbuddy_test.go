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
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha","serve-off"]}],"models":[{"id":"serve-alpha","name":"Alpha","disabled":false,"maxInputTokens":4096,"maxOutputTokens":512},{"id":"serve-off","disabled":true}]}}`)
	got, err := parseWorkBuddyLegacyModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "serve-alpha" || got[0].ContextLength == nil || *got[0].ContextLength != 4096 {
		t.Fatalf("models = %#v", got)
	}
	if got[0].MaxCompletionTokens == nil || *got[0].MaxCompletionTokens != 512 {
		t.Fatalf("max completion tokens = %#v, want 512", got[0].MaxCompletionTokens)
	}
}

// The upstream catalog uses maxOutputTokens / maxInputTokens. An earlier
// revision decoded maxTokens / contextWindow, which never matched, so every
// limit stayed nil and the plugin fell back to models.dev values that were up
// to 5x larger than the models' real allowance. This test pins the real keys.
func TestParseWorkBuddyLegacyModelsUsesUpstreamLimitKeys(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}],"models":[{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash",` +
		`"maxInputTokens":1000000,"maxOutputTokens":128000,"maxAllowedSize":1000000,` +
		`"onlyReasoning":true,"supportsReasoning":true,"reasoning":{"effort":"high","summary":"auto"}}]}}`)
	got, err := parseWorkBuddyLegacyModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("models = %#v", got)
	}
	m := got[0]
	if m.ID != "deepseek-v4.1-flash" || m.Name != "Deepseek-V4.1-Flash" {
		t.Fatalf("id/name = %q/%q", m.ID, m.Name)
	}
	if m.ContextLength == nil || *m.ContextLength != 1000000 {
		t.Fatalf("context length = %#v, want 1000000", m.ContextLength)
	}
	if m.MaxCompletionTokens == nil || *m.MaxCompletionTokens != 128000 {
		t.Fatalf("max completion tokens = %#v, want 128000", m.MaxCompletionTokens)
	}
}

// The modality a client sees must come from the WorkBuddy catalogue itself.
// It used to be filled from models.dev, which made a third-party site a
// requirement for describing models the upstream already describes.
//
// Conservative rule: only an explicit supportsImages=true declares image
// input. A missing key stays undeclared, because asserting a modality the
// model does not accept would invite payloads the upstream rejects.
func TestParseWorkBuddyCatalogModalitiesFromUpstream(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["vision-model","text-model","silent-model","image-generator"]}],"models":[` +
		`{"id":"vision-model","supportsImages":true,"supportsToolCall":true},` +
		`{"id":"text-model","supportsImages":false},` +
		`{"id":"silent-model"},` +
		`{"id":"image-generator"}]}}`)
	got, err := parseWorkBuddyLegacyModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]modelFacts, len(got))
	for _, m := range got {
		byID[m.ID] = m
	}

	if mods := byID["vision-model"].SupportedInputModalities; len(mods) != 2 || mods[0] != modalityText || mods[1] != modalityImage {
		t.Fatalf("vision model modalities = %#v, want [text image]", mods)
	}
	// An explicit false means the upstream told us text-only; it must not be
	// upgraded to text+image, and it must not invent a modality list either.
	if mods := byID["text-model"].SupportedInputModalities; mods != nil {
		t.Fatalf("explicit-false model modalities = %#v, want nil (undeclared)", mods)
	}
	// Absent key: nothing claimed.
	if mods := byID["silent-model"].SupportedInputModalities; mods != nil {
		t.Fatalf("silent model modalities = %#v, want nil", mods)
	}
	if mods := byID["image-generator"].SupportedInputModalities; mods != nil {
		t.Fatalf("image generator modalities = %#v, want nil", mods)
	}

	// Output modalities are never asserted: every catalogue model produces
	// text and the upstream says nothing further.
	for _, m := range got {
		if m.SupportedOutputModalities != nil {
			t.Fatalf("%s output modalities = %#v, want nil", m.ID, m.SupportedOutputModalities)
		}
	}
}

// The /v3/config path joins an ordered roster with a detail map; the modality
// must survive that join, which is the path the plugin actually uses.
func TestParseWorkBuddyV3ConfigKeepsModalityThroughRosterJoin(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["vision-model","plain-model"]}],` +
		`"models":[` +
		`{"id":"vision-model","name":"Vision","supportsImages":true,"maxInputTokens":1000,"maxOutputTokens":32000},` +
		`{"id":"plain-model","name":"Plain","supportsImages":false,"maxInputTokens":2000,"maxOutputTokens":64000}` +
		`]}}`)
	got, err := parseWorkBuddyV3Config(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("models = %#v", got)
	}
	if got[0].ID != "vision-model" || len(got[0].SupportedInputModalities) != 2 {
		t.Fatalf("vision entry = %#v", got[0])
	}
	if got[1].ID != "plain-model" || got[1].SupportedInputModalities != nil {
		t.Fatalf("plain entry = %#v", got[1])
	}
}

// A description may arrive as description / descriptionEn / descriptionZh
// depending on catalog shape; the parser must not depend on the flat field.
func TestParseWorkBuddyModelEntryDescriptionFallbacks(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["flat","en","zh","none"]}],"models":[` +
		`{"id":"flat","description":"flat-desc"},` +
		`{"id":"en","descriptionEn":"en-desc"},` +
		`{"id":"zh","descriptionZh":"zh-desc"},` +
		`{"id":"none"}]}}`)
	got, err := parseWorkBuddyLegacyModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]modelFacts{}
	for _, m := range got {
		byID[m.ID] = m
	}
	for id, want := range map[string]string{
		"flat": "flat-desc", "en": "en-desc", "zh": "zh-desc", "none": "",
	} {
		if byID[id].Description != want {
			t.Errorf("%s description = %q, want %q", id, byID[id].Description, want)
		}
	}
}

// /v3/config's data.models[] carries the limits; the cli agent's models[] is the
// ordered roster. They must be joined by id so limits stop being dropped.
func TestParseWorkBuddyV3ConfigJoinsRosterWithLimits(t *testing.T) {
	raw := []byte(`{"code":0,"data":{` +
		`"agents":[{"name":"cli","models":["serve-alpha","serve-beta","serve-gamma"]}],` +
		`"models":[` +
		`{"id":"serve-alpha","name":"Alpha","maxInputTokens":200000,"maxOutputTokens":24000},` +
		`{"id":"serve-beta","name":"Beta","maxInputTokens":1000000,"maxOutputTokens":128000}` +
		`]}}`)
	got, err := parseWorkBuddyV3Config(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("models = %#v", got)
	}
	// order follows the roster, not the detail list
	if got[0].ID != "serve-alpha" || got[1].ID != "serve-beta" || got[2].ID != "serve-gamma" {
		t.Fatalf("roster order lost: %#v", got)
	}
	if got[0].MaxCompletionTokens == nil || *got[0].MaxCompletionTokens != 24000 {
		t.Fatalf("alpha max completion = %#v, want 24000", got[0].MaxCompletionTokens)
	}
	if got[0].ContextLength == nil || *got[0].ContextLength != 200000 {
		t.Fatalf("alpha context = %#v, want 200000", got[0].ContextLength)
	}
	if got[1].MaxCompletionTokens == nil || *got[1].MaxCompletionTokens != 128000 {
		t.Fatalf("beta max completion = %#v, want 128000", got[1].MaxCompletionTokens)
	}
	// roster id with no detail row survives with nil limits
	if got[2].MaxCompletionTokens != nil || got[2].ContextLength != nil {
		t.Fatalf("gamma limits should be nil: %#v", got[2])
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
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"," serve-alpha "]}],"models":[{"id":"serve-alpha"},{"id":" serve-alpha "}]}}`),
		},
		{
			name: "whitespace-only ID",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["   "]}],"models":[{"id":"   "}]}}`),
		},
		{
			name: "ID longer than 512 bytes",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["` + strings.Repeat("x", maxDiscoveredModelIDBytes+1) + `"]}],"models":[{"id":"` + strings.Repeat("x", maxDiscoveredModelIDBytes+1) + `"}]}}`),
		},
		{
			name: "negative maxInputTokens",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],"models":[{"id":"serve-alpha","maxInputTokens":-1}]}}`),
		},
		{
			name: "negative maxOutputTokens",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],"models":[{"id":"serve-alpha","maxOutputTokens":-1}]}}`),
		},
		{
			name: "malformed JSON",
			raw:  []byte(`{"code":`),
		},
		{
			name: "non-zero business code",
			raw:  []byte(`{"code":12,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],"models":[{"id":"serve-alpha"}]}}`),
		},
		{
			name: "wrong field type",
			raw:  []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],"models":[{"id":"serve-alpha","maxInputTokens":"4096"}]}}`),
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

// Filtered entries are dropped BEFORE validation, so a malformed limit on a
// row that never reaches the catalogue cannot fail the whole refresh. Serving a
// good catalogue is strictly better than failing because an entry the client
// will never see was malformed.
func TestParseWorkBuddyLegacyModelsFiltersBeforeValidating(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha","gone","image-model"]}],` +
		`"models":[` +
		`{"id":"serve-alpha","maxInputTokens":4096},` +
		`{"id":"gone","disabled":true,"maxInputTokens":-1},` +
		`{"id":"image-model","maxInputTokens":-1,"tags":["text-to-image"]}` +
		`]}}`)
	got, err := parseWorkBuddyLegacyModels(raw)
	if err != nil {
		t.Fatalf("filtered rows must not fail the parse: %v", err)
	}
	if len(got) != 1 || got[0].ID != "serve-alpha" {
		t.Fatalf("models = %#v", got)
	}
}

// A roster that is absent or empty leaves no usable authority for which models
// the account may call, which is a schema failure rather than an empty success.
func TestParseWorkBuddyLegacyModelsRequiresRoster(t *testing.T) {
	for name, raw := range map[string]string{
		"no agents":    `{"code":0,"data":{"models":[{"id":"serve-alpha"}]}}`,
		"empty roster": `{"code":0,"data":{"agents":[{"name":"cli","models":[]}],"models":[{"id":"serve-alpha"}]}}`,
		"other agent":  `{"code":0,"data":{"agents":[{"name":"general-purpose","models":["serve-alpha"]}],"models":[{"id":"serve-alpha"}]}}`,
	} {
		if _, err := parseWorkBuddyLegacyModels([]byte(raw)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestParseWorkBuddyAcceptsAdditiveUnknownFields(t *testing.T) {
	v3 := []byte(`{"code":0,"future":true,"data":{"agents":[{"name":"cli","models":["serve-alpha"],"future":{"accepted":true}}],"future":true}}`)
	if got, err := parseWorkBuddyV3Config(v3); err != nil || len(got) != 1 || got[0].ID != "serve-alpha" {
		t.Fatalf("v3 models = %#v, err = %v", got, err)
	}

	legacy := []byte(`{"code":0,"future":true,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],"models":[{"id":"serve-alpha","future":{"accepted":true}}],"future":true}}`)
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
		// The CN desktop issuer (WorkBuddy for the CN region) also uses a www. host.
		{issuer: "https://workbuddy.cn/realms/cli", want: workBuddyRealmCN},
		{issuer: "https://www.workbuddy.cn/auth/realms/copilot", want: workBuddyRealmCN},
		{issuer: "https://workbuddy.ai/realms/cli", want: workBuddyRealmGlobal},
		// The international desktop issuer carries the www. host; matching the
		// bare workbuddy.ai only left every Global account unauthorized.
		{issuer: "https://www.workbuddy.ai/auth/realms/copilot", want: workBuddyRealmGlobal},
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
	// A v3 shape that answers 404/405 is one unavailable source, not a failed
	// refresh: the remaining sources still produce a catalogue.
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(strconv.FormatInt(int64(status), 10), func(t *testing.T) {
			t.Run("another v3 shape serves it", func(t *testing.T) {
				calls := 0
				do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
					calls++
					if calls == 1 {
						return &hostHTTPResponse{StatusCode: status, Headers: make(http.Header)}, nil
					}
					return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
				}
				got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmGlobal), "callback-1", do)
				// The desktop shape 404s, the CLI shape answers, and the
				// Global realm has no legacy endpoint to try.
				if err != nil || calls != 2 || got.Endpoint != workBuddyEndpointV3Config {
					t.Fatalf("catalog=%#v calls=%d err=%v", got, calls, err)
				}
			})

			t.Run("legacy serves it when every v3 shape fails", func(t *testing.T) {
				calls := 0
				do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
					calls++
					if req.URL.Path == "/console/enterprises/personal/models" {
						return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],"models":[{"id":"serve-alpha"}]}}`)}, nil
					}
					return &hostHTTPResponse{StatusCode: status, Headers: make(http.Header)}, nil
				}
				got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "callback-1", do)
				if err != nil || calls != 3 || got.Endpoint != workBuddyEndpointLegacyPersonalModels {
					t.Fatalf("catalog=%#v calls=%d err=%v", got, calls, err)
				}
			})
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
			var method, callbackID string
			var uas []string
			var requestURLs []string
			var headers http.Header
			deadlineOK := false
			do := func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
				// The legacy leg answers 401 (as production does for an
				// account without entitlement), so the v3 legs alone decide the
				// catalogue and their routing facts are what get asserted.
				if req.URL.Path == "/console/enterprises/personal/models" {
					return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
				}
				method = req.Method
				requestURLs = append(requestURLs, req.URL.String())
				uas = append(uas, req.Header.Get("User-Agent"))
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
			if got.Realm != tt.realm || got.Endpoint != workBuddyEndpointV3ConfigShapes || len(got.Models) != 1 || got.Models[0].ID != "serve-alpha" {
				t.Fatalf("catalog = %#v", got)
			}
			if method != http.MethodGet || len(requestURLs) != 2 || requestURLs[0] != tt.base+"/v3/config" || requestURLs[1] != tt.base+"/v3/config" {
				t.Fatalf("requests = %s %v, want two GETs to %s/v3/config", method, requestURLs, tt.base)
			}
			// The catalogue is UA-scoped, so the two legs must differ in
			// exactly the User-Agent: desktop shape first, CLI shape second.
			if len(uas) != 2 || uas[0] != catalogUA || uas[1] != clientUA {
				t.Fatalf("user agents = %#v, want [%q %q]", uas, catalogUA, clientUA)
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
				"X-Tenant-Id":     "enterprise-1",
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
	const legacyBody = `{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],"models":[{"id":"serve-alpha"}]}}`

	t.Run(string(workBuddyRealmCN), func(t *testing.T) {
		var urls, callbacks []string
		var legacyHeaders http.Header
		do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
			urls = append(urls, req.URL.String())
			callbacks = append(callbacks, callbackID)
			if req.URL.Path != "/console/enterprises/personal/models" {
				return &hostHTTPResponse{StatusCode: http.StatusNotFound, Headers: make(http.Header)}, nil
			}
			legacyHeaders = req.Header.Clone()
			return &hostHTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    make(http.Header),
				Body:       []byte(legacyBody),
			}, nil
		}

		got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "callback-legacy", do)
		if err != nil {
			t.Fatal(err)
		}
		if got.Endpoint != workBuddyEndpointLegacyPersonalModels || len(urls) != 3 {
			t.Fatalf("catalog = %#v, URLs = %#v", got, urls)
		}
		if urls[0] != upstreamBaseCN+"/v3/config" || urls[1] != upstreamBaseCN+"/v3/config" || urls[2] != upstreamBaseCN+"/console/enterprises/personal/models" {
			t.Fatalf("URLs = %#v", urls)
		}
		for i, id := range callbacks {
			if id != "callback-legacy" {
				t.Fatalf("callback IDs = %#v (entry %d)", callbacks, i)
			}
		}
		if legacyHeaders.Get("Accept") != "application/json" || legacyHeaders.Get("Origin") != originReferer || legacyHeaders.Get("Referer") != originReferer+"/" {
			t.Fatalf("legacy headers = %#v", legacyHeaders)
		}
		if legacyHeaders.Get("User-Agent") != clientUA {
			t.Fatalf("legacy request UA = %q, want the CLI identity", legacyHeaders.Get("User-Agent"))
		}
	})

	// The legacy route answers 500 on the Global deployment (measured
	// 2026-09-28), so the Global realm must not spend a request on it.
	t.Run(string(workBuddyRealmGlobal), func(t *testing.T) {
		var urls []string
		do := func(req *http.Request, _ string) (*hostHTTPResponse, error) {
			urls = append(urls, req.URL.String())
			return &hostHTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    make(http.Header),
				Body:       []byte(legacyBody),
			}, nil
		}

		got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmGlobal), "callback-legacy", do)
		if err != nil {
			t.Fatal(err)
		}
		if got.Endpoint != workBuddyEndpointV3ConfigShapes {
			t.Fatalf("endpoint = %q, want the merged v3 shapes", got.Endpoint)
		}
		if len(urls) != 2 || urls[0] != upstreamBaseGlobal+"/v3/config" || urls[1] != upstreamBaseGlobal+"/v3/config" {
			t.Fatalf("URLs = %#v, want the v3 config requests only", urls)
		}
	})
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
			// Every source is requested, so a failing catalogue makes three
			// attempts on CN (both v3 client shapes plus the legacy endpoint);
			// the reported error is the first failing source's.
			if calls != 3 {
				t.Fatalf("calls = %d, want 3", calls)
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
