package main

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseModelsDevMetadataAcceptsAdditiveFields(t *testing.T) {
	raw := []byte(`{
		"vendor/serve-alpha":{"id":"serve-alpha","name":"Alpha Canonical","limit":{"context":32768,"output":4096},"modalities":{"input":["text","image"],"output":["text"]},"future":{"accepted":true}},
		"vendor/serve-beta":{"id":"serve-beta","name":"Beta Canonical"}
	}`)
	got, err := parseModelsDevMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	alpha := got["vendor/serve-alpha"]
	if alpha.ContextLength == nil || *alpha.ContextLength != 32768 || alpha.MaxCompletionTokens == nil || *alpha.MaxCompletionTokens != 4096 {
		t.Fatalf("alpha = %#v", alpha)
	}
	if alpha.ID != "vendor/serve-alpha" || alpha.Name != "Alpha Canonical" || !reflect.DeepEqual(alpha.SupportedInputModalities, []string{"text", "image"}) || !reflect.DeepEqual(alpha.SupportedOutputModalities, []string{"text"}) {
		t.Fatalf("alpha = %#v", alpha)
	}
	beta := got["vendor/serve-beta"]
	if beta.ContextLength != nil || beta.MaxCompletionTokens != nil || beta.SupportedInputModalities != nil || beta.SupportedOutputModalities != nil {
		t.Fatalf("beta optionals = %#v, want nil", beta)
	}
}

