package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	modelRuntimeRawWorkBuddyTransport = "raw-workbuddy-transport-secret"
	modelRuntimeRawWorkBuddyBody      = "raw-workbuddy-response-body-secret"
)

// isCatalogRefreshLeg reports whether req is the leading leg of one catalogue
// refresh. A refresh issues one request per client shape (desktop first, then
// CLI), so tests that count refreshes must not count both legs.
func isCatalogRefreshLeg(req *http.Request) bool {
	return req.Header.Get("User-Agent") == catalogUA
}

func installModelStatesForTest(t *testing.T, states map[string]modelReadinessState) *modelRuntime {
	t.Helper()
	runtime := newModelRuntime(newModelStore(t.TempDir()), func(*http.Request, string) (*hostHTTPResponse, error) {
		t.Fatal("unexpected model bootstrap HTTP")
		return nil, nil
	})
	generation := runtime.configGeneration.Load()
	for authID, state := range states {
		slot := runtime.authSlot(authID)
		snapshot := modelReadinessSnapshot{
			State:            state,
			ModelSource:      modelSourceFresh,
			Models:           []pluginapi.ModelInfo{},
			configGeneration: generation,
		}
		slot.current.Store(&snapshot)
	}
	old := activeModelRuntime.Swap(runtime)
	t.Cleanup(func() { activeModelRuntime.Store(old) })
	return runtime
}

// modelRuntimeLegacyUnavailable answers the enterprise-endpoint leg the way
// production does for an account without enterprise entitlement: a 401. The
// union fetch must treat that as "this leg contributed nothing" and serve the
// v3 catalogue, so tests that only care about the v3 leg use this helper
// instead of teaching every mock a second response body.
func modelRuntimeLegacyUnavailable(t *testing.T) (*hostHTTPResponse, error) {
	t.Helper()
	return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401 Authorization Required")}, nil
}

func TestModelRuntimeFreshBootstrapReady(t *testing.T) {
	store := newModelStore(t.TempDir())
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-fresh" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch {
		case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],"models":[{"id":"serve-alpha","name":"serve-alpha","maxInputTokens":32768}]}}`)}, nil
		case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/console/enterprises/personal/models":
			return modelRuntimeLegacyUnavailable(t)
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	runtime := newModelRuntime(store, do)
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:      "auth-fresh",
			StorageJSON: mustJSON(sa),
		},
		HostCallbackID: "callback-fresh",
	})
	if got.State != modelReady || got.ModelSource != modelSourceFresh {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "serve-alpha" || got.Models[0].ContextLength != 32768 {
		t.Fatalf("models = %#v", got.Models)
	}
	identity, err := modelAuthIdentityFor("auth-fresh", sa)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.loadModels(identity.sha256(), workBuddyRealmCN); err != nil || !found {
		t.Fatalf("model cache found=%v err=%v", found, err)
	}
}

func TestModelRuntimeFreshBootstrapFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name        string
		invalidAuth bool
		fault       modelRuntimeFreshFault
		wantCode    modelErrorCode
		do          func(*testing.T, string, modelRuntimeFreshFault) modelHTTPDo
	}{
		{name: "invalid auth", invalidAuth: true, wantCode: modelErrorAuthInvalid, do: modelRuntimeFreshFaultDo},
		{name: "WorkBuddy transport", fault: modelRuntimeFaultWorkBuddyTransport, wantCode: modelErrorWorkBuddyTransport, do: modelRuntimeFreshFaultDo},
		{name: "WorkBuddy HTTP", fault: modelRuntimeFaultWorkBuddyHTTP, wantCode: modelErrorWorkBuddyHTTP, do: modelRuntimeFreshFaultDo},
		{name: "WorkBuddy schema", fault: modelRuntimeFaultWorkBuddySchema, wantCode: modelErrorWorkBuddySchema, do: modelRuntimeFreshFaultDo},
		{name: "WorkBuddy save", fault: modelRuntimeFaultWorkBuddySave, wantCode: modelErrorCacheWrite, do: modelRuntimeFreshFaultDo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "model-store")
			store := newModelStore(root)
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			storageJSON := mustJSON(sa)
			if tt.invalidAuth {
				storageJSON = []byte(`{"auth":{"accessToken":""},"raw":"raw-invalid-auth-body-secret"}`)
			}
			runtime := newModelRuntime(store, tt.do(t, root, tt.fault))
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{
					AuthID:      "auth-failure",
					StorageJSON: storageJSON,
				},
				HostCallbackID: "callback-failure",
			})
			if got.State != modelFailed || got.executable() || got.Models == nil || len(got.Models) != 0 {
				t.Fatalf("snapshot = %#v", got)
			}
			if got.ErrorCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q; snapshot = %#v", got.ErrorCode, tt.wantCode, got)
			}
			assertModelRuntimeSnapshotRedacted(t, got, sa.Auth.AccessToken)
		})
	}
}

func TestModelRuntimeFreshBootstrapRetainsValidModelCache(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	identity, err := modelAuthIdentityFor("auth-cached", sa)
	if err != nil {
		t.Fatal(err)
	}
	cached := modelStoreTestCatalog(identity.sha256(), "cached")
	if err := store.saveModels(cached); err != nil {
		t.Fatal(err)
	}

	runtime := newModelRuntime(store, modelRuntimeFreshFaultDo(t, root, modelRuntimeFaultWorkBuddyTransport))
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-cached", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-failure",
	})
	if got.ModelSource != modelSourceCache || !got.ModelsFetchedAt.Equal(cached.FetchedAt) {
		t.Fatalf("valid model cache was not retained: %#v", got)
	}
	if got.ErrorCode != modelErrorWorkBuddyTransport {
		t.Fatalf("error code = %q, want %q", got.ErrorCode, modelErrorWorkBuddyTransport)
	}
}

func TestModelRuntimeFreshBootstrapModelFutureSchemaIsCacheRead(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	identity, err := modelAuthIdentityFor("auth-future-models", sa)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "models", identity.sha256()+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"schema_version":2}`)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	do := modelRuntimeFreshFaultDo(t, root, "")
	runtime := newModelRuntime(store, func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		// Count refreshes, not legs: one refresh asks each client shape, and
		// this test asserts that a future-schema cache stops the refresh from
		// happening at all.
		if isCatalogRefreshLeg(req) && req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config" {
			calls++
		}
		return do(req, callbackID)
	})

	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-future-models", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-failure",
	})
	if calls != 1 {
		t.Fatalf("WorkBuddy refresh calls = %d, want 1", calls)
	}
	if got.State != modelFailed || got.ErrorCode != modelErrorCacheRead {
		t.Fatalf("snapshot = %#v", got)
	}
	if got.ModelSource != modelSourceNone {
		t.Fatalf("future model cache source = %q, want none", got.ModelSource)
	}
	if gotFile := modelStoreReadFile(t, path); string(gotFile) != string(future) {
		t.Fatalf("future model cache was overwritten: %s", gotFile)
	}
}

