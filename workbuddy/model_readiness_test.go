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
	modelRuntimeRawMetadataTransport  = "raw-metadata-transport-secret"
	modelRuntimeRawMetadataBody       = "raw-metadata-response-body-secret"
)

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
			MetadataSource:   modelSourceFresh,
			Models:           []pluginapi.ModelInfo{},
			configGeneration: generation,
		}
		slot.current.Store(&snapshot)
	}
	old := activeModelRuntime.Swap(runtime)
	t.Cleanup(func() { activeModelRuntime.Store(old) })
	return runtime
}

func TestModelRuntimeFreshBootstrapReady(t *testing.T) {
	store := newModelStore(t.TempDir())
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-fresh" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch {
		case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case req.URL.Host == "models.dev" && req.URL.Path == "/models.json":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"ETag": []string{`"fresh-etag"`}}, Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","name":"Alpha","limit":{"context":32768,"output":4096}}}`)}, nil
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
	if got.State != modelReady || got.ModelSource != modelSourceFresh || got.MetadataSource != modelSourceFresh {
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
	if _, found, err := store.loadMetadata(); err != nil || !found {
		t.Fatalf("metadata cache found=%v err=%v", found, err)
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
		{name: "models.dev transport", fault: modelRuntimeFaultMetadataTransport, wantCode: modelErrorModelsDevTransport, do: modelRuntimeFreshFaultDo},
		{name: "models.dev HTTP", fault: modelRuntimeFaultMetadataHTTP, wantCode: modelErrorModelsDevHTTP, do: modelRuntimeFreshFaultDo},
		{name: "models.dev schema", fault: modelRuntimeFaultMetadataSchema, wantCode: modelErrorModelsDevSchema, do: modelRuntimeFreshFaultDo},
		{name: "metadata save", fault: modelRuntimeFaultMetadataSave, wantCode: modelErrorCacheWrite, do: modelRuntimeFreshFaultDo},
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
	if err := store.saveMetadata(modelStoreTestMetadata("cached")); err != nil {
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
		if req.URL.Host == "copilot.tencent.com" {
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

func TestModelRuntimeFreshBootstrapMetadataFutureSchemaIsCacheRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metadata.json")
	future := []byte(`{"schema_version":2}`)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	do := modelRuntimeFreshFaultDo(t, root, "")
	runtime := newModelRuntime(newModelStore(root), func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if req.URL.Host == "models.dev" {
			calls++
		}
		return do(req, callbackID)
	})

	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-future-metadata", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-failure",
	})
	if calls != 1 {
		t.Fatalf("models.dev refresh calls = %d, want 1", calls)
	}
	if got.State != modelFailed || got.ErrorCode != modelErrorCacheRead {
		t.Fatalf("snapshot = %#v", got)
	}
	if got.MetadataSource != modelSourceNone {
		t.Fatalf("future metadata cache source = %q, want none", got.MetadataSource)
	}
	if gotFile := modelStoreReadFile(t, path); string(gotFile) != string(future) {
		t.Fatalf("future metadata cache was overwritten: %s", gotFile)
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
		case "models.dev":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","modalities":{"input":["text"],"output":["text"]}}}`)}, nil
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
	first.Models[0].SupportedInputModalities[0] = "changed"
	first.Models[0].SupportedOutputModalities[0] = "changed"
	first.Models[0].Thinking = &pluginapi.ThinkingSupport{Levels: []string{"changed"}}

	second := runtime.snapshotForAuthID("auth-copy")
	model := second.Models[0]
	if model.ID != "serve-alpha" || model.SupportedGenerationMethods[0] != "chat" || model.SupportedParameters != nil || model.SupportedInputModalities[0] != "text" || model.SupportedOutputModalities[0] != "text" || model.Thinking != nil {
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
			case "models.dev":
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"}}`)}, nil
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
	var metadataCalls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			if workBuddyCalls.Add(1) == 1 {
				close(started)
			}
			<-release
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case "models.dev":
			metadataCalls.Add(1)
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"}}`)}, nil
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
	if workBuddyCalls.Load() != 1 || metadataCalls.Load() != 1 {
		t.Fatalf("calls: workbuddy=%d metadata=%d", workBuddyCalls.Load(), metadataCalls.Load())
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
		switch req.URL.Host {
		case "copilot.tencent.com":
			if cnCalls.Add(1) == 1 {
				close(cnStarted)
			}
			<-cnRelease
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["cn-model"]}]}}`)}, nil
		case "www.workbuddy.ai":
			if globalCalls.Add(1) == 1 {
				close(globalStarted)
			}
			<-globalRelease
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["global-model"]}]}}`)}, nil
		case "models.dev":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/cn-model":{"id":"cn-model"},"vendor/global-model":{"id":"global-model"}}`)}, nil
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