func TestParseModelsDevMetadataRejectsInvalidSnapshots(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "empty top-level map", raw: []byte(`{}`)},
		{name: "top level is not a map", raw: []byte(`[]`)},
		{name: "canonical key has no provider", raw: []byte(`{"serve-alpha":{"id":"serve-alpha"}}`)},
		{name: "canonical key has empty final segment", raw: []byte(`{"vendor/":{"id":"serve-alpha"}}`)},
		{name: "canonical key has surrounding whitespace", raw: []byte(`{" vendor/serve-alpha ":{"id":"serve-alpha"}}`)},
		{name: "canonical key is too long", raw: []byte(`{"vendor/` + strings.Repeat("x", maxDiscoveredModelIDBytes) + `":{"id":"serve-alpha"}}`)},
		{name: "empty model ID", raw: []byte(`{"vendor/serve-alpha":{"id":" "}}`)},
		{name: "model ID is too long", raw: []byte(`{"vendor/serve-alpha":{"id":"` + strings.Repeat("x", maxDiscoveredModelIDBytes+1) + `"}}`)},
		{name: "negative context limit", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","limit":{"context":-1}}}`)},
		{name: "negative output limit", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","limit":{"output":-1}}}`)},
		{name: "ID has wrong type", raw: []byte(`{"vendor/serve-alpha":{"id":1}}`)},
		{name: "name has wrong type", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","name":[]}}`)},
		{name: "limit has wrong type", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","limit":[]}}`)},
		{name: "context has wrong type", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","limit":{"context":"32768"}}}`)},
		{name: "output has wrong type", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","limit":{"output":false}}}`)},
		{name: "modalities has wrong type", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","modalities":[]}}`)},
		{name: "input modalities have wrong type", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","modalities":{"input":"text"}}}`)},
		{name: "output modalities have wrong type", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","modalities":{"output":{}}}}`)},
		{name: "duplicate input modality", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","modalities":{"input":["text","text"]}}}`)},
		{name: "empty input modality", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","modalities":{"input":[""]}}}`)},
		{name: "blank output modality", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","modalities":{"output":[" "]}}}`)},
		{name: "explicitly empty modality list", raw: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","modalities":{"input":[]}}}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := parseModelsDevMetadata(tt.raw); err == nil {
				t.Fatalf("records = %#v, want error", got)
			}
		})
	}
}

func TestMatchModelsDevRecordUsesExactThenUniqueFinalSegment(t *testing.T) {
	records := map[string]modelFacts{
		"vendor/serve-alpha": {ID: "vendor/serve-alpha", Name: "Alpha"},
		"other/serve-alpha":  {ID: "other/serve-alpha", Name: "Other Alpha"},
		"vendor/serve-beta":  {ID: "vendor/serve-beta", Name: "Beta"},
	}

	tests := []struct {
		name    string
		rawID   string
		records map[string]modelFacts
		wantID  string
	}{
		{name: "full canonical key", rawID: "vendor/serve-alpha", records: records, wantID: "vendor/serve-alpha"},
		{name: "unique final segment", rawID: "serve-beta", records: records, wantID: "vendor/serve-beta"},
		{name: "case mismatch", rawID: "Serve-beta", records: records},
		{name: "ambiguous final segment", rawID: "serve-alpha", records: records},
		{name: "no candidate", rawID: "serve-gamma", records: records},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchModelsDevRecord(tt.rawID, tt.records)
			if tt.wantID == "" {
				if got != nil {
					t.Fatalf("match = %#v, want nil", got)
				}
				return
			}
			if got == nil || got.ID != tt.wantID {
				t.Fatalf("match = %#v, want ID %q", got, tt.wantID)
			}
		})
	}
}

func TestFetchModelsDevMetadataPreservesOpaqueETag(t *testing.T) {
	const opaque = `W/"opaque value,with punctuation"`
	var ifNoneMatch string
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		ifNoneMatch = req.Header.Get("If-None-Match")
		return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: http.Header{"ETag": []string{opaque}}}, nil
	}
	got, err := fetchModelsDevMetadata(opaque, "callback-2", do)
	if err != nil || !got.NotModified || ifNoneMatch != opaque {
		t.Fatalf("result=%#v header=%q err=%v", got, ifNoneMatch, err)
	}
	if got.ETag != opaque || got.Records != nil {
		t.Fatalf("result = %#v, want opaque response ETag and no records", got)
	}
}

func TestFetchModelsDevMetadataBuildsRequestAndValidates200(t *testing.T) {
	const responseETag = `"fresh,opaque"`
	var method, requestURL, callbackID, accept, ifNoneMatch string
	deadlineOK := false
	do := func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
		method = req.Method
		requestURL = req.URL.String()
		callbackID = gotCallbackID
		accept = req.Header.Get("Accept")
		ifNoneMatch = req.Header.Get("If-None-Match")
		if deadline, ok := req.Context().Deadline(); ok {
			remaining := time.Until(deadline)
			deadlineOK = remaining > 14*time.Second && remaining <= modelSourceRequestTimeout
		}
		return &hostHTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"ETag": []string{responseETag}},
			Body:       []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","limit":{"context":32768}}}`),
		}, nil
	}

	got, err := fetchModelsDevMetadata("", "callback-200", do)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || requestURL != modelsDevMetadataURL || callbackID != "callback-200" || accept != "application/json" || ifNoneMatch != "" || !deadlineOK {
		t.Fatalf("request: method=%q url=%q callback=%q accept=%q if-none-match=%q deadline=%v", method, requestURL, callbackID, accept, ifNoneMatch, deadlineOK)
	}
	alpha, ok := got.Records["vendor/serve-alpha"]
	if got.NotModified || got.ETag != responseETag || !ok || alpha.ContextLength == nil || *alpha.ContextLength != 32768 {
		t.Fatalf("result = %#v", got)
	}
}

func TestFetchModelsDevMetadataRejectsInvalid200BeforeReturningRecords(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "empty body"},
		{name: "malformed body", body: []byte(`{"vendor/serve-alpha":`)},
		{name: "invalid snapshot", body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","limit":{"context":-1}}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: tt.body}, nil
			}
			got, err := fetchModelsDevMetadata("", "callback-invalid", do)
			if err == nil || got.Records != nil {
				t.Fatalf("result = %#v, err = %v, want schema error and no records", got, err)
			}
			var sourceErr *modelSourceError
			if !errors.As(err, &sourceErr) || sourceErr.Kind != modelSourceSchemaFailure || err.Error() != "model source schema failure" {
				t.Fatalf("source error = %#v, err = %v", sourceErr, err)
			}
		})
	}
}