func TestModelRuntimeSnapshotImmutable(t *testing.T) {
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-copy" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	}
	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	published := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-copy", StorageJSON: mustJSON(syntheticStoredAuth(t, workBuddyRealmCN))},
		HostCallbackID:   "callback-copy",
	})
	if published.State != modelReady || len(published.Models) != 1 {
		t.Fatalf("published snapshot = %#v", published)
	}

	first := runtime.snapshotForAuthID("auth-copy")
	first.Models[0].ID = "changed"
	first.Models[0].SupportedGenerationMethods[0] = "changed"
	first.Models[0].SupportedParameters = []string{"changed"}
	first.Models[0].SupportedInputModalities = []string{"changed"}
	first.Models[0].SupportedOutputModalities = []string{"changed"}
	first.Models[0].Thinking = &pluginapi.ThinkingSupport{Levels: []string{"changed"}}

	second := runtime.snapshotForAuthID("auth-copy")
	model := second.Models[0]
	if model.ID != "serve-alpha" || model.SupportedGenerationMethods[0] != "chat" || model.SupportedParameters != nil || model.SupportedInputModalities != nil || model.SupportedOutputModalities != nil || model.Thinking != nil {
		t.Fatalf("published snapshot was mutated: %#v", second)
	}

	nested := modelReadinessSnapshot{Models: []pluginapi.ModelInfo{{
		SupportedGenerationMethods: []string{"chat"},
		SupportedParameters:        []string{"temperature"},
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
		Thinking:                   &pluginapi.ThinkingSupport{Levels: []string{"low"}},
	}}}
	runtime.authSlot("auth-nested").current.Store(&nested)
	nestedResult := runtime.snapshotForAuthID("auth-nested")
	nestedResult.Models[0].SupportedGenerationMethods[0] = "changed"
	nestedResult.Models[0].SupportedParameters[0] = "changed"
	nestedResult.Models[0].SupportedInputModalities[0] = "changed"
	nestedResult.Models[0].SupportedOutputModalities[0] = "changed"
	nestedResult.Models[0].Thinking.Levels[0] = "changed"
	unchanged := runtime.snapshotForAuthID("auth-nested").Models[0]
	if unchanged.SupportedGenerationMethods[0] != "chat" || unchanged.SupportedParameters[0] != "temperature" || unchanged.SupportedInputModalities[0] != "text" || unchanged.SupportedOutputModalities[0] != "text" || unchanged.Thinking.Levels[0] != "low" {
		t.Fatalf("nested model info was not deeply cloned: %#v", unchanged)
	}

	if got := runtime.advanceConfigGeneration(); got != 1 {
		t.Fatalf("config generation = %d, want 1", got)
	}
	runtime.markAuthNotStarted("auth-copy")
	marked := runtime.snapshotForAuthID("auth-copy")
	if marked.State != modelNotStarted || marked.executable() || marked.Models == nil || len(marked.Models) != 0 {
		t.Fatalf("marked snapshot = %#v", marked)
	}

	t.Run("host alias and exclusion config stay response-local", func(t *testing.T) {
		root := t.TempDir()
		store := newModelStore(root)
		do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
			if callbackID != "callback-config" {
				t.Fatalf("callback ID = %q", callbackID)
			}
			switch req.URL.Host {
			case "copilot.tencent.com":
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
			default:
				t.Fatalf("unexpected request %s", req.URL)
				return nil, nil
			}
		}
		sa := syntheticStoredAuth(t, workBuddyRealmCN)
		runtime := newModelRuntime(store, do)
		got := runtime.ensureForAuth(authModelRequestWire{
			AuthModelRequest: pluginapi.AuthModelRequest{
				AuthID:      "auth-config",
				StorageJSON: mustJSON(sa),
				Host: pluginapi.HostConfigSummary{
					OAuthModelAlias: map[string][]pluginapi.ModelAlias{providerName: {{Name: "serve-alpha", Alias: "secret-alias"}}},
					ExcludedModels:  map[string][]string{providerName: {"serve-alpha", "secret-excluded"}},
				},
			},
			HostCallbackID: "callback-config",
		})
		if got.State != modelReady || len(got.Models) != 1 || got.Models[0].ID != "serve-alpha" {
			t.Fatalf("snapshot = %#v", got)
		}
		identity, err := modelAuthIdentityFor("auth-config", sa)
		if err != nil {
			t.Fatal(err)
		}
		cacheRaw, err := os.ReadFile(filepath.Join(root, "models", identity.sha256()+".json"))
		if err != nil {
			t.Fatal(err)
		}
		snapshotRaw, err := json.Marshal(runtime.snapshotForAuthID("auth-config"))
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range [][]byte{cacheRaw, snapshotRaw} {
			for _, forbidden := range []string{"secret-alias", "secret-excluded"} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatalf("cached/shared model state contains host config %q: %s", forbidden, raw)
				}
			}
		}
	})
}

