package main

import (
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type authModelRequestWire struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type modelReadinessState string

const (
	modelNotStarted modelReadinessState = "not_started"
	modelLoading    modelReadinessState = "loading"
	modelReady      modelReadinessState = "ready"
	modelStale      modelReadinessState = "stale"
	modelFailed     modelReadinessState = "failed"
)

type modelSnapshotSource string

const (
	modelSourceFresh  modelSnapshotSource = "fresh"
	modelSourceCache  modelSnapshotSource = "cache"
	modelSourceConfig modelSnapshotSource = "config"
	modelSourceNone   modelSnapshotSource = "none"
)

func (s modelReadinessState) executable() bool {
	return s == modelReady || s == modelStale
}

type modelErrorCode string

const (
	modelErrorNone               modelErrorCode = ""
	modelErrorAuthInvalid        modelErrorCode = "auth_invalid"
	modelErrorWorkBuddyTransport modelErrorCode = "workbuddy_transport"
	modelErrorWorkBuddyHTTP      modelErrorCode = "workbuddy_http"
	modelErrorWorkBuddySchema    modelErrorCode = "workbuddy_schema"
	modelErrorModelsDevTransport modelErrorCode = "models_dev_transport"
	modelErrorModelsDevHTTP      modelErrorCode = "models_dev_http"
	modelErrorModelsDevSchema    modelErrorCode = "models_dev_schema"
	modelErrorCacheRead          modelErrorCode = "cache_read"
	modelErrorCacheWrite         modelErrorCode = "cache_write"
)

type modelReadinessSnapshot struct {
	State             modelReadinessState
	ModelSource       modelSnapshotSource
	MetadataSource    modelSnapshotSource
	ModelsFetchedAt   time.Time
	MetadataFetchedAt time.Time
	ErrorCode         modelErrorCode
	Models            []pluginapi.ModelInfo
	configGeneration  uint64
	authGeneration    uint64
	identitySHA256    string
}

func (s modelReadinessSnapshot) executable() bool {
	return s.State.executable()
}

type modelMetadataStatus struct {
	Source    modelSnapshotSource
	FetchedAt time.Time
	ErrorCode modelErrorCode
}

type modelCatalogSelection struct {
	cache     modelCatalogCacheV1
	source    modelSnapshotSource
	errorCode modelErrorCode
	ok        bool
}

func selectModelCatalog(
	fresh modelCatalogCacheV1,
	freshOK bool,
	cached modelCatalogCacheV1,
	cacheOK bool,
	failure modelErrorCode,
) modelCatalogSelection {
	if freshOK {
		return modelCatalogSelection{cache: fresh, source: modelSourceFresh, ok: true}
	}
	if cacheOK {
		return modelCatalogSelection{cache: cached, source: modelSourceCache, errorCode: failure, ok: true}
	}
	return modelCatalogSelection{source: modelSourceNone, errorCode: failure}
}

type metadataSelection struct {
	cache     metadataCacheV1
	source    modelSnapshotSource
	errorCode modelErrorCode
	ok        bool
}

func selectMetadata(
	fresh metadataCacheV1,
	freshOK bool,
	cached metadataCacheV1,
	cacheOK bool,
	failure modelErrorCode,
) metadataSelection {
	if freshOK {
		return metadataSelection{cache: fresh, source: modelSourceFresh, ok: true}
	}
	if cacheOK {
		return metadataSelection{cache: cached, source: modelSourceCache, errorCode: failure, ok: true}
	}
	return metadataSelection{source: modelSourceNone, errorCode: failure}
}

type modelMetadataResult = metadataSelection

type modelGenerationKey struct {
	Config         uint64
	TokenSHA256    [sha256.Size]byte
	IdentitySHA256 string
}

type modelAuthSlot struct {
	mu       sync.Mutex
	current  atomic.Pointer[modelReadinessSnapshot]
	calls    map[uint64]*modelAuthCall
	nextAuth uint64
	key      modelGenerationKey
}

type modelAuthCall struct {
	done chan struct{}
}

type modelIdentityCall struct {
	done     chan struct{}
	sequence uint64
}

type modelIdentitySlot struct {
	mu                sync.Mutex
	nextSequence      uint64
	committedSequence uint64
	committedConfig   uint64
	active            map[*modelIdentityCall]struct{}
}

type metadataCall struct {
	done      chan struct{}
	result    modelMetadataResult
	committed bool
}

type modelRuntime struct {
	store            *modelStore
	do               modelHTTPDo
	storeError       modelErrorCode
	configCommitMu   sync.RWMutex
	configGeneration atomic.Uint64
	authSlots        sync.Map
	identitySlots    sync.Map
	metadataMu       sync.Mutex
	metadataCall     *metadataCall
	metadataCache    *metadataCacheV1
	metadataResult   *modelMetadataResult
}

var activeModelRuntime atomic.Pointer[modelRuntime]

func newModelRuntime(store *modelStore, do modelHTTPDo) *modelRuntime {
	runtime := &modelRuntime{store: store, do: do}
	if store == nil {
		runtime.storeError = modelErrorCacheRead
		return runtime
	}
	cache, found, err := store.loadMetadata()
	if err != nil {
		runtime.metadataResult = &modelMetadataResult{source: modelSourceNone, errorCode: modelErrorCacheRead}
	} else if found {
		runtime.metadataCache = &cache
	}
	return runtime
}

func currentModelRuntime() *modelRuntime {
	if runtime := activeModelRuntime.Load(); runtime != nil {
		return runtime
	}
	store, err := defaultModelStore()
	var candidate *modelRuntime
	if err != nil {
		candidate = newModelRuntime(nil, hostHTTPDoWithCallback)
	} else {
		candidate = newModelRuntime(store, hostHTTPDoWithCallback)
	}
	if activeModelRuntime.CompareAndSwap(nil, candidate) {
		return candidate
	}
	return activeModelRuntime.Load()
}

func (r *modelRuntime) authSlot(authID string) *modelAuthSlot {
	candidate := &modelAuthSlot{calls: make(map[uint64]*modelAuthCall)}
	loaded, _ := r.authSlots.LoadOrStore(authID, candidate)
	return loaded.(*modelAuthSlot)
}

func (r *modelRuntime) startIdentityCall(identitySHA256 string) (*modelIdentitySlot, *modelIdentityCall) {
	candidate := &modelIdentitySlot{active: make(map[*modelIdentityCall]struct{})}
	loaded, _ := r.identitySlots.LoadOrStore(identitySHA256, candidate)
	slot := loaded.(*modelIdentitySlot)
	slot.mu.Lock()
	if slot.active == nil {
		slot.active = make(map[*modelIdentityCall]struct{})
	}
	slot.nextSequence++
	call := &modelIdentityCall{done: make(chan struct{}), sequence: slot.nextSequence}
	slot.active[call] = struct{}{}
	slot.mu.Unlock()
	return slot, call
}

func (r *modelRuntime) ensureForAuth(req authModelRequestWire) modelReadinessSnapshot {
	slot := r.authSlot(req.AuthID)
	sa, err := parseStored(req.StorageJSON)
	if err == nil {
		_, err = workBuddyRealmFromAccessToken(sa.Auth.AccessToken)
	}
	var identity modelAuthIdentity
	if err == nil {
		identity, err = modelAuthIdentityFor(req.AuthID, sa)
	}
	if err != nil {
		r.configCommitMu.RLock()
		configGeneration := r.configGeneration.Load()
		key := modelGenerationKey{Config: configGeneration}
		slot.mu.Lock()
		if slot.key != key {
			slot.key = key
			slot.nextAuth++
		}
		result := storeModelReadinessSnapshot(slot, modelReadinessSnapshot{
			State:            modelFailed,
			ModelSource:      modelSourceNone,
			MetadataSource:   modelSourceNone,
			ErrorCode:        modelErrorAuthInvalid,
			Models:           []pluginapi.ModelInfo{},
			configGeneration: configGeneration,
			authGeneration:   slot.nextAuth,
		})
		slot.mu.Unlock()
		r.configCommitMu.RUnlock()
		return result
	}

	identitySHA256 := identity.sha256()
	r.configCommitMu.RLock()
	configGeneration := r.configGeneration.Load()
	var configuredModels []string
	if features := currentFeatureRuntime(); features != nil {
		configuredModels = features.configuredModels
	}
	key := modelGenerationKey{
		Config:         configGeneration,
		TokenSHA256:    sha256.Sum256([]byte(sa.Auth.AccessToken)),
		IdentitySHA256: identitySHA256,
	}
	slot.mu.Lock()
	if slot.key != key {
		slot.key = key
		slot.nextAuth++
	}
	authGeneration := slot.nextAuth
	if current := slot.current.Load(); current != nil && current.authGeneration == authGeneration && current.State != modelLoading {
		result := cloneModelReadinessSnapshot(*current)
		slot.mu.Unlock()
		r.configCommitMu.RUnlock()
		return result
	}
	if call := slot.calls[authGeneration]; call != nil {
		slot.mu.Unlock()
		r.configCommitMu.RUnlock()
		<-call.done
		return r.snapshotForAuthID(req.AuthID)
	}
	call := &modelAuthCall{done: make(chan struct{})}
	slot.calls[authGeneration] = call
	snapshot := modelReadinessSnapshot{
		State:            modelLoading,
		ModelSource:      modelSourceNone,
		MetadataSource:   modelSourceNone,
		Models:           []pluginapi.ModelInfo{},
		configGeneration: configGeneration,
		authGeneration:   authGeneration,
		identitySHA256:   identitySHA256,
	}
	storeModelReadinessSnapshot(slot, snapshot)
	slot.mu.Unlock()
	r.configCommitMu.RUnlock()

	if r.storeError != modelErrorNone || r.store == nil {
		if len(configuredModels) > 0 {
			snapshot.ModelSource = modelSourceConfig
		}
		snapshot.State = modelFailed
		snapshot.ErrorCode = modelErrorCacheRead
		return r.finishAuthCall(slot, key, authGeneration, call, snapshot)
	}

	if len(configuredModels) > 0 {
		snapshot.ModelSource = modelSourceConfig
		metadata := r.metadataForAuth(req.HostCallbackID, slot, key, authGeneration)
		snapshot.MetadataSource = metadata.source
		if metadata.ok {
			snapshot.MetadataFetchedAt = metadata.cache.FetchedAt
		}
		snapshot.ErrorCode = metadata.errorCode
		if !metadata.ok {
			snapshot.State = modelFailed
			return r.finishAuthCall(slot, key, authGeneration, call, snapshot)
		}

		models := make([]pluginapi.ModelInfo, len(configuredModels))
		for i, id := range configuredModels {
			serving := modelFacts{ID: id}
			models[i] = modelInfoFromSources(serving, matchModelsDevRecord(id, metadata.cache.Records))
		}
		snapshot.Models = models
		if metadata.source == modelSourceFresh {
			snapshot.State = modelReady
			snapshot.ErrorCode = modelErrorNone
		} else {
			snapshot.State = modelStale
		}
		return r.finishAuthCall(slot, key, authGeneration, call, snapshot)
	}

	identitySlot, identityCall := r.startIdentityCall(identitySHA256)
	cachedModels, cachedModelsOK, modelCacheErr := r.store.loadModels(identitySHA256, identity.Realm)
	var freshModels modelCatalogCacheV1
	freshModelsOK := false
	modelFailure := modelErrorNone
	var identityWaits []<-chan struct{}
	reloadCache := false
	catalog, err := fetchWorkBuddyCatalog(sa, req.HostCallbackID, r.do)
	if err != nil {
		modelFailure = workBuddyModelErrorCode(err)
		identityWaits = r.settleIdentityCall(identitySlot, identityCall)
		reloadCache = true
	} else {
		freshModels = modelCatalogCacheV1{
			SchemaVersion:  modelCacheSchemaVersion,
			IdentitySHA256: identitySHA256,
			Realm:          catalog.Realm,
			FetchedAt:      time.Now().UTC(),
			Endpoint:       catalog.Endpoint,
			Models:         catalog.Models,
		}
		authCurrent, sharedFresh, waits, saveErr := r.saveModelsForGeneration(slot, key, authGeneration, identitySlot, identityCall, freshModels)
		if !authCurrent {
			return r.finishAuthCall(slot, key, authGeneration, call, snapshot)
		}
		identityWaits = waits
		if sharedFresh {
			committed, found, loadErr := r.store.loadModels(identitySHA256, identity.Realm)
			if loadErr != nil || !found {
				modelFailure = modelErrorCacheRead
			} else {
				freshModels = committed
				freshModelsOK = true
			}
		} else if saveErr != nil {
			modelFailure = modelErrorCacheWrite
			if modelCacheErr != nil {
				modelFailure = modelErrorCacheRead
			}
			reloadCache = true
		} else {
			freshModelsOK = true
		}
	}
	if reloadCache {
		waitForIdentityCalls(identityWaits)
		cachedModels, cachedModelsOK, modelCacheErr = r.store.loadModels(identitySHA256, identity.Realm)
		if modelCacheErr != nil {
			modelFailure = modelErrorCacheRead
		}
	}
	modelSelection := selectModelCatalog(freshModels, freshModelsOK, cachedModels, cachedModelsOK, modelFailure)
	metadata := r.metadataForAuth(req.HostCallbackID, slot, key, authGeneration)

	snapshot.ModelSource = modelSelection.source
	if modelSelection.ok {
		snapshot.ModelsFetchedAt = modelSelection.cache.FetchedAt
	}
	snapshot.MetadataSource = metadata.source
	if metadata.ok {
		snapshot.MetadataFetchedAt = metadata.cache.FetchedAt
	}
	snapshot.ErrorCode = modelSelection.errorCode
	if snapshot.ErrorCode == modelErrorNone {
		snapshot.ErrorCode = metadata.errorCode
	}
	if !modelSelection.ok || !metadata.ok {
		snapshot.State = modelFailed
		return r.finishAuthCall(slot, key, authGeneration, call, snapshot)
	}

	models := make([]pluginapi.ModelInfo, len(modelSelection.cache.Models))
	for i, model := range modelSelection.cache.Models {
		models[i] = modelInfoFromSources(model, matchModelsDevRecord(model.ID, metadata.cache.Records))
	}
	snapshot.Models = models
	if modelSelection.source == modelSourceFresh && metadata.source == modelSourceFresh {
		snapshot.State = modelReady
		snapshot.ErrorCode = modelErrorNone
	} else {
		snapshot.State = modelStale
	}
	return r.finishAuthCall(slot, key, authGeneration, call, snapshot)
}

func (r *modelRuntime) settleIdentityCall(slot *modelIdentitySlot, call *modelIdentityCall) []<-chan struct{} {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return settleIdentityCallLocked(slot, call)
}

func settleIdentityCallLocked(slot *modelIdentitySlot, call *modelIdentityCall) []<-chan struct{} {
	waits := make([]<-chan struct{}, 0, len(slot.active)-1)
	for active := range slot.active {
		if active != call {
			waits = append(waits, active.done)
		}
	}
	delete(slot.active, call)
	close(call.done)
	return waits
}

func waitForIdentityCalls(waits []<-chan struct{}) {
	for _, done := range waits {
		<-done
	}
}

func (r *modelRuntime) saveModelsForGeneration(
	slot *modelAuthSlot,
	key modelGenerationKey,
	authGeneration uint64,
	identitySlot *modelIdentitySlot,
	identityCall *modelIdentityCall,
	cache modelCatalogCacheV1,
) (bool, bool, []<-chan struct{}, error) {
	r.configCommitMu.RLock()
	defer r.configCommitMu.RUnlock()
	slot.mu.Lock()
	defer slot.mu.Unlock()
	identitySlot.mu.Lock()
	defer identitySlot.mu.Unlock()
	if !r.authGenerationCurrentLocked(slot, key, authGeneration) {
		settleIdentityCallLocked(identitySlot, identityCall)
		return false, false, nil, nil
	}
	if identitySlot.committedConfig == key.Config && identityCall.sequence < identitySlot.committedSequence {
		settleIdentityCallLocked(identitySlot, identityCall)
		return true, true, nil, nil
	}
	err := r.store.saveModels(cache)
	if err == nil {
		identitySlot.committedConfig = key.Config
		identitySlot.committedSequence = identityCall.sequence
	}
	waits := settleIdentityCallLocked(identitySlot, identityCall)
	return true, false, waits, err
}

func (r *modelRuntime) finishAuthCall(slot *modelAuthSlot, key modelGenerationKey, authGeneration uint64, call *modelAuthCall, snapshot modelReadinessSnapshot) modelReadinessSnapshot {
	r.configCommitMu.RLock()
	configGeneration := r.configGeneration.Load()
	slot.mu.Lock()
	var result modelReadinessSnapshot
	if r.authGenerationCurrentLocked(slot, key, authGeneration) {
		result = storeModelReadinessSnapshot(slot, snapshot)
	} else if current := slot.current.Load(); current != nil && current.configGeneration == configGeneration {
		result = cloneModelReadinessSnapshot(*current)
	} else {
		result = notStartedModelReadinessSnapshot(configGeneration)
	}
	close(call.done)
	if slot.calls[authGeneration] == call {
		delete(slot.calls, authGeneration)
	}
	slot.mu.Unlock()
	r.configCommitMu.RUnlock()
	return result
}

func (r *modelRuntime) authGenerationCurrentLocked(slot *modelAuthSlot, key modelGenerationKey, authGeneration uint64) bool {
	return slot.key == key && slot.nextAuth == authGeneration && r.configGeneration.Load() == key.Config
}

func (r *modelRuntime) authGenerationCurrent(slot *modelAuthSlot, key modelGenerationKey, authGeneration uint64) bool {
	r.configCommitMu.RLock()
	defer r.configCommitMu.RUnlock()
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return r.authGenerationCurrentLocked(slot, key, authGeneration)
}

func (r *modelRuntime) metadataForAuth(callbackID string, slot *modelAuthSlot, key modelGenerationKey, authGeneration uint64) modelMetadataResult {
	var call *metadataCall
	var cached metadataCacheV1
	var cachedOK bool
	var cacheReadFailed bool
	for {
		if !r.authGenerationCurrent(slot, key, authGeneration) {
			return modelMetadataResult{}
		}
		r.metadataMu.Lock()
		if r.metadataResult != nil && r.metadataResult.ok {
			result := *r.metadataResult
			r.metadataMu.Unlock()
			return result
		}
		if active := r.metadataCall; active != nil {
			r.metadataMu.Unlock()
			<-active.done
			if active.committed {
				return active.result
			}
			continue
		}

		call = &metadataCall{done: make(chan struct{})}
		r.metadataCall = call
		cachedOK = r.metadataCache != nil
		if cachedOK {
			cached = *r.metadataCache
		}
		cacheReadFailed = r.metadataResult != nil && r.metadataResult.errorCode == modelErrorCacheRead
		r.metadataMu.Unlock()
		break
	}

	var fresh metadataCacheV1
	freshOK := false
	needsSave := false
	failure := modelErrorNone
	fetched, err := fetchModelsDevMetadata(cached.ETag, callbackID, r.do)
	if err != nil {
		failure = modelsDevModelErrorCode(err)
	} else if fetched.NotModified {
		if cachedOK {
			fresh = cached
			freshOK = true
		} else {
			failure = modelErrorModelsDevSchema
		}
	} else {
		fresh = metadataCacheV1{
			SchemaVersion: modelCacheSchemaVersion,
			ETag:          fetched.ETag,
			FetchedAt:     time.Now().UTC(),
			Records:       fetched.Records,
		}
		needsSave = true
	}

	r.configCommitMu.RLock()
	slot.mu.Lock()
	if !r.authGenerationCurrentLocked(slot, key, authGeneration) {
		r.metadataMu.Lock()
		close(call.done)
		if r.metadataCall == call {
			r.metadataCall = nil
		}
		r.metadataMu.Unlock()
		slot.mu.Unlock()
		r.configCommitMu.RUnlock()
		return modelMetadataResult{}
	}
	if needsSave {
		if err := r.store.saveMetadata(fresh); err != nil {
			failure = modelErrorCacheWrite
			if cacheReadFailed {
				failure = modelErrorCacheRead
			}
		} else {
			freshOK = true
		}
	}
	selected := selectMetadata(fresh, freshOK, cached, cachedOK, failure)

	r.metadataMu.Lock()
	call.result = selected
	call.committed = true
	if selected.ok {
		settled := selected
		cache := selected.cache
		r.metadataResult = &settled
		r.metadataCache = &cache
	} else if cacheReadFailed {
		failed := modelMetadataResult{source: modelSourceNone, errorCode: modelErrorCacheRead}
		r.metadataResult = &failed
	} else {
		r.metadataResult = nil
	}
	close(call.done)
	if r.metadataCall == call {
		r.metadataCall = nil
	}
	r.metadataMu.Unlock()
	slot.mu.Unlock()
	r.configCommitMu.RUnlock()
	return selected
}

func (r *modelRuntime) snapshotForAuthID(authID string) modelReadinessSnapshot {
	slot := r.authSlot(authID)
	for {
		configGeneration := r.configGeneration.Load()
		snapshot := slot.current.Load()
		if r.configGeneration.Load() != configGeneration {
			continue
		}
		if snapshot != nil && snapshot.configGeneration == configGeneration {
			return cloneModelReadinessSnapshot(*snapshot)
		}
		return notStartedModelReadinessSnapshot(configGeneration)
	}
}

func notStartedModelReadinessSnapshot(configGeneration uint64) modelReadinessSnapshot {
	return modelReadinessSnapshot{
		State:            modelNotStarted,
		ModelSource:      modelSourceNone,
		MetadataSource:   modelSourceNone,
		Models:           []pluginapi.ModelInfo{},
		configGeneration: configGeneration,
	}
}

func (r *modelRuntime) metadataStatus() modelMetadataStatus {
	r.metadataMu.Lock()
	defer r.metadataMu.Unlock()
	if r.metadataResult != nil {
		return modelMetadataStatus{
			Source:    r.metadataResult.source,
			FetchedAt: r.metadataResult.cache.FetchedAt,
			ErrorCode: r.metadataResult.errorCode,
		}
	}
	if r.metadataCache != nil {
		return modelMetadataStatus{Source: modelSourceCache, FetchedAt: r.metadataCache.FetchedAt}
	}
	return modelMetadataStatus{Source: modelSourceNone, ErrorCode: r.storeError}
}

func (r *modelRuntime) commitFeatureRuntime(next *featureRuntimeConfig) uint64 {
	snapshot := *next
	snapshot.desensitizeTerms = append([]string(nil), next.desensitizeTerms...)
	snapshot.configuredModels = append([]string(nil), next.configuredModels...)
	r.configCommitMu.Lock()
	featureRuntime.Store(&snapshot)
	generation := r.configGeneration.Add(1)
	r.configCommitMu.Unlock()
	return generation
}

func (r *modelRuntime) advanceConfigGeneration() uint64 {
	r.configCommitMu.Lock()
	generation := r.configGeneration.Add(1)
	r.configCommitMu.Unlock()
	return generation
}

func (r *modelRuntime) markAuthNotStarted(authID string) {
	r.configCommitMu.RLock()
	configGeneration := r.configGeneration.Load()
	slot := r.authSlot(authID)
	slot.mu.Lock()
	key := modelGenerationKey{Config: configGeneration}
	if slot.key != key {
		slot.key = key
		slot.nextAuth++
	}
	storeModelReadinessSnapshot(slot, modelReadinessSnapshot{
		State:            modelNotStarted,
		ModelSource:      modelSourceNone,
		MetadataSource:   modelSourceNone,
		Models:           []pluginapi.ModelInfo{},
		configGeneration: configGeneration,
		authGeneration:   slot.nextAuth,
	})
	slot.mu.Unlock()
	r.configCommitMu.RUnlock()
}

func storeModelReadinessSnapshot(slot *modelAuthSlot, snapshot modelReadinessSnapshot) modelReadinessSnapshot {
	published := cloneModelReadinessSnapshot(snapshot)
	slot.current.Store(&published)
	return cloneModelReadinessSnapshot(published)
}

func cloneModelInfo(info pluginapi.ModelInfo) pluginapi.ModelInfo {
	info.SupportedGenerationMethods = append([]string(nil), info.SupportedGenerationMethods...)
	info.SupportedParameters = append([]string(nil), info.SupportedParameters...)
	info.SupportedInputModalities = append([]string(nil), info.SupportedInputModalities...)
	info.SupportedOutputModalities = append([]string(nil), info.SupportedOutputModalities...)
	if info.Thinking != nil {
		thinking := *info.Thinking
		thinking.Levels = append([]string(nil), info.Thinking.Levels...)
		info.Thinking = &thinking
	}
	return info
}

func cloneModelInfos(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if models == nil {
		return nil
	}
	cloned := make([]pluginapi.ModelInfo, len(models))
	for i, model := range models {
		cloned[i] = cloneModelInfo(model)
	}
	return cloned
}

func cloneModelReadinessSnapshot(snapshot modelReadinessSnapshot) modelReadinessSnapshot {
	snapshot.Models = cloneModelInfos(snapshot.Models)
	return snapshot
}

func workBuddyModelErrorCode(err error) modelErrorCode {
	return modelSourceErrorCode(err, modelErrorWorkBuddyTransport, modelErrorWorkBuddyHTTP, modelErrorWorkBuddySchema)
}

func modelsDevModelErrorCode(err error) modelErrorCode {
	return modelSourceErrorCode(err, modelErrorModelsDevTransport, modelErrorModelsDevHTTP, modelErrorModelsDevSchema)
}

func modelSourceErrorCode(err error, transport, http, schema modelErrorCode) modelErrorCode {
	var sourceErr *modelSourceError
	if !errors.As(err, &sourceErr) {
		return schema
	}
	switch sourceErr.Kind {
	case modelSourceTransportFailure:
		return transport
	case modelSourceHTTPFailure:
		return http
	default:
		return schema
	}
}
