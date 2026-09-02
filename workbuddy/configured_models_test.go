package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseFeatureRuntimeConfiguredModelsDynamicDefaults(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  []byte
	}{
		{name: "missing"},
		{name: "null", raw: []byte("models: null\n")},
		{name: "empty inline", raw: []byte("models: []\n")},
		{name: "empty block", raw: []byte("models:\n  []\n")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseFeatureRuntime(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.configuredModels) != 0 {
				t.Fatalf("configured models = %#v, want dynamic discovery", cfg.configuredModels)
			}
		})
	}
}

func TestParseFeatureRuntimeConfiguredModelsPreservesTrimmedOrder(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "inline strings",
			raw:  "models: ['  serve-alpha  ', Serve-alpha, serve beta, \"123\"]\n",
			want: []string{"serve-alpha", "Serve-alpha", "serve beta", "123"},
		},
		{
			name: "block strings and Unicode surrounding whitespace",
			raw:  "models:\n  - \"\\u2003serve-one\\u3000\"\n  - 'serve  two'\n",
			want: []string{"serve-one", "serve  two"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseFeatureRuntime([]byte(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			if !sameStrings(cfg.configuredModels, tt.want) {
				t.Fatalf("configured models = %#v, want %#v", cfg.configuredModels, tt.want)
			}
		})
	}
}

func TestParseFeatureRuntimeConfiguredModelsRejectsInvalidYAMLValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
	}{
		{name: "wrong top-level kind", raw: "- models\n"},
		{name: "models is scalar", raw: "models: fixed\n"},
		{name: "null-tagged sequence", raw: "models: !!null [serve-alpha]\n"},
		{name: "null-tagged mapping", raw: "models: !!null {id: serve-alpha}\n"},
		{name: "null-tagged non-null scalar", raw: "models: !!null serve-beta\n"},
		{name: "map-tagged sequence", raw: "models: !!map [serve-alpha]\n"},
		{name: "string-tagged sequence", raw: "models: !!str [serve-alpha]\n"},
		{name: "custom-tagged sequence", raw: "models: !catalog [serve-alpha]\n"},
		{name: "non-specific tagged sequence", raw: "models: ! [serve-alpha]\n"},
		{name: "object entry", raw: "models: [{id: serve-alpha}]\n"},
		{name: "nested list entry", raw: "models: [[serve-alpha]]\n"},
		{name: "number entry", raw: "models: [123]\n"},
		{name: "explicitly tagged string entry", raw: "models: [!!str 123]\n"},
		{name: "boolean entry", raw: "models: [true]\n"},
		{name: "null entry", raw: "models: [null]\n"},
		{name: "literal multiline entry", raw: "models:\n  - |-\n    serve-alpha\n    scheduler_mode: credits\n"},
		{name: "escaped multiline entry", raw: "models: [\"serve-alpha\\nserve-beta\"]\n"},
		{name: "escaped leading newline", raw: "models: [\"\\nserve-alpha\"]\n"},
		{name: "escaped trailing newline", raw: "models: [\"serve-alpha\\n\"]\n"},
		{name: "escaped next-line separator", raw: "models: [\"serve-alpha\\Nserve-beta\"]\n"},
		{name: "escaped line separator", raw: "models: [\"serve-alpha\\Lserve-beta\"]\n"},
		{name: "escaped paragraph separator", raw: "models: [\"serve-alpha\\Pserve-beta\"]\n"},
		{name: "whitespace-only entry", raw: "models: [\" \\t\\u3000 \"]\n"},
		{name: "over 512 bytes", raw: "models: ['" + strings.Repeat("x", maxDiscoveredModelIDBytes+1) + "']\n"},
		{name: "duplicate after trim", raw: "models: [serve-alpha, ' serve-alpha ']\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if cfg, err := parseFeatureRuntime([]byte(tt.raw)); err == nil {
				t.Fatalf("configured models = %#v, want error", cfg.configuredModels)
			}
		})
	}
}

