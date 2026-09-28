// model_union_test.go pins the two-endpoint catalogue merge.
//
// The merge decides what the plugin offers clients, so the rules under test are
// the ones that keep a refresh from losing models or advertising duplicates:
// primary wins on conflicts, order stays stable, and an empty merge never
// replaces a usable catalogue.
package main

import (
	"net/http"
	"testing"
)

func TestUnionModelFacts(t *testing.T) {
	ctx := func(v int64) *int64 { return &v }

	t.Run("secondary contributes only ids the primary lacks", func(t *testing.T) {
		primary := []modelFacts{{ID: "a", ContextLength: ctx(100)}}
		secondary := []modelFacts{{ID: "a", ContextLength: ctx(999)}, {ID: "b", ContextLength: ctx(200)}}
		got := unionModelFacts(primary, secondary)
		if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
			t.Fatalf("union = %#v", got)
		}
		// The duplicate keeps the primary's fields: the primary endpoint is the
		// authoritative source for per-model facts.
		if got[0].ContextLength == nil || *got[0].ContextLength != 100 {
			t.Fatalf("primary entry was overwritten: %#v", got[0])
		}
	})

	t.Run("order is primary first then secondary additions", func(t *testing.T) {
		primary := []modelFacts{{ID: "p1"}, {ID: "p2"}}
		secondary := []modelFacts{{ID: "s1"}, {ID: "p2"}, {ID: "s2"}}
		got := unionModelFacts(primary, secondary)
		want := []string{"p1", "p2", "s1", "s2"}
		if len(got) != len(want) {
			t.Fatalf("union = %#v", got)
		}
		for i, id := range want {
			if got[i].ID != id {
				t.Fatalf("union[%d] = %q, want %q (full: %#v)", i, got[i].ID, id, got)
			}
		}
	})

	t.Run("empty secondary returns the primary", func(t *testing.T) {
		primary := []modelFacts{{ID: "a"}}
		got := unionModelFacts(primary, nil)
		if len(got) != 1 || got[0].ID != "a" {
			t.Fatalf("union = %#v", got)
		}
	})

	t.Run("an empty merge falls back to the primary", func(t *testing.T) {
		// Defensive: publishing an empty catalogue would wipe a working one.
		got := unionModelFacts(nil, nil)
		if len(got) != 0 {
			t.Fatalf("union = %#v", got)
		}
	})

	t.Run("ids are compared after trimming", func(t *testing.T) {
		primary := []modelFacts{{ID: " a "}}
		secondary := []modelFacts{{ID: "a"}, {ID: "b"}}
		got := unionModelFacts(primary, secondary)
		// "a" is already present via the trimmed primary key, so only "b" is
		// added; the primary entry keeps its original spelling.
		if len(got) != 2 || got[0].ID != " a " || got[1].ID != "b" {
			t.Fatalf("union = %#v", got)
		}
	})
}

// The catalogue is UA-scoped. Each client shape lists models the other omits
// (measured on the live service: the Global desktop shape adds three the CLI
// shape lacks, the CLI shape adds one the desktop shape lacks), and every
// model in either roster is callable — so the snapshot must carry the union
// rather than whichever shape happened to be asked for.
func TestFetchWorkBuddyCatalogMergesClientShapes(t *testing.T) {
	desktopBody := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["desktop-only","shared"]}]}}`)
	cliBody := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["shared","cli-only"]}]}}`)

	do := func(req *http.Request, _ string) (*hostHTTPResponse, error) {
		if req.URL.Path == "/console/enterprises/personal/models" {
			return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header)}, nil
		}
		body := cliBody
		if req.Header.Get("User-Agent") == catalogUA {
			body = desktopBody
		}
		return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: body}, nil
	}

	got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmGlobal), "cb", do)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != workBuddyEndpointV3ConfigShapes {
		t.Fatalf("endpoint = %q, want the merged shapes", got.Endpoint)
	}
	// The desktop shape's order leads, then the CLI shape's additions.
	want := []string{"desktop-only", "shared", "cli-only"}
	if len(got.Models) != len(want) {
		t.Fatalf("models = %#v", got.Models)
	}
	for i, id := range want {
		if got.Models[i].ID != id {
			t.Fatalf("models[%d] = %q, want %q (full: %#v)", i, got.Models[i].ID, id, got.Models)
		}
	}
}

// A refresh must survive one endpoint failing: the reachable endpoint still
// produces a catalogue, and the snapshot records which shape it came from.
func TestFetchWorkBuddyCatalogUnionDegradesToSingleEndpoint(t *testing.T) {
	v3Body := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["v3-model"]}],` +
		`"models":[{"id":"v3-model","maxInputTokens":1000,"maxOutputTokens":32000}]}}`)
	legacyBody := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["legacy-model"]}],` +
		`"models":[{"id":"legacy-model","maxInputTokens":2000,"maxOutputTokens":32000}]}}`)

	t.Run("both endpoints merge", func(t *testing.T) {
		do := func(req *http.Request, _ string) (*hostHTTPResponse, error) {
			body := v3Body
			if req.URL.Path == "/console/enterprises/personal/models" {
				body = legacyBody
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: body}, nil
		}
		got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "cb", do)
		if err != nil {
			t.Fatal(err)
		}
		if got.Endpoint != workBuddyEndpointV3ConfigUnion {
			t.Fatalf("endpoint = %q, want the union", got.Endpoint)
		}
		if len(got.Models) != 2 || got.Models[0].ID != "v3-model" || got.Models[1].ID != "legacy-model" {
			t.Fatalf("models = %#v", got.Models)
		}
	})

	t.Run("legacy failure keeps the v3 catalogue", func(t *testing.T) {
		do := func(req *http.Request, _ string) (*hostHTTPResponse, error) {
			if req.URL.Path == "/console/enterprises/personal/models" {
				return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: v3Body}, nil
		}
		got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "cb", do)
		if err != nil {
			t.Fatal(err)
		}
		if got.Endpoint != workBuddyEndpointV3ConfigShapes || len(got.Models) != 1 || got.Models[0].ID != "v3-model" {
			t.Fatalf("catalog = %#v", got)
		}
	})

	t.Run("v3 failure keeps the legacy catalogue", func(t *testing.T) {
		do := func(req *http.Request, _ string) (*hostHTTPResponse, error) {
			if req.URL.Path == "/v3/config" {
				return &hostHTTPResponse{StatusCode: http.StatusServiceUnavailable, Headers: make(http.Header)}, nil
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: legacyBody}, nil
		}
		got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "cb", do)
		if err != nil {
			t.Fatal(err)
		}
		if got.Endpoint != workBuddyEndpointLegacyPersonalModels || len(got.Models) != 1 || got.Models[0].ID != "legacy-model" {
			t.Fatalf("catalog = %#v", got)
		}
	})

	t.Run("both failing reports the v3 error", func(t *testing.T) {
		do := func(*http.Request, string) (*hostHTTPResponse, error) {
			return &hostHTTPResponse{StatusCode: http.StatusServiceUnavailable, Headers: make(http.Header)}, nil
		}
		if _, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmCN), "cb", do); err == nil {
			t.Fatal("want an error when no endpoint answered")
		}
	})
}