func TestModelRuntimeMetadataSingleflight(t *testing.T) {
	var workBuddyCalls atomic.Int32
	var metadataCalls atomic.Int32
	workBuddyStarted := make(chan struct{})
	releaseWorkBuddy := make(chan struct{})
	metadataStarted := make(chan struct{})
	secondMetadataStarted := make(chan struct{})
	releaseMetadata := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			if workBuddyCalls.Add(1) == 2 {
				close(workBuddyStarted)
			}
			<-releaseWorkBuddy
			id := "model-one"
			if req.Header.Get("X-User-Id") == "uid-two" {
				id = "model-two"
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(fmt.Sprintf(`{"code":0,"data":{"agents":[{"name":"cli","models":[%q]}]}}`, id))}, nil
		case "models.dev":
			call := metadataCalls.Add(1)
			if call == 1 {
				close(metadataStarted)
			} else if call == 2 {
				close(secondMetadataStarted)
			}
			<-releaseMetadata
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/model-one":{"id":"model-one"},"vendor/model-two":{"id":"model-two"}}`)}, nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	firstAuth := syntheticStoredAuth(t, workBuddyRealmCN)
	firstAuth.Account.UID = "uid-one"
	firstAuth.Account.EnterpriseID = "enterprise-one"
	secondAuth := syntheticStoredAuth(t, workBuddyRealmCN)
	secondAuth.Account.UID = "uid-two"
	secondAuth.Account.EnterpriseID = "enterprise-two"
	start := make(chan struct{})
	results := make(chan modelReadinessSnapshot, 2)
	var wg sync.WaitGroup
	for _, req := range []authModelRequestWire{
		{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-one", StorageJSON: mustJSON(firstAuth)}},
		{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-two", StorageJSON: mustJSON(secondAuth)}},
	} {
		req := req
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- runtime.ensureForAuth(req)
		}()
	}
	close(start)
	concurrentAuths := true
	select {
	case <-workBuddyStarted:
	case <-time.After(2 * time.Second):
		concurrentAuths = false
	}
	close(releaseWorkBuddy)
	<-metadataStarted
	select {
	case <-secondMetadataStarted:
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseMetadata)
	wg.Wait()
	close(results)
	if !concurrentAuths {
		t.Fatal("auth bootstraps were serialized before metadata refresh")
	}
	for result := range results {
		if result.State != modelReady {
			t.Fatalf("result = %#v", result)
		}
	}
	if metadataCalls.Load() != 1 {
		t.Fatalf("models.dev calls = %d, want 1", metadataCalls.Load())
	}
}

func TestModelRuntimeConcurrentReaders(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			close(started)
			<-release
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case "models.dev":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"}}`)}, nil
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
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if req.URL.Host == "models.dev" {
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"},"vendor/serve-beta":{"id":"serve-beta"}}`)}, nil
		}
		token := req.Header.Get("Authorization")
		if strings.HasSuffix(token, "signature-a") {
			close(oldStarted)
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
		do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
			if req.URL.Host == "models.dev" {
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-beta":{"id":"serve-beta"}}`)}, nil
			}
			if strings.HasSuffix(req.Header.Get("Authorization"), "signature-a") {
				close(oldStarted)
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
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if req.URL.Host == "models.dev" {
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"},"vendor/serve-beta":{"id":"serve-beta"}}`)}, nil
		}
		if strings.HasSuffix(req.Header.Get("Authorization"), "signature-a") {
			close(oldStarted)
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
			started <- struct{}{}
			<-release
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case "models.dev":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"}}`)}, nil
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
		if req.URL.Host == "models.dev" {
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"}}`)}, nil
		}
		if strings.HasSuffix(req.Header.Get("Authorization"), "signature-good") {
			close(successStarted)
			<-releaseSuccess
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		}
		close(failureReturned)
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
			switch req.URL.Host {
			case "copilot.tencent.com":
				if workBuddyCalls.Add(1) == 1 {
					close(oldStarted)
					<-releaseOld
					return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
				}
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-beta"]}]}}`)}, nil
			case "models.dev":
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"},"vendor/serve-beta":{"id":"serve-beta"}}`)}, nil
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

	t.Run("in-flight metadata save", func(t *testing.T) {
		root := t.TempDir()
		store := newModelStore(root)
		backup := modelStoreTestMetadata("before-config-backup")
		primary := modelStoreTestMetadata("before-config-primary")
		primary.FetchedAt = backup.FetchedAt.Add(time.Minute)
		if err := store.saveMetadata(backup); err != nil {
			t.Fatal(err)
		}
		if err := store.saveMetadata(primary); err != nil {
			t.Fatal(err)
		}
		metadataPath := filepath.Join(root, "metadata.json")
		primaryBefore := modelStoreReadFile(t, metadataPath)
		backupBefore := modelStoreReadFile(t, metadataPath+".bak")

		var workBuddyCalls atomic.Int32
		var metadataCalls atomic.Int32
		metadataStarted := make(chan struct{})
		releaseMetadata := make(chan struct{})
		do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
			switch req.URL.Host {
			case "copilot.tencent.com":
				workBuddyCalls.Add(1)
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
			case "models.dev":
				if metadataCalls.Add(1) == 1 {
					close(metadataStarted)
					<-releaseMetadata
				}
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha"}}`)}, nil
			default:
				t.Fatalf("unexpected request %s", req.URL)
				return nil, nil
			}
		}
		runtime := newModelRuntime(store, do)
		sa := syntheticStoredAuth(t, workBuddyRealmCN)
		req := authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-config-metadata", StorageJSON: mustJSON(sa)}}
		oldDone := make(chan modelReadinessSnapshot, 1)
		go func() { oldDone <- runtime.ensureForAuth(req) }()
		<-metadataStarted
		if got := runtime.advanceConfigGeneration(); got != 1 {
			t.Fatalf("config generation = %d, want 1", got)
		}
		invalidated := runtime.snapshotForAuthID("auth-config-metadata")
		if invalidated.State != modelNotStarted || invalidated.executable() || invalidated.Models == nil || len(invalidated.Models) != 0 {
			t.Errorf("invalidated snapshot = %#v", invalidated)
		}
		close(releaseMetadata)
		<-oldDone
		if got := modelStoreReadFile(t, metadataPath); string(got) != string(primaryBefore) {
			t.Fatalf("stale generation replaced metadata primary: before=%s after=%s", primaryBefore, got)
		}
		if got := modelStoreReadFile(t, metadataPath+".bak"); string(got) != string(backupBefore) {
			t.Fatalf("stale generation replaced metadata backup: before=%s after=%s", backupBefore, got)
		}
		if runtime.metadataResult != nil {
			t.Fatalf("stale generation settled metadata = %#v", runtime.metadataResult)
		}

		newResult := runtime.ensureForAuth(req)
		if workBuddyCalls.Load() != 2 || metadataCalls.Load() != 2 || newResult.State != modelReady || len(newResult.Models) != 1 || newResult.Models[0].ID != "serve-alpha" {
			t.Fatalf("workbuddy=%d metadata=%d new result=%#v", workBuddyCalls.Load(), metadataCalls.Load(), newResult)
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

func TestModelRuntimeFreshBootstrapPreloadsMetadataWithoutSettlingRefresh(t *testing.T) {
	store := newModelStore(t.TempDir())
	cached := modelStoreTestMetadata("cached")
	if err := store.saveMetadata(cached); err != nil {
		t.Fatal(err)
	}
	runtime := newModelRuntime(store, func(*http.Request, string) (*hostHTTPResponse, error) {
		t.Fatal("metadata preload made an HTTP request")
		return nil, nil
	})
	status := runtime.metadataStatus()
	if status.Source != modelSourceCache || !status.FetchedAt.Equal(cached.FetchedAt) || status.ErrorCode != modelErrorNone {
		t.Fatalf("metadata status = %#v", status)
	}
	if runtime.metadataResult != nil {
		t.Fatalf("metadata preload settled refresh: %#v", runtime.metadataResult)
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

func TestModelRuntimeStaleMatrix(t *testing.T) {
	tests := []struct {
		name                   string
		workBuddyFails         bool
		metadataFails          bool
		metadataNotModified    bool
		wantState              modelReadinessState
		wantModelSource        modelSnapshotSource
		wantMetadataSource     modelSnapshotSource
		wantID                 string
		wantName               string
		wantContext            int64
		wantCode               modelErrorCode
		wantCachedModelTime    bool
		wantCachedMetadataTime bool
	}{
		{
			name:               "fresh models and fresh metadata",
			wantState:          modelReady,
			wantModelSource:    modelSourceFresh,
			wantMetadataSource: modelSourceFresh,
			wantID:             "fresh-model",
			wantName:           "Fresh metadata for fresh",
			wantContext:        2222,
		},
		{
			name:                "cached models and fresh metadata",
			workBuddyFails:      true,
			wantState:           modelStale,
			wantModelSource:     modelSourceCache,
			wantMetadataSource:  modelSourceFresh,
			wantID:              "cached-model",
			wantName:            "Fresh metadata for cached",
			wantContext:         2222,
			wantCode:            modelErrorWorkBuddyTransport,
			wantCachedModelTime: true,
		},
		{
			name:                   "fresh models and cached metadata",
			metadataFails:          true,
			wantState:              modelStale,
			wantModelSource:        modelSourceFresh,
			wantMetadataSource:     modelSourceCache,
			wantID:                 "fresh-model",
			wantName:               "Cached metadata for fresh",
			wantContext:            1111,
			wantCode:               modelErrorModelsDevTransport,
			wantCachedMetadataTime: true,
		},
		{
			name:                   "cached models and cached metadata",
			workBuddyFails:         true,
			metadataFails:          true,
			wantState:              modelStale,
			wantModelSource:        modelSourceCache,
			wantMetadataSource:     modelSourceCache,
			wantID:                 "cached-model",
			wantName:               "Cached metadata for cached",
			wantContext:            1111,
			wantCode:               modelErrorWorkBuddyTransport,
			wantCachedModelTime:    true,
			wantCachedMetadataTime: true,
		},
		{
			name:                   "fresh models and not modified metadata",
			metadataNotModified:    true,
			wantState:              modelReady,
			wantModelSource:        modelSourceFresh,
			wantMetadataSource:     modelSourceFresh,
			wantID:                 "fresh-model",
			wantName:               "Cached metadata for fresh",
			wantContext:            1111,
			wantCachedMetadataTime: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			lastGood := modelRuntimeSeedLastGood(t, root, "auth-stale", sa)
			workBuddyCalls := 0
			metadataCalls := 0
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				if callbackID != "callback-stale" {
					t.Fatalf("callback ID = %q", callbackID)
				}
				switch {
				case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
					workBuddyCalls++
					if tt.workBuddyFails {
						return nil, errors.New(modelRuntimeRawWorkBuddyTransport)
					}
					return modelRuntimeFreshWorkBuddyResponse(), nil
				case req.URL.Host == "models.dev" && req.URL.Path == "/models.json":
					metadataCalls++
					if got := req.Header.Get("If-None-Match"); got != lastGood.metadata.ETag {
						t.Fatalf("If-None-Match = %q, want %q", got, lastGood.metadata.ETag)
					}
					if tt.metadataFails {
						return nil, errors.New(modelRuntimeRawMetadataTransport)
					}
					if tt.metadataNotModified {
						return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: http.Header{"ETag": []string{`"ignored-etag"`}}}, nil
					}
					return modelRuntimeFreshMetadataResponse(), nil
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
			if workBuddyCalls != 1 || metadataCalls != 1 {
				t.Fatalf("refresh calls: workbuddy=%d metadata=%d, want 1 each", workBuddyCalls, metadataCalls)
			}
			if got.State != tt.wantState || got.ModelSource != tt.wantModelSource || got.MetadataSource != tt.wantMetadataSource {
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
			if got.MetadataFetchedAt.Equal(lastGood.metadata.FetchedAt) != tt.wantCachedMetadataTime {
				t.Fatalf("metadata fetched_at = %s, cached = %s", got.MetadataFetchedAt, lastGood.metadata.FetchedAt)
			}
		})
	}
}

func TestModelRuntimeStalePersistenceFailuresRetainOldPrimary(t *testing.T) {
	tests := []struct {
		name               string
		blockedSource      string
		wantModelSource    modelSnapshotSource
		wantMetadataSource modelSnapshotSource
		wantID             string
		wantName           string
		wantContext        int64
	}{
		{
			name:               "model catalog save",
			blockedSource:      "models",
			wantModelSource:    modelSourceCache,
			wantMetadataSource: modelSourceFresh,
			wantID:             "cached-model",
			wantName:           "Fresh metadata for cached",
			wantContext:        2222,
		},
		{
			name:               "metadata save",
			blockedSource:      "metadata",
			wantModelSource:    modelSourceFresh,
			wantMetadataSource: modelSourceCache,
			wantID:             "fresh-model",
			wantName:           "Cached metadata for fresh",
			wantContext:        1111,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			lastGood := modelRuntimeSeedLastGood(t, root, "auth-save", sa)
			blockedPath := lastGood.modelPath
			if tt.blockedSource == "metadata" {
				blockedPath = lastGood.metadataPath
			}
			before := modelStoreReadFile(t, blockedPath)
			futureBackup := []byte(`{"schema_version":2}`)
			if err := os.WriteFile(blockedPath+".bak", futureBackup, 0o600); err != nil {
				t.Fatal(err)
			}

			runtime := newModelRuntime(newModelStore(root), modelRuntimeSuccessfulRefreshDo(t, "callback-save"))
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-save", StorageJSON: mustJSON(sa)},
				HostCallbackID:   "callback-save",
			})
			if got.State != modelStale || !got.executable() || got.ModelSource != tt.wantModelSource || got.MetadataSource != tt.wantMetadataSource || got.ErrorCode != modelErrorCacheWrite {
				t.Fatalf("snapshot = %#v", got)
			}
			if len(got.Models) != 1 || got.Models[0].ID != tt.wantID || got.Models[0].Name != tt.wantName || got.Models[0].ContextLength != tt.wantContext {
				t.Fatalf("models = %#v", got.Models)
			}
			if after := modelStoreReadFile(t, blockedPath); string(after) != string(before) {
				t.Fatalf("old primary was replaced after failed save: before=%s after=%s", before, after)
			}
			if after := modelStoreReadFile(t, blockedPath+".bak"); string(after) != string(futureBackup) {
				t.Fatalf("future backup was replaced after failed save: %s", after)
			}
		})
	}
}

func TestModelRuntimeStaleRejectsPartialAndCorruptRefreshes(t *testing.T) {
	tests := []struct {
		name               string
		failedSource       string
		body               string
		wantModelSource    modelSnapshotSource
		wantMetadataSource modelSnapshotSource
		wantID             string
		wantName           string
		wantContext        int64
		wantCode           modelErrorCode
	}{
		{
			name:               "partial WorkBuddy body",
			failedSource:       "models",
			body:               `{"code":0,"data":{"agents":[{"name":"cli","models":["fresh-model",""]}]}}`,
			wantModelSource:    modelSourceCache,
			wantMetadataSource: modelSourceFresh,
			wantID:             "cached-model",
			wantName:           "Fresh metadata for cached",
			wantContext:        2222,
			wantCode:           modelErrorWorkBuddySchema,
		},
		{
			name:               "corrupt WorkBuddy body",
			failedSource:       "models",
			body:               `{"code":`,
			wantModelSource:    modelSourceCache,
			wantMetadataSource: modelSourceFresh,
			wantID:             "cached-model",
			wantName:           "Fresh metadata for cached",
			wantContext:        2222,
			wantCode:           modelErrorWorkBuddySchema,
		},
		{
			name:               "partial metadata body",
			failedSource:       "metadata",
			body:               `{"fresh-provider/fresh-model":{"id":"fresh-model","name":"Fresh metadata for fresh","limit":{"context":2222}},"fresh-provider/broken":{"id":""}}`,
			wantModelSource:    modelSourceFresh,
			wantMetadataSource: modelSourceCache,
			wantID:             "fresh-model",
			wantName:           "Cached metadata for fresh",
			wantContext:        1111,
			wantCode:           modelErrorModelsDevSchema,
		},
		{
			name:               "corrupt metadata body",
			failedSource:       "metadata",
			body:               `{"fresh-provider/fresh-model":`,
			wantModelSource:    modelSourceFresh,
			wantMetadataSource: modelSourceCache,
			wantID:             "fresh-model",
			wantName:           "Cached metadata for fresh",
			wantContext:        1111,
			wantCode:           modelErrorModelsDevSchema,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			lastGood := modelRuntimeSeedLastGood(t, root, "auth-invalid-refresh", sa)
			failedPath := lastGood.modelPath
			if tt.failedSource == "metadata" {
				failedPath = lastGood.metadataPath
			}
			before := modelStoreReadFile(t, failedPath)
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				if callbackID != "callback-invalid-refresh" {
					t.Fatalf("callback ID = %q", callbackID)
				}
				switch req.URL.Host {
				case "copilot.tencent.com":
					if tt.failedSource == "models" {
						return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(tt.body)}, nil
					}
					return modelRuntimeFreshWorkBuddyResponse(), nil
				case "models.dev":
					if tt.failedSource == "metadata" {
						return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(tt.body)}, nil
					}
					return modelRuntimeFreshMetadataResponse(), nil
				default:
					t.Fatalf("unexpected model request %s", req.URL)
					return nil, nil
				}
			}

			runtime := newModelRuntime(newModelStore(root), do)
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-invalid-refresh", StorageJSON: mustJSON(sa)},
				HostCallbackID:   "callback-invalid-refresh",
			})
			if got.State != modelStale || !got.executable() || got.ModelSource != tt.wantModelSource || got.MetadataSource != tt.wantMetadataSource || got.ErrorCode != tt.wantCode {
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

func TestModelRuntimeNotModifiedKeepsMetadataCacheWithoutRewrite(t *testing.T) {
	root := t.TempDir()
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	lastGood := modelRuntimeSeedLastGood(t, root, "auth-not-modified", sa)
	before := modelStoreReadFile(t, lastGood.metadataPath)
	metadataCalls := 0
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-not-modified" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		case "models.dev":
			metadataCalls++
			if got := req.Header.Get("If-None-Match"); got != lastGood.metadata.ETag {
				t.Fatalf("If-None-Match = %q, want %q", got, lastGood.metadata.ETag)
			}
			return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: http.Header{"ETag": []string{`"replacement-etag"`}}}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(root), do)
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-not-modified", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-not-modified",
	})
	if metadataCalls != 1 {
		t.Fatalf("metadata calls = %d, want 1", metadataCalls)
	}
	if got.State != modelReady || got.ModelSource != modelSourceFresh || got.MetadataSource != modelSourceFresh || got.ErrorCode != modelErrorNone {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "fresh-model" || got.Models[0].Name != "Cached metadata for fresh" || got.Models[0].ContextLength != 1111 {
		t.Fatalf("models = %#v", got.Models)
	}
	if !got.MetadataFetchedAt.Equal(lastGood.metadata.FetchedAt) {
		t.Fatalf("metadata fetched_at = %s, want %s", got.MetadataFetchedAt, lastGood.metadata.FetchedAt)
	}
	if runtime.metadataResult == nil || runtime.metadataResult.cache.ETag != lastGood.metadata.ETag || !runtime.metadataResult.cache.FetchedAt.Equal(lastGood.metadata.FetchedAt) {
		t.Fatalf("metadata result = %#v", runtime.metadataResult)
	}
	if after := modelStoreReadFile(t, lastGood.metadataPath); string(after) != string(before) {
		t.Fatalf("metadata primary was rewritten: before=%s after=%s", before, after)
	}
	if _, err := os.Stat(lastGood.metadataPath + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata backup exists after 304: %v", err)
	}
}

func TestModelRuntimeNotModifiedWithoutMetadataCacheFailsAndRetries(t *testing.T) {
	metadataCalls := 0
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-not-modified-no-cache" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		case "models.dev":
			metadataCalls++
			if metadataCalls == 1 {
				return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: make(http.Header)}, nil
			}
			return modelRuntimeFreshMetadataResponse(), nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	first := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-not-modified-first", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-not-modified-no-cache",
	})
	if first.State != modelFailed || first.ModelSource != modelSourceFresh || first.MetadataSource != modelSourceNone || first.ErrorCode != modelErrorModelsDevSchema || first.executable() || first.Models == nil || len(first.Models) != 0 {
		t.Fatalf("first snapshot = %#v", first)
	}
	if runtime.metadataResult != nil {
		t.Fatalf("304 without cache settled the runtime: %#v", runtime.metadataResult)
	}

	second := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-not-modified-second", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-not-modified-no-cache",
	})
	if metadataCalls != 2 {
		t.Fatalf("metadata calls = %d, want 2", metadataCalls)
	}
	if second.State != modelReady || second.ModelSource != modelSourceFresh || second.MetadataSource != modelSourceFresh || !second.executable() {
		t.Fatalf("second snapshot = %#v", second)
	}
}