func TestFetchModelsDevMetadataClassifiesOtherFailures(t *testing.T) {
	transportErr := errors.New("secret transport detail")
	tests := []struct {
		name       string
		response   *hostHTTPResponse
		doErr      error
		wantKind   modelSourceFailureKind
		wantStatus int
		wantError  string
	}{
		{
			name:      "transport error",
			doErr:     transportErr,
			wantKind:  modelSourceTransportFailure,
			wantError: "model source transport failure",
		},
		{
			name:      "nil response",
			wantKind:  modelSourceTransportFailure,
			wantError: "model source transport failure",
		},
		{
			name:       "unexpected HTTP status",
			response:   &hostHTTPResponse{StatusCode: http.StatusTeapot, Headers: make(http.Header), Body: []byte("secret response body")},
			wantKind:   modelSourceHTTPFailure,
			wantStatus: http.StatusTeapot,
			wantError:  "model source HTTP 418",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				return tt.response, tt.doErr
			}
			got, err := fetchModelsDevMetadata("", "callback-failure", do)
			if err == nil {
				t.Fatalf("result = %#v, want error", got)
			}
			if err.Error() != tt.wantError || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %q, want safe %q", err, tt.wantError)
			}
			var sourceErr *modelSourceError
			if !errors.As(err, &sourceErr) || sourceErr.Kind != tt.wantKind || sourceErr.StatusCode != tt.wantStatus {
				t.Fatalf("source error = %#v", sourceErr)
			}
			if tt.doErr != nil && !errors.Is(err, tt.doErr) {
				t.Fatalf("error does not unwrap transport cause: %v", err)
			}
		})
	}
}

func TestModelInfoFromSourcesUsesServingPrecedenceAndCanonicalFallback(t *testing.T) {
	canonicalContext := int64(32768)
	canonicalOutput := int64(4096)
	serving := modelFacts{
		ID:                       "serve-alpha",
		Name:                     "Serving Alpha",
		SupportedInputModalities: []string{"audio"},
	}
	canonical := modelFacts{
		ID:                        "vendor/serve-alpha",
		Name:                      "Canonical Alpha",
		Description:               "Canonical description",
		ContextLength:             &canonicalContext,
		MaxCompletionTokens:       &canonicalOutput,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	}

	got := modelInfoFromSources(serving, &canonical)
	want := defaultModelInfo("serve-alpha", "Serving Alpha")
	want.Description = "Canonical description"
	want.ContextLength = 32768
	want.MaxCompletionTokens = 4096
	want.SupportedInputModalities = []string{"audio"}
	want.SupportedOutputModalities = []string{"text"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("model info = %#v, want %#v", got, want)
	}

	got.SupportedInputModalities[0] = "changed"
	got.SupportedOutputModalities[0] = "changed"
	if serving.SupportedInputModalities[0] != "audio" || canonical.SupportedOutputModalities[0] != "text" {
		t.Fatalf("returned slices alias inputs: serving=%#v canonical=%#v", serving, canonical)
	}
}

func TestModelInfoFromSourcesKeepsAllNonMissingServingFacts(t *testing.T) {
	servingContext := int64(8192)
	servingOutput := int64(1024)
	canonicalContext := int64(32768)
	canonicalOutput := int64(4096)
	serving := modelFacts{
		ID:                        "serve-alpha",
		Name:                      "Serving Alpha",
		Description:               "Serving description",
		ContextLength:             &servingContext,
		MaxCompletionTokens:       &servingOutput,
		SupportedInputModalities:  []string{"audio"},
		SupportedOutputModalities: []string{"audio"},
	}
	canonical := modelFacts{
		ID:                        "vendor/serve-alpha",
		Name:                      "Canonical Alpha",
		Description:               "Canonical description",
		ContextLength:             &canonicalContext,
		MaxCompletionTokens:       &canonicalOutput,
		SupportedInputModalities:  []string{"text"},
		SupportedOutputModalities: []string{"text"},
	}

	got := modelInfoFromSources(serving, &canonical)
	want := defaultModelInfo("serve-alpha", "Serving Alpha")
	want.Description = "Serving description"
	want.ContextLength = 8192
	want.MaxCompletionTokens = 1024
	want.SupportedInputModalities = []string{"audio"}
	want.SupportedOutputModalities = []string{"audio"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("model info = %#v, want %#v", got, want)
	}
}

func TestModelInfoFromSourcesUsesDefaultTemplateWhenUnmatched(t *testing.T) {
	serving := modelFacts{ID: "serve-unmatched"}
	got := modelInfoFromSources(serving, nil)
	want := defaultModelInfo("serve-unmatched", "")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("model info = %#v, want %#v", got, want)
	}
}