func TestParseTopLevelConfigScalarsRejectsWrongTypesAndMerges(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
	}{
		{name: "management sequence", raw: "management_key: [replacement]\n"},
		{name: "management boolean", raw: "management_key: true\n"},
		{name: "lifecycle sequence", raw: "lifecycle_auto: [false]\n"},
		{name: "scheduler incompatible tag", raw: "scheduler_mode: !!seq credits\n"},
		{name: "usage key mapping", raw: "usage_report_key: {value: secret}\n"},
		{name: "root merge", raw: "defaults: &d\n  lifecycle_auto: false\n  scheduler_mode: credits\n  models: [serve-alpha]\n<<: *d\n"},
		{name: "alias key", raw: "key_name: &k management_key\n*k: [replacement]\n"},
		{name: "alias value", raw: "replacement: &v secret\nmanagement_key: *v\n"},
		{name: "unused anchor", raw: "management_key: &key secret\n"},
		{name: "false tagged null", raw: "lifecycle_auto: !!null false\n"},
		{name: "on tagged integer", raw: "lifecycle_auto: !!int on\n"},
		{name: "management tagged null", raw: "management_key: !!null replacement\n"},
		{name: "proxy tagged null", raw: "proxy-url: !!null direct\n"},
		{name: "proxy non-specific tag", raw: "proxy-url: ! null\n"},
		{name: "multiple documents", raw: "proxy-url:\n---\nproxy-url: [not-a-string]\n"},
		{name: "duplicate proxy key", raw: "proxy-url: http://proxy-a.example\nproxy-url: http://proxy-b.example\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if values, err := parseTopLevelConfigScalars([]byte(tt.raw)); err == nil {
				t.Fatalf("config scalars = %#v, want error", values)
			}
		})
	}
}

func TestParseTopLevelConfigScalarsAcceptsYAMLBooleanCase(t *testing.T) {
	values, err := parseTopLevelConfigScalars([]byte("lifecycle_auto: True\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !enabledConfigValue(values["lifecycle_auto"]) {
		t.Fatalf("lifecycle_auto = %q, want enabled", values["lifecycle_auto"])
	}
}

func TestConfigureDuplicateProxyFailsClosed(t *testing.T) {
	oldProxy := proxyState.Load()
	t.Cleanup(func() { proxyState.Store(oldProxy) })
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})

	err := configure(mustJSON(map[string]any{
		"config_yaml": []byte("proxy-url: http://proxy-a.example\nproxy-url: http://proxy-b.example\n"),
	}))
	if err == nil {
		t.Fatal("duplicate proxy-url was accepted")
	}
	if got := currentProxyState().mode; got != proxyModeBlocked {
		t.Fatalf("proxy mode = %v, want blocked", got)
	}
}

func TestConfigureConfiguredModelsIgnoresScalarContinuationSettings(t *testing.T) {
	runtime := newModelRuntime(newModelStore(t.TempDir()), nil)
	previousRuntime := activeModelRuntime.Swap(runtime)
	oldFeatures := featureRuntime.Load()
	oldProxy := proxyState.Load()
	restoreScheduler := setSchedulerMode(schedulerModeOff)
	usageReportMu.RLock()
	oldUsageURL, oldUsageKey := usageReportURL, usageReportKey
	usageReportMu.RUnlock()
	t.Cleanup(func() {
		activeModelRuntime.Store(previousRuntime)
		featureRuntime.Store(oldFeatures)
		proxyState.Store(oldProxy)
		restoreScheduler()
		usageReportMu.Lock()
		usageReportURL, usageReportKey = oldUsageURL, oldUsageKey
		usageReportMu.Unlock()
	})

	for _, raw := range []string{
		"models:\n  - >-\n    serve-alpha\n    scheduler_mode: credits\nusage_report_url: http://127.0.0.1:1\n",
		"models:\n  - \"serve-alpha\n    scheduler_mode: credits\"\nusage_report_url: http://127.0.0.1:1\n",
	} {
		if err := configure(mustJSON(map[string]any{"config_yaml": []byte(raw)})); err != nil {
			t.Fatal(err)
		}
		if got := loadedSchedulerMode(); got != schedulerModeOff {
			t.Fatalf("scheduler mode = %q, want %q", got, schedulerModeOff)
		}
	}
}