func TestModelRuntimeSameAuthSingleflight(t *testing.T) {
	var workBuddyCalls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			if req.URL.Path == "/console/enterprises/personal/models" {
				// The ssoother leg of the union; contributes nothing here.
				return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
			}
			// Count refreshes, not legs: one refresh asks each client shape, and this
			// test asserts single-flight collapses concurrent refreshes into one.
			if isCatalogRefreshLeg(req) && workBuddyCalls.Add(1) == 1 {
				close(started)
			}
			<-release
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	}
	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	req := authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-one", StorageJSON: mustJSON(syntheticStoredAuth(t, workBuddyRealmCN))}}
	results := make(chan modelReadinessSnapshot, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- runtime.ensureForAuth(req)
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	for result := range results {
		if result.State != modelReady {
			t.Fatalf("result = %#v", result)
		}
	}
	if workBuddyCalls.Load() != 1 {
		t.Fatalf("calls: WorkBuddy=%d, want 1", workBuddyCalls.Load())
	}
}

func TestModelRuntimeDifferentAuthIsolation(t *testing.T) {
	var cnCalls atomic.Int32
	var globalCalls atomic.Int32
	cnStarted := make(chan struct{})
	globalStarted := make(chan struct{})
	cnRelease := make(chan struct{})
	globalRelease := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		// Count refreshes, not legs: one refresh asks each client shape, and this
		// test asserts that two different auths refresh concurrently and
		// independently.
		if req.URL.Path == "/console/enterprises/personal/models" {
			return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			if isCatalogRefreshLeg(req) && cnCalls.Add(1) == 1 {
				close(cnStarted)
			}
			<-cnRelease
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["cn-model"]}]}}`)}, nil
		case "www.workbuddy.ai":
			if isCatalogRefreshLeg(req) && globalCalls.Add(1) == 1 {
				close(globalStarted)
			}
			<-globalRelease
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["global-model"]}]}}`)}, nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	cnAuth := syntheticStoredAuth(t, workBuddyRealmCN)
	cnAuth.Account.UID = "uid-cn"
	cnAuth.Account.EnterpriseID = "enterprise-cn"
	globalAuth := syntheticStoredAuth(t, workBuddyRealmGlobal)
	globalAuth.Account.UID = "uid-global"
	globalAuth.Account.EnterpriseID = "enterprise-global"
	cnResult := make(chan modelReadinessSnapshot, 1)
	globalResult := make(chan modelReadinessSnapshot, 1)
	go func() {
		cnResult <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-cn", StorageJSON: mustJSON(cnAuth)}})
	}()
	go func() {
		globalResult <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-global", StorageJSON: mustJSON(globalAuth)}})
	}()

	bothStarted := make(chan struct{})
	go func() {
		<-cnStarted
		<-globalStarted
		close(bothStarted)
	}()
	concurrent := true
	select {
	case <-bothStarted:
	case <-time.After(2 * time.Second):
		concurrent = false
	}
	close(cnRelease)
	close(globalRelease)
	gotCN := <-cnResult
	gotGlobal := <-globalResult
	if !concurrent {
		t.Fatal("different auth WorkBuddy requests were serialized")
	}
	if cnCalls.Load() != 1 || globalCalls.Load() != 1 {
		t.Fatalf("WorkBuddy calls: cn=%d global=%d", cnCalls.Load(), globalCalls.Load())
	}
	if gotCN.State != modelReady || len(gotCN.Models) != 1 || gotCN.Models[0].ID != "cn-model" {
		t.Fatalf("CN snapshot = %#v", gotCN)
	}
	if gotGlobal.State != modelReady || len(gotGlobal.Models) != 1 || gotGlobal.Models[0].ID != "global-model" {
		t.Fatalf("Global snapshot = %#v", gotGlobal)
	}
}

func TestModelRuntimeConcurrentReaders(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		// The union fetch issues a v3 request and an enterprise-endpoint
		// request; only the first must signal, and neither may be released
		// before the readers have started.
		if req.URL.Path == "/console/enterprises/personal/models" {
			return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			startedOnce.Do(func() { close(started) })
			<-release
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	bootstrapDone := make(chan modelReadinessSnapshot, 1)
	go func() {
		bootstrapDone <- runtime.ensureForAuth(authModelRequestWire{
			AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-readers", StorageJSON: mustJSON(syntheticStoredAuth(t, workBuddyRealmCN))},
		})
	}()
	<-started

	const readerCount = 32
	var readersStarted atomic.Int32
	var invalidState atomic.Bool
	allReadersStarted := make(chan struct{})
	stopReaders := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < readerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			for {
				snapshot := runtime.snapshotForAuthID("auth-readers")
				if snapshot.State != modelLoading && snapshot.State != modelReady {
					invalidState.Store(true)
				}
				if first {
					first = false
					if readersStarted.Add(1) == readerCount {
						close(allReadersStarted)
					}
				}
				select {
				case <-stopReaders:
					return
				default:
				}
			}
		}()
	}
	lockFree := true
	select {
	case <-allReadersStarted:
	case <-time.After(2 * time.Second):
		lockFree = false
	}
	close(release)
	result := <-bootstrapDone
	close(stopReaders)
	wg.Wait()
	if !lockFree {
		t.Fatal("snapshot readers blocked on bootstrap")
	}
	if invalidState.Load() {
		t.Fatal("snapshot reader observed an invalid state")
	}
	if result.State != modelReady {
		t.Fatalf("bootstrap result = %#v", result)
	}
}

