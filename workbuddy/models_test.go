package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWBModelsReturnsOnlyAutoDefaultMetadata(t *testing.T) {
	models := wbModels()
	if len(models) != 1 {
		t.Fatalf("len(wbModels()) = %d, want 1", len(models))
	}
	want := pluginapi.ModelInfo{
		ID:                         "auto",
		Name:                       "auto",
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
	if !reflect.DeepEqual(models[0], want) {
		t.Fatalf("auto metadata = %#v, want %#v", models[0], want)
	}
}

func TestDefaultModelInfoUsesSourceNameThenID(t *testing.T) {
	if got := defaultModelInfo("serve-alpha", "Alpha"); got.Name != "Alpha" || got.ID != "serve-alpha" {
		t.Fatalf("named default = %#v", got)
	}
	if got := defaultModelInfo("serve-beta", ""); got.Name != "serve-beta" || got.ID != "serve-beta" {
		t.Fatalf("unnamed default = %#v", got)
	}
}

func TestModelForAuthReturnsResponseLocalReadyAndStaleModels(t *testing.T) {
	modelAliasCache.Lock()
	oldAliases := make(map[string]string, len(modelAliasCache.byAlias))
	for alias, model := range modelAliasCache.byAlias {
		oldAliases[alias] = model
	}
	modelAliasCache.Unlock()
	t.Cleanup(func() {
		modelAliasCache.Lock()
		modelAliasCache.byAlias = oldAliases
		modelAliasCache.Unlock()
	})

	for _, tt := range []struct {
		name      string
		stale     bool
		wantState modelReadinessState
	}{
		{name: "ready", wantState: modelReady},
		{name: "stale", stale: true, wantState: modelStale},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const (
				authID     = "auth-handler"
				callbackID = "callback-handler"
			)
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			store := newModelStore(t.TempDir())
			if tt.stale {
				identity, err := modelAuthIdentityFor(authID, sa)
				if err != nil {
					t.Fatal(err)
				}
				catalog := modelCatalogCacheV1{
					SchemaVersion:  modelCacheSchemaVersion,
					IdentitySHA256: identity.sha256(),
					Realm:          workBuddyRealmCN,
					FetchedAt:      time.Date(2026, time.August, 29, 1, 2, 3, 0, time.UTC),
					Endpoint:       workBuddyEndpointV3Config,
					Models:         []modelFacts{{ID: "serve-visible"}, {ID: "serve-hidden"}},
				}
				metadata := metadataCacheV1{
					SchemaVersion: modelCacheSchemaVersion,
					ETag:          `"handler-etag"`,
					FetchedAt:     time.Date(2026, time.August, 29, 4, 5, 6, 0, time.UTC),
					Records: map[string]modelFacts{
						"synthetic/serve-visible": {ID: "synthetic/serve-visible", Name: "Visible"},
						"synthetic/serve-hidden":  {ID: "synthetic/serve-hidden", Name: "Hidden"},
					},
				}
				if err := store.saveModels(catalog); err != nil {
					t.Fatal(err)
				}
				if err := store.saveMetadata(metadata); err != nil {
					t.Fatal(err)
				}
			}

			workBuddyCalls, metadataCalls := 0, 0
			runtime := newModelRuntime(store, func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
				if gotCallbackID != callbackID {
					t.Fatalf("callback ID = %q, want %q", gotCallbackID, callbackID)
				}
				switch req.URL.Host {
				case "copilot.tencent.com":
					workBuddyCalls++
					if tt.stale {
						return nil, errors.New("synthetic WorkBuddy outage")
					}
					return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-visible","serve-hidden"]}]}}`)}, nil
				case "models.dev":
					metadataCalls++
					if tt.stale {
						return nil, errors.New("synthetic metadata outage")
					}
					return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"synthetic/serve-visible":{"id":"serve-visible","name":"Visible"},"synthetic/serve-hidden":{"id":"serve-hidden","name":"Hidden"}}`)}, nil
				default:
					t.Fatalf("unexpected model request %s", req.URL)
					return nil, nil
				}
			})
			oldRuntime := activeModelRuntime.Swap(runtime)
			t.Cleanup(func() { activeModelRuntime.Store(oldRuntime) })

			raw, err := handleModelForAuth(mustJSON(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{
					AuthID:      authID,
					StorageJSON: mustJSON(sa),
					Host: pluginapi.HostConfigSummary{
						OAuthModelAlias: map[string][]pluginapi.ModelAlias{providerName: {{Name: "serve-visible", Alias: "visible-alias"}}},
						ExcludedModels:  map[string][]string{providerName: {"serve-hidden"}},
					},
				},
				HostCallbackID: callbackID,
			}))
			if err != nil {
				t.Fatal(err)
			}
			resp := decodeModelResponse(t, raw)
			if resp.Provider != providerName || len(resp.Models) != 1 || resp.Models[0].ID != "serve-visible" {
				t.Fatalf("model response = %#v", resp)
			}
			if workBuddyCalls != 1 || metadataCalls != 1 {
				t.Fatalf("source calls: WorkBuddy=%d models.dev=%d, want 1 each", workBuddyCalls, metadataCalls)
			}
			if got := resolveUpstreamModel("visible-alias", nil); got != "serve-visible" {
				t.Fatalf("cached alias resolved to %q", got)
			}

			resp.Models[0].ID = "mutated-response"
			resp.Models[0].SupportedGenerationMethods[0] = "mutated-response"
			snapshot := runtime.snapshotForAuthID(authID)
			if snapshot.State != tt.wantState || len(snapshot.Models) != 2 || snapshot.Models[0].ID != "serve-visible" || snapshot.Models[1].ID != "serve-hidden" || snapshot.Models[0].SupportedGenerationMethods[0] != "chat" {
				t.Fatalf("shared snapshot changed by response config: %#v", snapshot)
			}
			rendered, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			for _, responseOnly := range []string{"visible-alias", "mutated-response"} {
				if strings.Contains(string(rendered), responseOnly) {
					t.Fatalf("shared snapshot contains response-only value %q: %s", responseOnly, rendered)
				}
			}
		})
	}
}

func TestModelForAuthFailedAndNotStartedReturnEmptySuccess(t *testing.T) {
	assertEmpty := func(t *testing.T, raw []byte) {
		t.Helper()
		resp := decodeModelResponse(t, raw)
		if resp.Provider != providerName || resp.Models == nil || len(resp.Models) != 0 {
			t.Fatalf("blocked model response = %#v", resp)
		}
		for _, model := range resp.Models {
			if model.ID == "auto" {
				t.Fatal("blocked model response fell back to auto")
			}
		}
	}

	t.Run("failed", func(t *testing.T) {
		runtime := newModelRuntime(newModelStore(t.TempDir()), func(*http.Request, string) (*hostHTTPResponse, error) {
			t.Fatal("invalid auth performed model HTTP")
			return nil, nil
		})
		oldRuntime := activeModelRuntime.Swap(runtime)
		t.Cleanup(func() { activeModelRuntime.Store(oldRuntime) })
		raw, err := handleModelForAuth(mustJSON(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:      "auth-failed-handler",
			StorageJSON: []byte(`{"auth":{"accessToken":""}}`),
		}}))
		if err != nil {
			t.Fatal(err)
		}
		assertEmpty(t, raw)
		if got := runtime.snapshotForAuthID("auth-failed-handler"); got.State != modelFailed {
			t.Fatalf("failed snapshot = %#v", got)
		}
	})

	t.Run("not started after generation invalidation", func(t *testing.T) {
		const authID = "auth-not-started-handler"
		var runtime *modelRuntime
		calls := 0
		runtime = newModelRuntime(newModelStore(t.TempDir()), func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
			calls++
			if req.URL.Host != "copilot.tencent.com" || callbackID != "callback-not-started" {
				t.Fatalf("unexpected bootstrap request %s callback=%q", req.URL, callbackID)
			}
			runtime.advanceConfigGeneration()
			return modelRuntimeFreshWorkBuddyResponse(), nil
		})
		oldRuntime := activeModelRuntime.Swap(runtime)
		t.Cleanup(func() { activeModelRuntime.Store(oldRuntime) })
		raw, err := handleModelForAuth(mustJSON(authModelRequestWire{
			AuthModelRequest: pluginapi.AuthModelRequest{AuthID: authID, StorageJSON: mustJSON(syntheticStoredAuth(t, workBuddyRealmCN))},
			HostCallbackID:   "callback-not-started",
		}))
		if err != nil {
			t.Fatal(err)
		}
		assertEmpty(t, raw)
		if calls != 1 {
			t.Fatalf("bootstrap calls = %d, want 1", calls)
		}
		if got := runtime.snapshotForAuthID(authID); got.State != modelNotStarted {
			t.Fatalf("invalidated snapshot = %#v", got)
		}
	})
}

func TestModelForAuthStaticRemainsAutoWithoutRuntimeBootstrap(t *testing.T) {
	runtime := installModelStatesForTest(t, map[string]modelReadinessState{"dynamic-auth": modelReady})
	snapshot := runtime.snapshotForAuthID("dynamic-auth")
	snapshot.Models = []pluginapi.ModelInfo{defaultModelInfo("dynamic-only", "Dynamic Only")}
	runtime.authSlot("dynamic-auth").current.Store(&snapshot)

	for i := 0; i < 2; i++ {
		raw, err := handleModelStatic(mustJSON(pluginapi.StaticModelRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		resp := decodeModelResponse(t, raw)
		if len(resp.Models) != 1 || resp.Models[0].ID != "auto" {
			t.Fatalf("static model response = %#v", resp)
		}
	}
}

func TestModelForAuthPluginLifecycleDoesNotBootstrap(t *testing.T) {
	installModelStatesForTest(t, nil)
	rawConfig := mustJSON(map[string]any{"config_yaml": []byte("usage_report_url: http://127.0.0.1:1\n")})
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		raw, err := handleMethod(method, rawConfig)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK {
			t.Fatalf("%s response = %#v", method, env)
		}
	}
}

func decodeModelResponse(t *testing.T, raw []byte) pluginapi.ModelResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("model envelope = %#v", env)
	}
	var resp pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