func TestModelRuntimeRetriesMetadataWithoutCache(t *testing.T) {
	metadataCalls := 0
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-retry" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		case "models.dev":
			metadataCalls++
			if metadataCalls == 1 {
				return nil, errors.New(modelRuntimeRawMetadataTransport)
			}
			return modelRuntimeFreshMetadataResponse(), nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	first := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-retry-first", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-retry",
	})
	if first.State != modelFailed || first.MetadataSource != modelSourceNone || first.ErrorCode != modelErrorModelsDevTransport || first.executable() {
		t.Fatalf("first snapshot = %#v", first)
	}
	if runtime.metadataResult != nil {
		t.Fatalf("metadata failure without cache settled the runtime: %#v", runtime.metadataResult)
	}

	second := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-retry-second", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-retry",
	})
	if metadataCalls != 2 {
		t.Fatalf("metadata calls = %d, want 2", metadataCalls)
	}
	if second.State != modelReady || second.ModelSource != modelSourceFresh || second.MetadataSource != modelSourceFresh || !second.executable() {
		t.Fatalf("second snapshot = %#v", second)
	}
	if len(second.Models) != 1 || second.Models[0].ID != "fresh-model" || second.Models[0].ContextLength != 2222 {
		t.Fatalf("second models = %#v", second.Models)
	}
}