func TestModelRuntimeOldGenerationCannotCommit(t *testing.T) {
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var oldStartedOnce sync.Once
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		// The union fetch issues two requests per refresh, so the "old" signal
		// fires once even though both legs carry the same token.
		if req.URL.Path == "/console/enterprises/personal/models" {
			return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
		}
		token := req.Header.Get("Authorization")
		if strings.HasSuffix(token, "signature-a") {
			oldStartedOnce.Do(func() { close(oldStarted) })
			<-releaseOld
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		}
		return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-beta"]}]}}`)}, nil
	}
	root := t.TempDir()
	store := newModelStore(root)
	runtime := newModelRuntime(store, do)
	saOld := syntheticStoredAuth(t, workBuddyRealmCN)
	parts := strings.Split(saOld.Auth.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("synthetic token has %d parts", len(parts))
	}
	saOld.Auth.AccessToken = parts[0] + "." + parts[1] + ".signature-a"
	saNew := *saOld
	saNew.Auth.AccessToken = parts[0] + "." + parts[1] + ".signature-b"
	identity, err := modelAuthIdentityFor("auth-race", &saNew)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveModels(modelStoreTestCatalog(identity.sha256(), "pre-race")); err != nil {
		t.Fatal(err)
	}

	oldDone := make(chan modelReadinessSnapshot, 1)
	go func() {
		oldDone <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-race", StorageJSON: mustJSON(saOld)}})
	}()
	<-oldStarted
	newResult := runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-race", StorageJSON: mustJSON(&saNew)}})
	if newResult.State != modelReady || len(newResult.Models) != 1 || newResult.Models[0].ID != "serve-beta" {
		t.Fatalf("new result = %#v", newResult)
	}
	modelPath := filepath.Join(root, "models", identity.sha256()+".json")
	backupBeforeOldFinished := modelStoreReadFile(t, modelPath+".bak")
	close(releaseOld)
	<-oldDone

	current := runtime.snapshotForAuthID("auth-race")
	if current.State != modelReady || current.ErrorCode != modelErrorNone || len(current.Models) != 1 || current.Models[0].ID != "serve-beta" {
		t.Errorf("old generation overwrote current = %#v", current)
	}
	cached, found, err := store.loadModels(identity.sha256(), workBuddyRealmCN)
	if err != nil || !found || len(cached.Models) != 1 || cached.Models[0].ID != "serve-beta" {
		t.Errorf("cache=%#v found=%v err=%v", cached, found, err)
	}
	if backupAfterOldFinished := modelStoreReadFile(t, modelPath+".bak"); string(backupAfterOldFinished) != string(backupBeforeOldFinished) {
		t.Errorf("old generation replaced backup: before=%s after=%s", backupBeforeOldFinished, backupAfterOldFinished)
	}

	t.Run("late failure does not publish an error", func(t *testing.T) {
		oldStarted := make(chan struct{})
		releaseOld := make(chan struct{})
		var oldStartedOnce sync.Once
		do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
			if req.URL.Path == "/console/enterprises/personal/models" {
				return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
			}
			if strings.HasSuffix(req.Header.Get("Authorization"), "signature-a") {
				oldStartedOnce.Do(func() { close(oldStarted) })
				<-releaseOld
				return nil, errors.New(modelRuntimeRawWorkBuddyTransport)
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-beta"]}]}}`)}, nil
		}
		runtime := newModelRuntime(newModelStore(t.TempDir()), do)
		oldDone := make(chan modelReadinessSnapshot, 1)
		go func() {
			oldDone <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-race-error", StorageJSON: mustJSON(saOld)}})
		}()
		<-oldStarted
		newResult := runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-race-error", StorageJSON: mustJSON(&saNew)}})
		if newResult.State != modelReady || len(newResult.Models) != 1 || newResult.Models[0].ID != "serve-beta" {
			t.Fatalf("new result = %#v", newResult)
		}
		close(releaseOld)
		<-oldDone
		current := runtime.snapshotForAuthID("auth-race-error")
		if current.State != modelReady || current.ErrorCode != modelErrorNone || len(current.Models) != 1 || current.Models[0].ID != "serve-beta" {
			t.Fatalf("old generation published its failure = %#v", current)
		}
	})
}