func TestFeatureRuntimeConfiguredModelsSnapshotIsImmutable(t *testing.T) {
	old := featureRuntime.Load()
	t.Cleanup(func() { featureRuntime.Store(old) })

	cfg, err := parseFeatureRuntime([]byte("models: [serve-alpha, serve-beta]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)

	first := currentFeatureRuntime()
	first.configuredModels[0] = "mutated"
	second := currentFeatureRuntime()
	if !sameStrings(second.configuredModels, []string{"serve-alpha", "serve-beta"}) {
		t.Fatalf("mutating caller snapshot changed configured models: %#v", second.configuredModels)
	}
}

func TestConfigureConfiguredModelsRetainsInvalidStateAndInvalidatesValidGenerations(t *testing.T) {
	runtime := newModelRuntime(newModelStore(t.TempDir()), nil)
	previousRuntime := activeModelRuntime.Swap(runtime)
	oldFeatures := featureRuntime.Load()
	oldProxy := proxyState.Load()
	usageReportMu.RLock()
	oldUsageURL, oldUsageKey := usageReportURL, usageReportKey
	usageReportMu.RUnlock()
	t.Cleanup(func() {
		activeModelRuntime.Store(previousRuntime)
		featureRuntime.Store(oldFeatures)
		proxyState.Store(oldProxy)
		usageReportMu.Lock()
		usageReportURL, usageReportKey = oldUsageURL, oldUsageKey
		usageReportMu.Unlock()
	})

	seedAuth := func() {
		generation := runtime.configGeneration.Load()
		snapshot := modelReadinessSnapshot{State: modelReady, Models: []pluginapi.ModelInfo{}, configGeneration: generation}
		runtime.authSlot("auth-configured-models").current.Store(&snapshot)
	}
	configureModels := func(value string) error {
		return configure(mustJSON(map[string]any{
			"config_yaml": []byte("models: " + value + "\nusage_report_url: http://127.0.0.1:1\n"),
		}))
	}

	seedAuth()
	if err := configureModels("[serve-alpha, serve-beta]"); err != nil {
		t.Fatal(err)
	}
	if got := runtime.configGeneration.Load(); got != 1 {
		t.Fatalf("first generation = %d, want 1", got)
	}
	if got := currentFeatureRuntime().configuredModels; !sameStrings(got, []string{"serve-alpha", "serve-beta"}) {
		t.Fatalf("first configured models = %#v", got)
	}
	if got := runtime.snapshotForAuthID("auth-configured-models"); got.State != modelNotStarted || got.configGeneration != 1 {
		t.Fatalf("first invalidated snapshot = %#v", got)
	}

	beforeFeature := featureRuntime.Load()
	beforeGeneration := runtime.configGeneration.Load()
	if err := configureModels("[serve-alpha, ' serve-alpha ']"); err == nil {
		t.Fatal("duplicate configured models were accepted")
	}
	if featureRuntime.Load() != beforeFeature {
		t.Fatal("invalid configure replaced the feature snapshot")
	}
	if got := runtime.configGeneration.Load(); got != beforeGeneration {
		t.Fatalf("invalid configure generation = %d, want %d", got, beforeGeneration)
	}

	seedAuth()
	if err := configureModels("[serve-gamma]"); err != nil {
		t.Fatal(err)
	}
	if got := runtime.configGeneration.Load(); got != 2 {
		t.Fatalf("changed-list generation = %d, want 2", got)
	}
	if got := runtime.snapshotForAuthID("auth-configured-models"); got.State != modelNotStarted || got.configGeneration != 2 {
		t.Fatalf("changed-list invalidated snapshot = %#v", got)
	}

	seedAuth()
	if err := configureModels("[]"); err != nil {
		t.Fatal(err)
	}
	if got := runtime.configGeneration.Load(); got != 3 {
		t.Fatalf("dynamic generation = %d, want 3", got)
	}
	if got := currentFeatureRuntime().configuredModels; len(got) != 0 {
		t.Fatalf("dynamic configured models = %#v", got)
	}
	if got := runtime.snapshotForAuthID("auth-configured-models"); got.State != modelNotStarted || got.configGeneration != 3 {
		t.Fatalf("dynamic invalidated snapshot = %#v", got)
	}
}

func TestModelRuntimeCommitsFeatureSnapshotWithConfigGeneration(t *testing.T) {
	runtime := newModelRuntime(newModelStore(t.TempDir()), nil)
	oldFeatures := featureRuntime.Load()
	initial, err := parseFeatureRuntime([]byte("models: [serve-old]\n"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := parseFeatureRuntime([]byte("models: [serve-new]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(initial)
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })

	runtime.configCommitMu.RLock()
	started := make(chan struct{})
	done := make(chan uint64, 1)
	go func() {
		close(started)
		done <- runtime.commitFeatureRuntime(next)
	}()
	<-started
	select {
	case generation := <-done:
		t.Fatalf("commit completed under read lock with generation %d", generation)
	case <-time.After(20 * time.Millisecond):
	}
	if got := runtime.configGeneration.Load(); got != 0 {
		t.Fatalf("generation changed before feature commit: %d", got)
	}
	if got := currentFeatureRuntime().configuredModels; !sameStrings(got, []string{"serve-old"}) {
		t.Fatalf("feature changed before generation commit: %#v", got)
	}
	runtime.configCommitMu.RUnlock()

	if got := <-done; got != 1 {
		t.Fatalf("committed generation = %d, want 1", got)
	}
	if got := currentFeatureRuntime().configuredModels; !sameStrings(got, []string{"serve-new"}) {
		t.Fatalf("committed feature = %#v", got)
	}
	next.configuredModels[0] = "mutated-after-commit"
	if got := currentFeatureRuntime().configuredModels; !sameStrings(got, []string{"serve-new"}) {
		t.Fatalf("committed feature aliases caller slice: %#v", got)
	}
}

func TestConfiguredModelsBypassCatalogCacheAndUseModelsDevMetadata(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	identity, err := modelAuthIdentityFor("auth-configured-bypass", sa)
	if err != nil {
		t.Fatal(err)
	}
	catalog := modelCatalogCacheV1{
		SchemaVersion:  modelCacheSchemaVersion,
		IdentitySHA256: identity.sha256(),
		Realm:          workBuddyRealmCN,
		FetchedAt:      time.Date(2026, time.August, 28, 1, 2, 3, 0, time.UTC),
		Endpoint:       workBuddyEndpointV3Config,
		Models:         []modelFacts{{ID: "cached-older"}},
	}
	if err := store.saveModels(catalog); err != nil {
		t.Fatal(err)
	}
	catalog.FetchedAt = catalog.FetchedAt.Add(time.Minute)
	catalog.Models = []modelFacts{{ID: "cached-newer"}}
	if err := store.saveModels(catalog); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(root, "models", identity.sha256()+".json")
	primaryBefore := modelStoreReadFile(t, modelPath)
	backupBefore := modelStoreReadFile(t, modelPath+".bak")

	workBuddyCalls, metadataCalls := 0, 0
	runtime := newModelRuntime(store, func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			workBuddyCalls++
			return nil, errors.New("configured mode requested WorkBuddy")
		case "models.dev":
			metadataCalls++
			if callbackID != "callback-configured-bypass" {
				t.Fatalf("models.dev callback ID = %q", callbackID)
			}
			return &hostHTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"ETag": []string{`"configured-etag"`}},
				Body: []byte(`{"synthetic/serve-first":{"id":"serve-first","name":"First configured","limit":{"context":1111}},` +
					`"synthetic/serve-second":{"id":"serve-second","name":"Second configured","limit":{"context":2222}}}`),
			}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	})
	oldRuntime := activeModelRuntime.Swap(runtime)
	oldFeatures := featureRuntime.Load()
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		featureRuntime.Store(oldFeatures)
	})
	cfg, err := parseFeatureRuntime([]byte("models: [serve-second, serve-first]\n"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.commitFeatureRuntime(cfg)

	raw, err := handleModelForAuth(mustJSON(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-configured-bypass", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-configured-bypass",
	}))
	if err != nil {
		t.Fatal(err)
	}
	response := decodeModelResponse(t, raw)
	if len(response.Models) != 2 || response.Models[0].ID != "serve-second" || response.Models[0].Name != "Second configured" || response.Models[0].ContextLength != 2222 || response.Models[1].ID != "serve-first" || response.Models[1].Name != "First configured" || response.Models[1].ContextLength != 1111 {
		t.Fatalf("configured response models = %#v", response.Models)
	}
	got := runtime.snapshotForAuthID("auth-configured-bypass")
	if got.State != modelReady || got.ModelSource != modelSourceConfig || got.MetadataSource != modelSourceFresh || !got.ModelsFetchedAt.IsZero() {
		t.Fatalf("configured snapshot = %#v", got)
	}
	if workBuddyCalls != 0 || metadataCalls != 1 {
		t.Fatalf("source calls: WorkBuddy=%d models.dev=%d", workBuddyCalls, metadataCalls)
	}
	if after := modelStoreReadFile(t, modelPath); string(after) != string(primaryBefore) {
		t.Fatalf("configured mode rewrote catalog primary: before=%s after=%s", primaryBefore, after)
	}
	if after := modelStoreReadFile(t, modelPath+".bak"); string(after) != string(backupBefore) {
		t.Fatalf("configured mode rewrote catalog backup: before=%s after=%s", backupBefore, after)
	}
	entries, err := os.ReadDir(filepath.Join(root, "models"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("configured mode wrote catalog files: %#v", entries)
	}
	metadata, found, err := store.loadMetadata()
	if err != nil || !found || metadata.ETag != `"configured-etag"` {
		t.Fatalf("persisted metadata = %#v, found=%v err=%v", metadata, found, err)
	}
}

func TestConfiguredModelsMetadataReadinessMatrix(t *testing.T) {
	for _, tt := range []struct {
		name               string
		seedMetadata       bool
		responseStatus     int
		responseErr        error
		wantState          modelReadinessState
		wantMetadataSource modelSnapshotSource
		wantError          modelErrorCode
		wantName           string
		wantFetchedAt      bool
	}{
		{name: "fresh success", responseStatus: http.StatusOK, wantState: modelReady, wantMetadataSource: modelSourceFresh, wantName: "Fresh configured", wantFetchedAt: true},
		{name: "not modified with valid cache", seedMetadata: true, responseStatus: http.StatusNotModified, wantState: modelReady, wantMetadataSource: modelSourceFresh, wantName: "Cached configured", wantFetchedAt: true},
		{name: "failure with valid last-good", seedMetadata: true, responseErr: errors.New("synthetic metadata failure"), wantState: modelStale, wantMetadataSource: modelSourceCache, wantError: modelErrorModelsDevTransport, wantName: "Cached configured", wantFetchedAt: true},
		{name: "failure without valid metadata", responseErr: errors.New("synthetic metadata failure"), wantState: modelFailed, wantMetadataSource: modelSourceNone, wantError: modelErrorModelsDevTransport},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			store := newModelStore(root)
			cachedAt := time.Date(2026, time.August, 28, 5, 6, 7, 0, time.UTC)
			if tt.seedMetadata {
				cached := metadataCacheV1{
					SchemaVersion: modelCacheSchemaVersion,
					ETag:          `"cached-configured-etag"`,
					FetchedAt:     cachedAt,
					Records: map[string]modelFacts{
						"synthetic/serve-configured": {ID: "synthetic/serve-configured", Name: "Cached configured"},
					},
				}
				if err := store.saveMetadata(cached); err != nil {
					t.Fatal(err)
				}
			}

			workBuddyCalls, metadataCalls := 0, 0
			runtime := newModelRuntime(store, func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				switch req.URL.Host {
				case "copilot.tencent.com":
					workBuddyCalls++
					return modelRuntimeFreshWorkBuddyResponse(), nil
				case "models.dev":
					metadataCalls++
					if callbackID != "callback-configured-matrix" {
						t.Fatalf("models.dev callback ID = %q", callbackID)
					}
					if tt.seedMetadata && req.Header.Get("If-None-Match") != `"cached-configured-etag"` {
						t.Fatalf("If-None-Match = %q", req.Header.Get("If-None-Match"))
					}
					if tt.responseErr != nil {
						return nil, tt.responseErr
					}
					if tt.responseStatus == http.StatusNotModified {
						return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: make(http.Header)}, nil
					}
					return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"ETag": []string{`"fresh-configured-etag"`}}, Body: []byte(`{"synthetic/serve-configured":{"id":"serve-configured","name":"Fresh configured"}}`)}, nil
				default:
					t.Fatalf("unexpected model request %s", req.URL)
					return nil, nil
				}
			})
			oldFeatures := featureRuntime.Load()
			t.Cleanup(func() { featureRuntime.Store(oldFeatures) })
			cfg, err := parseFeatureRuntime([]byte("models: [serve-configured]\n"))
			if err != nil {
				t.Fatal(err)
			}
			runtime.commitFeatureRuntime(cfg)

			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-configured-matrix", StorageJSON: mustJSON(syntheticStoredAuth(t, workBuddyRealmCN))},
				HostCallbackID:   "callback-configured-matrix",
			})
			if workBuddyCalls != 0 || metadataCalls != 1 {
				t.Fatalf("source calls: WorkBuddy=%d models.dev=%d", workBuddyCalls, metadataCalls)
			}
			if got.State != tt.wantState || got.ModelSource != modelSourceConfig || got.MetadataSource != tt.wantMetadataSource || got.ErrorCode != tt.wantError || !got.ModelsFetchedAt.IsZero() {
				t.Fatalf("configured metadata snapshot = %#v", got)
			}
			if got.MetadataFetchedAt.IsZero() == tt.wantFetchedAt {
				t.Fatalf("metadata fetched_at = %s, want present=%v", got.MetadataFetchedAt, tt.wantFetchedAt)
			}
			if tt.wantState == modelFailed {
				if got.executable() || got.Models == nil || len(got.Models) != 0 {
					t.Fatalf("failed configured snapshot = %#v", got)
				}
				return
			}
			if !got.executable() || len(got.Models) != 1 || got.Models[0].ID != "serve-configured" || got.Models[0].Name != tt.wantName {
				t.Fatalf("configured metadata models = %#v", got.Models)
			}
			if tt.responseStatus == http.StatusOK {
				if persisted, found, err := store.loadMetadata(); err != nil || !found || persisted.ETag != `"fresh-configured-etag"` {
					t.Fatalf("fresh metadata persisted=%#v found=%v err=%v", persisted, found, err)
				}
			}
		})
	}
}