type modelRuntimeLastGood struct {
	catalog      modelCatalogCacheV1
	metadata     metadataCacheV1
	modelPath    string
	metadataPath string
}

func modelRuntimeSeedLastGood(t *testing.T, root, authID string, sa *storedAuth) modelRuntimeLastGood {
	t.Helper()
	identity, err := modelAuthIdentityFor(authID, sa)
	if err != nil {
		t.Fatal(err)
	}
	cachedContext := int64(1111)
	catalog := modelCatalogCacheV1{
		SchemaVersion:  1,
		IdentitySHA256: identity.sha256(),
		Realm:          workBuddyRealmCN,
		FetchedAt:      time.Date(2026, time.August, 28, 1, 2, 3, 0, time.UTC),
		Endpoint:       workBuddyEndpointV3Config,
		Models:         []modelFacts{{ID: "cached-model"}},
	}
	metadata := metadataCacheV1{
		SchemaVersion: 1,
		ETag:          `W/"cached-etag"`,
		FetchedAt:     time.Date(2026, time.August, 28, 4, 5, 6, 0, time.UTC),
		Records: map[string]modelFacts{
			"cached-provider/cached-model": {
				ID:            "cached-provider/cached-model",
				Name:          "Cached metadata for cached",
				ContextLength: &cachedContext,
			},
			"cached-provider/fresh-model": {
				ID:            "cached-provider/fresh-model",
				Name:          "Cached metadata for fresh",
				ContextLength: &cachedContext,
			},
		},
	}
	store := newModelStore(root)
	if err := store.saveModels(catalog); err != nil {
		t.Fatal(err)
	}
	if err := store.saveMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	return modelRuntimeLastGood{
		catalog:      catalog,
		metadata:     metadata,
		modelPath:    filepath.Join(root, "models", identity.sha256()+".json"),
		metadataPath: filepath.Join(root, "metadata.json"),
	}
}