func TestModelRuntimeSharedIdentityRejectsLateOlderCatalogCommit(t *testing.T) {
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var oldStartedOnce sync.Once
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if req.URL.Path == "/console/enterprises/personal/models" {
			return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
		}
		if strings.HasSuffix(req.Header.Get("Authorization"), "signature-a") {
			oldStartedOnce.Do(func() { close(oldStarted) })
			<-releaseOld
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		}
		return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-beta"]}]}}`)}, nil
	}

	root := t.TempDir()
	store := newModelStore(root)
	runtime := newModelRuntime(store, do)
	saOld := syntheticStoredAuth(t, workBuddyRealmCN)
	parts := strings.Split(saOld.Auth.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("synthetic token has %d parts", len(parts))
	}
	saOld.Auth.AccessToken = parts[0] + "." + parts[1] + ".signature-a"
	saNew := *saOld
	saNew.Auth.AccessToken = parts[0] + "." + parts[1] + ".signature-b"
	oldIdentity, err := modelAuthIdentityFor("auth-legacy", saOld)
	if err != nil {
		t.Fatal(err)
	}
	newIdentity, err := modelAuthIdentityFor("auth-canonical", &saNew)
	if err != nil {
		t.Fatal(err)
	}
	if oldIdentity.sha256() != newIdentity.sha256() {
		t.Fatalf("shared identity hashes differ: %q != %q", oldIdentity.sha256(), newIdentity.sha256())
	}
	identitySHA256 := newIdentity.sha256()
	backup := modelStoreTestCatalog(identitySHA256, "before-race-backup")
	backup.Models = []modelFacts{{ID: "serve-before-backup"}}
	primary := modelStoreTestCatalog(identitySHA256, "before-race-primary")
	primary.FetchedAt = backup.FetchedAt.Add(time.Minute)
	primary.Models = []modelFacts{{ID: "serve-before-primary"}}
	if err := store.saveModels(backup); err != nil {
		t.Fatal(err)
	}
	if err := store.saveModels(primary); err != nil {
		t.Fatal(err)
	}

	oldDone := make(chan modelReadinessSnapshot, 1)
	go func() {
		oldDone <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-legacy", StorageJSON: mustJSON(saOld)}})
	}()
	<-oldStarted
	newResult := runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-canonical", StorageJSON: mustJSON(&saNew)}})
	if newResult.State != modelReady || len(newResult.Models) != 1 || newResult.Models[0].ID != "serve-beta" {
		t.Fatalf("new identity generation = %#v", newResult)
	}
	modelPath := filepath.Join(root, "models", identitySHA256+".json")
	primaryAfterNew := modelStoreReadFile(t, modelPath)
	backupAfterNew := modelStoreReadFile(t, modelPath+".bak")

	close(releaseOld)
	oldResult := <-oldDone
	if got := modelStoreReadFile(t, modelPath); !bytes.Equal(got, primaryAfterNew) {
		t.Fatalf("late shared-identity generation replaced primary: before=%s after=%s", primaryAfterNew, got)
	}
	if got := modelStoreReadFile(t, modelPath+".bak"); !bytes.Equal(got, backupAfterNew) {
		t.Fatalf("late shared-identity generation replaced backup: before=%s after=%s", backupAfterNew, got)
	}
	if !oldResult.executable() || len(oldResult.Models) != 1 || oldResult.Models[0].ID != "serve-beta" {
		t.Fatalf("older auth did not adopt the committed shared catalog = %#v", oldResult)
	}
}

func TestModelRuntimeConcurrentSharedIdentityBootstrapsRemainExecutable(t *testing.T) {
	const authCount = 8
	started := make(chan struct{}, authCount)
	release := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			// One refresh asks each client shape; signal once per refresh so the
			// barrier below still counts refreshes.
			if isCatalogRefreshLeg(req) {
				started <- struct{}{}
				<-release
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	results := make(chan modelReadinessSnapshot, authCount)
	for i := 0; i < authCount; i++ {
		go func(index int) {
			results <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{
				AuthID:      fmt.Sprintf("auth-shared-%d", index),
				StorageJSON: mustJSON(sa),
			}})
		}(i)
	}
	for i := 0; i < authCount; i++ {
		<-started
	}
	close(release)
	for i := 0; i < authCount; i++ {
		result := <-results
		if !result.executable() || len(result.Models) != 1 || result.Models[0].ID != "serve-alpha" {
			t.Fatalf("shared-identity bootstrap became sticky-failed = %#v", result)
		}
	}
}

func TestModelRuntimeSharedIdentityFailureDoesNotDiscardConcurrentSuccess(t *testing.T) {
	successStarted := make(chan struct{})
	failureReturned := make(chan struct{})
	releaseSuccess := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		// The enterprise leg answers 401 for these synthetic accounts, so it
		// contributes nothing and the v3 leg alone decides the outcome.
		if req.URL.Path == "/console/enterprises/personal/models" {
			return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
		}
		if strings.HasSuffix(req.Header.Get("Authorization"), "signature-good") {
			if isCatalogRefreshLeg(req) {
				close(successStarted)
				<-releaseSuccess
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		}
		if isCatalogRefreshLeg(req) {
			close(failureReturned)
		}
		return nil, errors.New(modelRuntimeRawWorkBuddyTransport)
	}

	root := t.TempDir()
	store := newModelStore(root)
	runtime := newModelRuntime(store, do)
	saGood := syntheticStoredAuth(t, workBuddyRealmCN)
	parts := strings.Split(saGood.Auth.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("synthetic token has %d parts", len(parts))
	}
	saGood.Auth.AccessToken = parts[0] + "." + parts[1] + ".signature-good"
	saBad := *saGood
	saBad.Auth.AccessToken = parts[0] + "." + parts[1] + ".signature-bad"
	identity, err := modelAuthIdentityFor("auth-good", saGood)
	if err != nil {
		t.Fatal(err)
	}

	goodDone := make(chan modelReadinessSnapshot, 1)
	badDone := make(chan modelReadinessSnapshot, 1)
	go func() {
		goodDone <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID: "auth-good", StorageJSON: mustJSON(saGood),
		}})
	}()
	<-successStarted
	go func() {
		badDone <- runtime.ensureForAuth(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID: "auth-bad", StorageJSON: mustJSON(&saBad),
		}})
	}()
	<-failureReturned
	close(releaseSuccess)

	goodResult := <-goodDone
	if goodResult.State != modelReady || goodResult.ModelSource != modelSourceFresh || len(goodResult.Models) != 1 || goodResult.Models[0].ID != "serve-alpha" {
		t.Errorf("successful shared-identity bootstrap = %#v, want fresh ready catalog", goodResult)
	}
	badResult := <-badDone
	if badResult.State != modelStale || badResult.ModelSource != modelSourceCache || badResult.ErrorCode != modelErrorWorkBuddyTransport || len(badResult.Models) != 1 || badResult.Models[0].ID != "serve-alpha" {
		t.Errorf("failed peer bootstrap = %#v, want stale shared catalog with transport error", badResult)
	}
	cached, found, err := store.loadModels(identity.sha256(), workBuddyRealmCN)
	if err != nil || !found || len(cached.Models) != 1 || cached.Models[0].ID != "serve-alpha" {
		t.Fatalf("shared cache = %#v, found=%v, err=%v", cached, found, err)
	}
}