func TestConfiguredModelsValidateAuthIdentityAndTokenGeneration(t *testing.T) {
	metadataCalls := 0
	runtime := newModelRuntime(newModelStore(t.TempDir()), func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if req.URL.Host != "models.dev" {
			t.Fatalf("configured validation requested %s", req.URL)
		}
		metadataCalls++
		return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"synthetic/serve-configured":{"id":"serve-configured"}}`)}, nil
	})
	oldFeatures := featureRuntime.Load()
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })
	cfg, err := parseFeatureRuntime([]byte("models: [serve-configured]\n"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.commitFeatureRuntime(cfg)

	unsupportedRealm := syntheticStoredAuth(t, workBuddyRealmCN)
	unsupportedRealm.Auth.AccessToken = syntheticAccessToken(t, "https://unsupported.example/realms/cli")
	missingIdentity := syntheticStoredAuth(t, workBuddyRealmCN)
	missingIdentity.Account.UID = ""
	for _, req := range []authModelRequestWire{
		{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-unsupported-realm", StorageJSON: mustJSON(unsupportedRealm)}},
		{AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: mustJSON(missingIdentity)}},
	} {
		got := runtime.ensureForAuth(req)
		if got.State != modelFailed || got.ErrorCode != modelErrorAuthInvalid || got.executable() || len(got.Models) != 0 {
			t.Fatalf("invalid configured auth snapshot = %#v", got)
		}
	}
	if metadataCalls != 0 {
		t.Fatalf("invalid configured auth made %d metadata calls", metadataCalls)
	}

	wwwIssuerAuth := syntheticStoredAuth(t, workBuddyRealmCN)
	wwwIssuerAuth.Auth.AccessToken = syntheticAccessToken(t, "https://www.codebuddy.cn/auth/realms/copilot")
	wwwIssuer := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-www-codebuddy-issuer", StorageJSON: mustJSON(wwwIssuerAuth)},
	})
	if wwwIssuer.State != modelReady || wwwIssuer.ModelSource != modelSourceConfig || !wwwIssuer.executable() || len(wwwIssuer.Models) != 1 {
		t.Fatalf("www.codebuddy.cn configured snapshot = %#v", wwwIssuer)
	}

	firstAuth := syntheticStoredAuth(t, workBuddyRealmCN)
	first := runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-token-generation", StorageJSON: mustJSON(firstAuth)}})
	secondAuth := *firstAuth
	secondAuth.Auth.AccessToken = syntheticAccessToken(t, "https://codebuddy.cn/realms/other")
	second := runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-token-generation", StorageJSON: mustJSON(&secondAuth)}})
	if first.State != modelReady || second.State != modelReady || second.authGeneration != first.authGeneration+1 || second.configGeneration != first.configGeneration {
		t.Fatalf("token generations: first=%#v second=%#v", first, second)
	}
	if metadataCalls != 1 {
		t.Fatalf("valid configured auth metadata calls = %d, want shared 1", metadataCalls)
	}
}

func TestConfiguredModelsSwitchToEmptyResumesWorkBuddyCacheDiscovery(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	identity, err := modelAuthIdentityFor("auth-configured-switch", sa)
	if err != nil {
		t.Fatal(err)
	}
	catalog := modelCatalogCacheV1{
		SchemaVersion:  modelCacheSchemaVersion,
		IdentitySHA256: identity.sha256(),
		Realm:          workBuddyRealmCN,
		FetchedAt:      time.Date(2026, time.August, 28, 8, 9, 10, 0, time.UTC),
		Endpoint:       workBuddyEndpointV3Config,
		Models:         []modelFacts{{ID: "cached-dynamic"}},
	}
	if err := store.saveModels(catalog); err != nil {
		t.Fatal(err)
	}
	catalogBefore := modelStoreReadFile(t, filepath.Join(root, "models", identity.sha256()+".json"))

	workBuddyCalls, metadataCalls := 0, 0
	runtime := newModelRuntime(store, func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			workBuddyCalls++
			return nil, errors.New("synthetic WorkBuddy outage")
		case "models.dev":
			metadataCalls++
			if callbackID != "callback-configured-switch" {
				t.Fatalf("models.dev callback ID = %q", callbackID)
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"synthetic/serve-configured":{"id":"serve-configured"},"synthetic/cached-dynamic":{"id":"cached-dynamic"}}`)}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	})
	oldRuntime := activeModelRuntime.Swap(runtime)
	oldFeatures := featureRuntime.Load()
	oldProxy := proxyState.Load()
	usageReportMu.RLock()
	oldUsageURL, oldUsageKey := usageReportURL, usageReportKey
	usageReportMu.RUnlock()
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		featureRuntime.Store(oldFeatures)
		proxyState.Store(oldProxy)
		usageReportMu.Lock()
		usageReportURL, usageReportKey = oldUsageURL, oldUsageKey
		usageReportMu.Unlock()
	})
	configureModels := func(value string) {
		t.Helper()
		if err := configure(mustJSON(map[string]any{"config_yaml": []byte("models: " + value + "\nusage_report_url: http://127.0.0.1:1\n")})); err != nil {
			t.Fatal(err)
		}
	}

	configureModels("[serve-configured]")
	configured := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-configured-switch", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-configured-switch",
	})
	if configured.State != modelReady || configured.ModelSource != modelSourceConfig || len(configured.Models) != 1 || configured.Models[0].ID != "serve-configured" || workBuddyCalls != 0 {
		t.Fatalf("configured snapshot = %#v, WorkBuddy calls=%d", configured, workBuddyCalls)
	}

	configureModels("[]")
	dynamic := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-configured-switch", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-configured-switch",
	})
	if dynamic.State != modelStale || dynamic.ModelSource != modelSourceCache || dynamic.ErrorCode != modelErrorWorkBuddyTransport || len(dynamic.Models) != 1 || dynamic.Models[0].ID != "cached-dynamic" {
		t.Fatalf("dynamic snapshot = %#v", dynamic)
	}
	if workBuddyCalls != 1 || metadataCalls != 1 {
		t.Fatalf("source calls after switch: WorkBuddy=%d models.dev=%d", workBuddyCalls, metadataCalls)
	}
	if after := modelStoreReadFile(t, filepath.Join(root, "models", identity.sha256()+".json")); string(after) != string(catalogBefore) {
		t.Fatalf("dynamic fallback rewrote catalog: before=%s after=%s", catalogBefore, after)
	}
}
