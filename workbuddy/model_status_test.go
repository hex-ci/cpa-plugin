package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBuildModelStatusUsesFixedPriorityAndSafeAuthIndex(t *testing.T) {
	runtime := installModelStatesForTest(t, map[string]modelReadinessState{
		"internal-ready":  modelReady,
		"internal-stale":  modelStale,
		"internal-failed": modelFailed,
	})
	modelsFetchedAt := time.Date(2026, 8, 29, 12, 34, 56, 0, time.FixedZone("UTC+8", 8*60*60))
	for _, update := range []struct {
		authID    string
		source    modelSnapshotSource
		fetchedAt time.Time
		errorCode modelErrorCode
	}{
		{authID: "internal-ready", source: modelSourceFresh, fetchedAt: modelsFetchedAt},
		{authID: "internal-stale", source: modelSourceCache, fetchedAt: modelsFetchedAt},
		{authID: "internal-failed", source: modelSourceNone, errorCode: modelErrorWorkBuddyHTTP},
	} {
		snapshot := runtime.snapshotForAuthID(update.authID)
		snapshot.ModelSource = update.source
		snapshot.ModelsFetchedAt = update.fetchedAt
		snapshot.ErrorCode = update.errorCode
		runtime.authSlot(update.authID).current.Store(&snapshot)
	}
	metadataFetchedAt := time.Date(2026, 8, 30, 1, 2, 3, 0, time.FixedZone("UTC-7", -7*60*60))
	runtime.metadataMu.Lock()
	runtime.metadataResult = &modelMetadataResult{
		source: modelSourceCache,
		cache: metadataCacheV1{
			FetchedAt: metadataFetchedAt,
		},
		ok: true,
	}
	runtime.metadataMu.Unlock()

	got := buildModelStatus([]pluginapi.HostAuthFileEntry{
		{ID: "internal-ready", AuthIndex: "account-1"},
		{ID: "internal-stale", AuthIndex: "account-2"},
		{ID: "internal-failed", AuthIndex: "account-3"},
	})
	if got.State != modelFailed || len(got.Auths) != 3 {
		t.Fatalf("status = %#v", got)
	}
	if got.Auths[0].AuthIndex != "account-1" || got.Auths[1].State != modelStale || got.Auths[2].State != modelFailed {
		t.Fatalf("auth statuses = %#v", got.Auths)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"failed","message":"模型目录不可用","metadata_source":"cache","metadata_fetched_at":"2026-08-30T08:02:03Z","auths":[{"auth_index":"account-1","state":"ready","model_source":"fresh","models_fetched_at":"2026-08-29T04:34:56Z","error_code":""},{"auth_index":"account-2","state":"stale","model_source":"cache","models_fetched_at":"2026-08-29T04:34:56Z","error_code":""},{"auth_index":"account-3","state":"failed","model_source":"none","models_fetched_at":"","error_code":"workbuddy_http"}]}`
	if string(raw) != want {
		t.Fatalf("status JSON = %s, want %s", raw, want)
	}
	for _, forbidden := range []string{"internal-", "auth_id", "token", "endpoint", "path", "body", "query", "raw error"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("unsafe status JSON contains %q: %s", forbidden, raw)
		}
	}
}

func TestBuildModelStatusUsesHostFileIDForIndependentAuthStates(t *testing.T) {
	installModelStatesForTest(t, map[string]modelReadinessState{
		"internal-a": modelReady,
		"internal-b": modelStale,
		"account-1":  modelFailed,
		"account-2":  modelFailed,
	})
	got := buildModelStatus([]pluginapi.HostAuthFileEntry{
		{ID: "internal-a", AuthIndex: "account-1"},
		{ID: "internal-b", AuthIndex: "account-2"},
	})
	if len(got.Auths) != 2 || got.Auths[0].AuthIndex != "account-1" || got.Auths[0].State != modelReady || got.Auths[1].AuthIndex != "account-2" || got.Auths[1].State != modelStale {
		t.Fatalf("auth statuses = %#v", got.Auths)
	}
}

func TestBuildModelStatusSerializesConfiguredCatalogSource(t *testing.T) {
	runtime := installModelStatesForTest(t, map[string]modelReadinessState{"internal-configured": modelReady})
	snapshot := runtime.snapshotForAuthID("internal-configured")
	snapshot.ModelSource = modelSourceConfig
	snapshot.ModelsFetchedAt = time.Time{}
	runtime.authSlot("internal-configured").current.Store(&snapshot)
	metadataFetchedAt := time.Date(2026, time.August, 30, 9, 10, 11, 0, time.UTC)
	runtime.metadataMu.Lock()
	runtime.metadataResult = &modelMetadataResult{
		source: modelSourceFresh,
		cache:  metadataCacheV1{FetchedAt: metadataFetchedAt},
		ok:     true,
	}
	runtime.metadataMu.Unlock()

	got := buildModelStatus([]pluginapi.HostAuthFileEntry{{ID: "internal-configured", AuthIndex: "account-configured"}})
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"ready","message":"模型目录已就绪","metadata_source":"fresh","metadata_fetched_at":"2026-08-30T09:10:11Z","auths":[{"auth_index":"account-configured","state":"ready","model_source":"config","models_fetched_at":"","error_code":""}]}`
	if string(raw) != want {
		t.Fatalf("status JSON = %s, want %s", raw, want)
	}
}