func TestModelRuntimeConfigGenerationInvalidatesSnapshot(t *testing.T) {
	t.Run("in-flight catalog save", func(t *testing.T) {
		var workBuddyCalls atomic.Int32
		oldStarted := make(chan struct{})
		releaseOld := make(chan struct{})
		do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
			// Count refreshes, not legs: a refresh asks each client shape, and
			// this test tracks refreshes.
			switch req.URL.Host {
			case "copilot.tencent.com":
				if req.URL.Path == "/console/enterprises/personal/models" {
					return &hostHTTPResponse{StatusCode: http.StatusUnauthorized, Headers: make(http.Header), Body: []byte("401")}, nil
				}
				// Both legs of a round must answer alike: the first round
				// blocks so the test can advance the config generation while
				// the refresh is in flight.
				if isCatalogRefreshLeg(req) {
					workBuddyCalls.Add(1)
					if workBuddyCalls.Load() == 1 {
						close(oldStarted)
					}
				}
				if workBuddyCalls.Load() == 1 {
					<-releaseOld
					return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
				}
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-beta"]}]}}`)}, nil
			default:
				t.Fatalf("unexpected request %s", req.URL)
				return nil, nil
			}
		}
		store := newModelStore(t.TempDir())
		runtime := newModelRuntime(store, do)
		sa := syntheticStoredAuth(t, workBuddyRealmCN)
		identity, err := modelAuthIdentityFor("auth-config-catalog", sa)
		if err != nil {
			t.Fatal(err)
		}
		req := authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-config-catalog", StorageJSON: mustJSON(sa)}}
		oldDone := make(chan modelReadinessSnapshot, 1)
		go func() { oldDone <- runtime.ensureForAuth(req) }()
		<-oldStarted
		if got := runtime.advanceConfigGeneration(); got != 1 {
			t.Fatalf("config generation = %d, want 1", got)
		}
		invalidated := runtime.snapshotForAuthID("auth-config-catalog")
		if invalidated.State != modelNotStarted || invalidated.executable() || invalidated.Models == nil || len(invalidated.Models) != 0 || invalidated.configGeneration != 1 {
			t.Errorf("invalidated snapshot = %#v", invalidated)
		}
		close(releaseOld)
		<-oldDone
		if _, found, err := store.loadModels(identity.sha256(), workBuddyRealmCN); err != nil || found {
			t.Fatalf("stale generation cache found=%v err=%v", found, err)
		}
		if current := runtime.snapshotForAuthID("auth-config-catalog"); current.State != modelNotStarted || current.executable() {
			t.Fatalf("stale generation published = %#v", current)
		}

		newResult := runtime.ensureForAuth(req)
		if workBuddyCalls.Load() != 2 || newResult.State != modelReady || len(newResult.Models) != 1 || newResult.Models[0].ID != "serve-beta" {
			t.Fatalf("calls=%d new result=%#v", workBuddyCalls.Load(), newResult)
		}
	})

}

func TestModelRuntimeFailedConfigureKeepsGeneration(t *testing.T) {
	runtime := newModelRuntime(newModelStore(t.TempDir()), func(*http.Request, string) (*hostHTTPResponse, error) {
		t.Fatal("configure performed model HTTP")
		return nil, nil
	})
	previousRuntime := activeModelRuntime.Swap(runtime)
	oldProxy := proxyState.Load()
	oldFeatures := featureRuntime.Load()
	usageReportMu.RLock()
	oldUsageURL, oldUsageKey := usageReportURL, usageReportKey
	usageReportMu.RUnlock()
	t.Cleanup(func() {
		activeModelRuntime.Swap(previousRuntime)
		proxyState.Store(oldProxy)
		featureRuntime.Store(oldFeatures)
		usageReportMu.Lock()
		usageReportURL, usageReportKey = oldUsageURL, oldUsageKey
		usageReportMu.Unlock()
	})

	failures := []struct {
		name string
		raw  []byte
	}{
		{name: "request parse", raw: []byte(`{`)},
		{name: "proxy parse", raw: mustJSON(map[string]any{"config_yaml": []byte("proxy-url: [not-a-string]\n")})},
		{name: "feature parse", raw: mustJSON(map[string]any{"config_yaml": []byte("desensitize_terms: [x]\n")})},
		{name: "proxy configure", raw: mustJSON(map[string]any{"config_yaml": []byte("proxy-url: direct\n")})},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			before := runtime.configGeneration.Load()
			if err := configure(tt.raw); err == nil {
				t.Fatal("invalid configure succeeded")
			}
			if got := runtime.configGeneration.Load(); got != before {
				t.Fatalf("config generation = %d, want %d", got, before)
			}
		})
	}

	if err := configure(mustJSON(map[string]any{"config_yaml": []byte("usage_report_url: http://127.0.0.1:1\n")})); err != nil {
		t.Fatal(err)
	}
	if got := runtime.configGeneration.Load(); got != 1 {
		t.Fatalf("successful configure advanced generation to %d, want 1", got)
	}
}