func modelRuntimeFreshWorkBuddyResponse() *hostHTTPResponse {
	return &hostHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    make(http.Header),
		Body:       []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["fresh-model"]}]}}`),
	}
}

func modelRuntimeFreshMetadataResponse() *hostHTTPResponse {
	return &hostHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"ETag": []string{`"fresh-etag"`}},
		Body: []byte(`{"fresh-provider/cached-model":{"id":"cached-model","name":"Fresh metadata for cached","limit":{"context":2222}},` +
			`"fresh-provider/fresh-model":{"id":"fresh-model","name":"Fresh metadata for fresh","limit":{"context":2222}}}`),
	}
}

func modelRuntimeSuccessfulRefreshDo(t *testing.T, callbackID string) modelHTTPDo {
	t.Helper()
	return func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
		if gotCallbackID != callbackID {
			t.Fatalf("callback ID = %q, want %q", gotCallbackID, callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		case "models.dev":
			return modelRuntimeFreshMetadataResponse(), nil
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
	modelRuntimeFaultMetadataTransport  modelRuntimeFreshFault = "metadata_transport"
	modelRuntimeFaultMetadataHTTP       modelRuntimeFreshFault = "metadata_http"
	modelRuntimeFaultMetadataSchema     modelRuntimeFreshFault = "metadata_schema"
	modelRuntimeFaultMetadataSave       modelRuntimeFreshFault = "metadata_save"
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
		case req.URL.Host == "models.dev" && req.URL.Path == "/models.json":
			switch fault {
			case modelRuntimeFaultMetadataTransport:
				return nil, errors.New(modelRuntimeRawMetadataTransport)
			case modelRuntimeFaultMetadataHTTP:
				return &hostHTTPResponse{StatusCode: http.StatusBadGateway, Headers: make(http.Header), Body: []byte(modelRuntimeRawMetadataBody)}, nil
			case modelRuntimeFaultMetadataSchema:
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(modelRuntimeRawMetadataBody)}, nil
			case modelRuntimeFaultMetadataSave:
				modelRuntimeReplaceStoreRootWithFile(t, root)
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"ETag": []string{`"failure-etag"`}}, Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","name":"Alpha"}}`)}, nil
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
		modelRuntimeRawMetadataTransport,
		modelRuntimeRawMetadataBody,
		"raw-invalid-auth-body-secret",
		"https://",
		"copilot.tencent.com",
		"models.dev/models.json",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("snapshot contains raw detail %q: %s", forbidden, rendered)
		}
	}
}