func TestBuildModelStatusAggregationPriority(t *testing.T) {
	wantPriority := map[modelReadinessState]int{
		modelReady:      1,
		modelStale:      2,
		modelNotStarted: 3,
		modelLoading:    4,
		modelFailed:     5,
	}
	if len(modelStatePriority) != len(wantPriority) {
		t.Fatalf("priority count = %d, want %d", len(modelStatePriority), len(wantPriority))
	}
	for state, want := range wantPriority {
		if got := modelStatePriority[state]; got != want {
			t.Fatalf("priority[%q] = %d, want %d", state, got, want)
		}
	}

	tests := []struct {
		name        string
		states      []modelReadinessState
		want        modelReadinessState
		wantMessage string
	}{
		{name: "zero auth", want: modelNotStarted, wantMessage: "模型目录尚未初始化"},
		{name: "ready", states: []modelReadinessState{modelReady}, want: modelReady, wantMessage: "模型目录已就绪"},
		{name: "stale over ready", states: []modelReadinessState{modelReady, modelStale}, want: modelStale, wantMessage: "模型目录刷新失败，正在使用上次有效缓存"},
		{name: "not started over stale", states: []modelReadinessState{modelStale, modelNotStarted}, want: modelNotStarted, wantMessage: "模型目录尚未初始化"},
		{name: "loading over not started", states: []modelReadinessState{modelNotStarted, modelLoading}, want: modelLoading, wantMessage: "模型目录正在初始化"},
		{name: "failed over loading", states: []modelReadinessState{modelLoading, modelFailed}, want: modelFailed, wantMessage: "模型目录不可用"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := make(map[string]modelReadinessState, len(tt.states))
			files := make([]pluginapi.HostAuthFileEntry, len(tt.states))
			for i, state := range tt.states {
				authID := string(rune('a' + i))
				states[authID] = state
				files[i] = pluginapi.HostAuthFileEntry{ID: authID, AuthIndex: "account-" + authID}
			}
			installModelStatesForTest(t, states)
			got := buildModelStatus(files)
			if got.State != tt.want || got.Message != tt.wantMessage {
				t.Fatalf("state = %q, message = %q; want %q, %q; status = %#v", got.State, got.Message, tt.want, tt.wantMessage, got)
			}
		})
	}
}

func TestBuildModelStatusZeroAuthHasExactShapeAndNonNilAuths(t *testing.T) {
	installModelStatesForTest(t, nil)
	got := buildModelStatus(nil)
	if got.State != modelNotStarted || got.Auths == nil || len(got.Auths) != 0 {
		t.Fatalf("status = %#v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"not_started","message":"模型目录尚未初始化","metadata_source":"none","metadata_fetched_at":"","auths":[]}`
	if string(raw) != want {
		t.Fatalf("status JSON = %s, want %s", raw, want)
	}
}

func TestBuildModelStatusReadsSnapshotsWithoutInitializingRuntime(t *testing.T) {
	previous := activeModelRuntime.Swap(nil)
	t.Cleanup(func() { activeModelRuntime.Swap(previous) })
	configHome := t.TempDir()
	t.Setenv("APPDATA", configHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", configHome)

	got := buildModelStatus([]pluginapi.HostAuthFileEntry{{ID: "internal-new", AuthIndex: "account-new"}})
	if activeModelRuntime.Load() != nil {
		t.Fatal("status projection initialized the model runtime")
	}
	if got.State != modelNotStarted || got.MetadataSource != modelSourceNone || len(got.Auths) != 1 || got.Auths[0].State != modelNotStarted || got.Auths[0].ModelSource != modelSourceNone {
		t.Fatalf("status = %#v", got)
	}
}

func TestDashboardIncludesModelStatus(t *testing.T) {
	t.Run("normal accounts response", func(t *testing.T) {
		installModelStatesForTest(t, nil)
		old := panelHostAuthList
		panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) { return []pluginapi.HostAuthFileEntry{}, nil }
		t.Cleanup(func() { panelHostAuthList = old })

		resp := buildDashboardEx(false, false)
		if _, exists := resp["error"]; exists {
			t.Fatalf("normal dashboard response = %#v", resp)
		}
		status, ok := resp["model_status"].(modelStatus)
		if !ok || status.State != modelNotStarted || status.Message != "模型目录尚未初始化" {
			t.Fatalf("model_status = %#v", resp["model_status"])
		}
	})

	t.Run("early refresh host auth list error", func(t *testing.T) {
		installModelStatesForTest(t, nil)
		old := panelHostAuthList
		const dashboardError = "dashboard raw error with token endpoint path body query"
		panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) { return nil, errors.New(dashboardError) }
		t.Cleanup(func() { panelHostAuthList = old })

		resp := buildDashboardEx(true, true)
		if resp["error"] != dashboardError {
			t.Fatalf("dashboard error = %#v", resp["error"])
		}
		status, ok := resp["model_status"].(modelStatus)
		if !ok {
			t.Fatalf("model_status = %#v", resp["model_status"])
		}
		if status.Message != "模型目录尚未初始化" || bytes.Contains([]byte(status.Message), []byte(dashboardError)) {
			t.Fatalf("model status message = %q", status.Message)
		}
	})
}