func TestModelRuntimeFreshBootstrapCurrentRuntimeIsLazySingleton(t *testing.T) {
	previous := activeModelRuntime.Swap(nil)
	t.Cleanup(func() { activeModelRuntime.Swap(previous) })
	configHome := t.TempDir()
	t.Setenv("APPDATA", configHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", configHome)

	first := currentModelRuntime()
	second := currentModelRuntime()
	if first == nil || second != first || activeModelRuntime.Load() != first {
		t.Fatalf("runtime singleton: first=%p second=%p active=%p", first, second, activeModelRuntime.Load())
	}
}

// The stale matrix has one dimension left: whether the catalogue refresh
// succeeded. models.dev used to be a second dimension, which is why a host
// without egress to it saw a failed provider for a data point that changes no
// routing decision.
func TestModelRuntimeStaleMatrix(t *testing.T) {
	tests := []struct {
		name                string
		workBuddyFails      bool
		wantState           modelReadinessState
		wantModelSource     modelSnapshotSource
		wantID              string
		wantName            string
		wantContext         int64
		wantCode            modelErrorCode
		wantCachedModelTime bool
	}{
		{
			name:            "fresh catalogue",
			wantState:       modelReady,
			wantModelSource: modelSourceFresh,
			wantID:          "fresh-model",
			wantName:        "fresh-model",
			wantContext:     2222,
		},
		{
			name:                "cached catalogue after a failed refresh",
			workBuddyFails:      true,
			wantState:           modelStale,
			wantModelSource:     modelSourceCache,
			wantID:              "cached-model",
			wantName:            "cached-model",
			wantCode:            modelErrorWorkBuddyTransport,
			wantCachedModelTime: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			lastGood := modelRuntimeSeedLastGood(t, root, "auth-stale", sa)
			workBuddyCalls := 0
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				if callbackID != "callback-stale" {
					t.Fatalf("callback ID = %q", callbackID)
				}
				switch {
				case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
					// Count refreshes, not legs: a refresh asks each client shape.
					if isCatalogRefreshLeg(req) {
						workBuddyCalls++
					}
					if tt.workBuddyFails {
						return nil, errors.New(modelRuntimeRawWorkBuddyTransport)
					}
					return modelRuntimeFreshWorkBuddyResponse(), nil
				case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/console/enterprises/personal/models":
					return modelRuntimeLegacyUnavailable(t)
				default:
					t.Fatalf("unexpected model request %s", req.URL)
					return nil, nil
				}
			}

			runtime := newModelRuntime(newModelStore(root), do)
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-stale", StorageJSON: mustJSON(sa)},
				HostCallbackID:   "callback-stale",
			})
			if workBuddyCalls != 1 {
				t.Fatalf("refresh calls: WorkBuddy=%d, want 1", workBuddyCalls)
			}
			if got.State != tt.wantState || got.ModelSource != tt.wantModelSource {
				t.Fatalf("snapshot = %#v", got)
			}
			if !got.executable() {
				t.Fatalf("state %q did not allow execution", got.State)
			}
			if got.ErrorCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q", got.ErrorCode, tt.wantCode)
			}
			if len(got.Models) != 1 || got.Models[0].ID != tt.wantID || got.Models[0].Name != tt.wantName || got.Models[0].ContextLength != tt.wantContext {
				t.Fatalf("models = %#v", got.Models)
			}
			if got.ModelsFetchedAt.Equal(lastGood.catalog.FetchedAt) != tt.wantCachedModelTime {
				t.Fatalf("models fetched_at = %s, cached = %s", got.ModelsFetchedAt, lastGood.catalog.FetchedAt)
			}
		})
	}
}

// A failed cache write must keep the previous primary and backup intact and
// still serve the last good catalogue rather than the un-persisted refresh.
func TestModelRuntimeStalePersistenceFailuresRetainOldPrimary(t *testing.T) {
	root := t.TempDir()
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	lastGood := modelRuntimeSeedLastGood(t, root, "auth-save", sa)
	blockedPath := lastGood.modelPath
	futureBackup := []byte(`{"schema_version":2}`)
	if err := os.WriteFile(blockedPath+".bak", futureBackup, 0o600); err != nil {
		t.Fatal(err)
	}
	// The catalogue stays readable but its directory cannot be written to, so
	// the refresh succeeds and only the persist fails.
	modelRuntimeMakeModelsDirectoryReadOnly(t, root)

	runtime := newModelRuntime(newModelStore(root), modelRuntimeSuccessfulRefreshDo(t, "callback-save"))
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-save", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-save",
	})
	if got.State != modelStale || !got.executable() || got.ErrorCode != modelErrorCacheWrite {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "cached-model" || got.Models[0].Name != "cached-model" {
		t.Fatalf("models = %#v", got.Models)
	}
	if after := modelStoreReadFile(t, blockedPath+".bak"); string(after) != string(futureBackup) {
		t.Fatalf("backup was replaced after failed save: %s", after)
	}
}

// A refresh that parses only partly must not replace the last good catalogue:
// serving half a model list is worse than serving a stale one.
func TestModelRuntimeStaleRejectsPartialAndCorruptRefreshes(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		wantModelSource modelSnapshotSource
		wantID          string
		wantName        string
		wantContext     int64
		wantCode        modelErrorCode
	}{
		{
			name:            "partial WorkBuddy body",
			body:            `{"code":0,"data":{"agents":[{"name":"cli","models":["fresh-model",""]}]}}`,
			wantModelSource: modelSourceCache,
			wantID:          "cached-model",
			wantName:        "cached-model",
			wantCode:        modelErrorWorkBuddySchema,
		},
		{
			name:            "corrupt WorkBuddy body",
			body:            `{"code":`,
			wantModelSource: modelSourceCache,
			wantID:          "cached-model",
			wantName:        "cached-model",
			wantCode:        modelErrorWorkBuddySchema,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			lastGood := modelRuntimeSeedLastGood(t, root, "auth-invalid-refresh", sa)
			failedPath := lastGood.modelPath
			before := modelStoreReadFile(t, failedPath)
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				if callbackID != "callback-invalid-refresh" {
					t.Fatalf("callback ID = %q", callbackID)
				}
				if req.URL.Host != "copilot.tencent.com" {
					t.Fatalf("unexpected model request %s", req.URL)
				}
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(tt.body)}, nil
			}

			runtime := newModelRuntime(newModelStore(root), do)
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-invalid-refresh", StorageJSON: mustJSON(sa)},
				HostCallbackID:   "callback-invalid-refresh",
			})
			if got.State != modelStale || !got.executable() || got.ModelSource != tt.wantModelSource || got.ErrorCode != tt.wantCode {
				t.Fatalf("snapshot = %#v", got)
			}
			if len(got.Models) != 1 || got.Models[0].ID != tt.wantID || got.Models[0].Name != tt.wantName || got.Models[0].ContextLength != tt.wantContext {
				t.Fatalf("models = %#v", got.Models)
			}
			if after := modelStoreReadFile(t, failedPath); string(after) != string(before) {
				t.Fatalf("last-good primary was replaced: before=%s after=%s", before, after)
			}
		})
	}
}

type modelRuntimeLastGood struct {
	catalog   modelCatalogCacheV1
	modelPath string
}

// modelRuntimeSeedLastGood writes a valid on-disk catalogue cache, the state a
// plugin install is in after one successful refresh. Only the catalogue is
// cached: the plugin no longer keeps a models.dev record cache.
func modelRuntimeSeedLastGood(t *testing.T, root, authID string, sa *storedAuth) modelRuntimeLastGood {
	t.Helper()
	identity, err := modelAuthIdentityFor(authID, sa)
	if err != nil {
		t.Fatal(err)
	}
	catalog := modelCatalogCacheV1{
		SchemaVersion:  1,
		IdentitySHA256: identity.sha256(),
		Realm:          workBuddyRealmCN,
		FetchedAt:      time.Date(2026, time.August, 28, 1, 2, 3, 0, time.UTC),
		Endpoint:       workBuddyEndpointV3Config,
		Models:         []modelFacts{{ID: "cached-model"}},
	}
	store := newModelStore(root)
	if err := store.saveModels(catalog); err != nil {
		t.Fatal(err)
	}
	return modelRuntimeLastGood{
		catalog:   catalog,
		modelPath: filepath.Join(root, "models", identity.sha256()+".json"),
	}
}

// The catalogue carries the limits itself; before the models.dev removal this
// fixture only listed ids and leaned on the third-party record for context
// length, which is exactly the dependency that was removed.
func modelRuntimeFreshWorkBuddyResponse() *hostHTTPResponse {
	return &hostHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    make(http.Header),
		Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["fresh-model"]}],` +
			`"models":[{"id":"fresh-model","name":"fresh-model","maxInputTokens":2222,"maxOutputTokens":32000}]}}`),
	}
}

// The seeded cached catalogue likewise carries its own limits.

func modelRuntimeSuccessfulRefreshDo(t *testing.T, callbackID string) modelHTTPDo {
	t.Helper()
	return func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
		if gotCallbackID != callbackID {
			t.Fatalf("callback ID = %q, want %q", gotCallbackID, callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}
}

type modelRuntimeFreshFault string

const (
	modelRuntimeFaultWorkBuddyTransport modelRuntimeFreshFault = "workbuddy_transport"
	modelRuntimeFaultWorkBuddyHTTP      modelRuntimeFreshFault = "workbuddy_http"
	modelRuntimeFaultWorkBuddySchema    modelRuntimeFreshFault = "workbuddy_schema"
	modelRuntimeFaultWorkBuddySave      modelRuntimeFreshFault = "workbuddy_save"
)

func modelRuntimeFreshFaultDo(t *testing.T, root string, fault modelRuntimeFreshFault) modelHTTPDo {
	t.Helper()
	return func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-failure" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch {
		case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
			switch fault {
			case modelRuntimeFaultWorkBuddyTransport:
				return nil, errors.New(modelRuntimeRawWorkBuddyTransport)
			case modelRuntimeFaultWorkBuddyHTTP:
				return &hostHTTPResponse{StatusCode: http.StatusServiceUnavailable, Headers: make(http.Header), Body: []byte(modelRuntimeRawWorkBuddyBody)}, nil
			case modelRuntimeFaultWorkBuddySchema:
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(modelRuntimeRawWorkBuddyBody)}, nil
			case modelRuntimeFaultWorkBuddySave:
				modelRuntimeMakeModelsDirectoryReadOnly(t, root)
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/console/enterprises/personal/models":
			return modelRuntimeLegacyUnavailable(t)
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}
}

func modelRuntimeMakeModelsDirectoryReadOnly(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		modelRuntimeReplaceStoreRootWithFile(t, root)
		return
	}
	dir := filepath.Join(root, "models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	probe, err := os.CreateTemp(dir, ".permission-probe-*")
	if err == nil {
		probePath := probe.Name()
		if closeErr := probe.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if removeErr := os.Remove(probePath); removeErr != nil {
			t.Fatal(removeErr)
		}
		t.Skip("current process can write to a chmod 0500 directory")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write probe failed with %v, want permission denied", err)
	}
}

func modelRuntimeReplaceStoreRootWithFile(t *testing.T, root string) {
	t.Helper()
	if err := os.Rename(root, root+"-saved"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("regular-file-store-root"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertModelRuntimeSnapshotRedacted(t *testing.T, got modelReadinessSnapshot, accessToken string) {
	t.Helper()
	rendered := fmt.Sprintf("%#v", got)
	for _, forbidden := range []string{
		accessToken,
		modelRuntimeRawWorkBuddyTransport,
		modelRuntimeRawWorkBuddyBody,
		"raw-invalid-auth-body-secret",
		"https://",
		"copilot.tencent.com",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("snapshot contains raw detail %q: %s", forbidden, rendered)
		}
	}
}
