// models.go implements the ModelProvider capability: model lists (static +
// per-auth dynamic), alias reverse resolution, the host's
// oauth-excluded-models filter, and the catalog metadata inference requests
// depend on (model_config, thinking levels, context window).
//
// The catalog is the authority for per-model facts. Requests must be built
// from it rather than from hardcoded values: price factor, context windows and
// thinking levels all drift upstream.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// catalogTTL bounds how long one realm's catalog is reused. model.static /
// model.for_auth are re-invoked by the host on every reload and models query;
// without caching each of those fans out to one upstream call per account.
const catalogTTL = 5 * time.Minute

// defaultMaxOutputTokens is the completion ceiling advertised to clients. The
// catalog carries no output limit; 32k matches the body template's max_tokens.
const defaultMaxOutputTokens = 32768

// catalogEntry is one model as upstream describes it.
type catalogEntry struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Format         string  `json:"format"`
	Source         string  `json:"source"`
	Enable         bool    `json:"enable"`
	IsVL           bool    `json:"is_vl"`
	IsReasoning    bool    `json:"is_reasoning"`
	IsDefault      bool    `json:"is_default"`
	IsFree         bool    `json:"is_free"`
	MaxInputTokens int64   `json:"max_input_tokens"`
	PriceFactor    float64 `json:"price_factor"`

	ContextConfig map[string]struct {
		TokenCount int64 `json:"token_count"`
		IsDefault  bool  `json:"is_default"`
	} `json:"context_config"`

	ThinkingConfig *thinkingConfig `json:"thinking_config"`

	// Disabled marks a model upstream lists but does not offer to this
	// account. It is kept in the catalog (so "why is X missing" has an
	// answer) but never advertised to clients.
	Disabled bool `json:"-"`
}

// thinkingConfig mirrors the catalog's thinking_config block.
type thinkingConfig struct {
	Enabled  *thinkingEnabled `json:"enabled"`
	Disabled *struct{}        `json:"disabled"`
}

type thinkingEnabled struct {
	Efforts map[string]thinkingEffort `json:"efforts"`
}

type thinkingEffort struct {
	IsDefault bool `json:"is_default"`
}

// thinkingLevels returns the effort levels this model accepts, in a stable
// order, plus the default level.
func (e catalogEntry) thinkingLevels() ([]string, string) {
	if e.ThinkingConfig == nil || e.ThinkingConfig.Enabled == nil || len(e.ThinkingConfig.Enabled.Efforts) == 0 {
		return nil, ""
	}
	levels := make([]string, 0, len(e.ThinkingConfig.Enabled.Efforts))
	def := ""
	for name, cfg := range e.ThinkingConfig.Enabled.Efforts {
		if strings.TrimSpace(name) == "" {
			continue
		}
		levels = append(levels, name)
		if cfg.IsDefault {
			def = name
		}
	}
	sortThinkingLevels(levels)
	if def == "" && len(levels) > 0 {
		def = levels[0]
	}
	return levels, def
}

// thinkingToggleable reports whether thinking can be turned off (the catalog
// carries a "disabled" variant).
func (e catalogEntry) thinkingToggleable() bool {
	return e.ThinkingConfig != nil && e.ThinkingConfig.Disabled != nil
}

// contextWindow returns the default context window, falling back to the input
// token limit.
func (e catalogEntry) contextWindow() int64 {
	var def, largest int64
	for _, cfg := range e.ContextConfig {
		if cfg.TokenCount > largest {
			largest = cfg.TokenCount
		}
		if cfg.IsDefault && cfg.TokenCount > 0 {
			def = cfg.TokenCount
		}
	}
	if def > 0 {
		return def
	}
	if e.MaxInputTokens > 0 {
		return e.MaxInputTokens
	}
	if largest > 0 {
		return largest
	}
	return 180000
}

// modelDisplayName appends the upstream charge rate to the model name, the same
// way the WorkBuddy plugin does it: display_name is the only extra field the
// host's model pickers and the /v1/models formats carry, so it is where a user
// sees what a model costs. Empty rate leaves the name untouched.
func modelDisplayName(name string, factor float64, free bool) string {
	name = strings.TrimSpace(name)
	// A zero rate is shown as x0.0 when the catalogue marks the model free: it
	// costs nothing, which is the reason to pick it. The static fallback table
	// carries no rates at all, so its entries stay unsuffixed.
	if factor <= 0 && !free {
		return name
	}
	rate := strconv.FormatFloat(factor, 'f', -1, 64)
	if !strings.Contains(rate, ".") {
		rate += ".0"
	}
	return name + " \u00b7 x" + rate
}

// modelInfo renders one catalog entry for the host.
func (e catalogEntry) modelInfo() pluginapi.ModelInfo {
	info := pluginapi.ModelInfo{
		ID:                         e.Key,
		Object:                     "model",
		OwnedBy:                    providerName,
		Name:                       firstNonEmpty(e.DisplayName, e.Key),
		DisplayName:                modelDisplayName(firstNonEmpty(e.DisplayName, e.Key), e.PriceFactor, e.IsFree),
		ContextLength:              e.contextWindow(),
		InputTokenLimit:            firstNonZero64(e.MaxInputTokens, e.contextWindow()),
		MaxCompletionTokens:        defaultMaxOutputTokens,
		OutputTokenLimit:           defaultMaxOutputTokens,
		SupportedGenerationMethods: []string{"chat"},
	}
	levels, _ := e.thinkingLevels()
	if e.IsReasoning {
		info.Thinking = &pluginapi.ThinkingSupport{
			Levels:      levels,
			ZeroAllowed: e.thinkingToggleable(),
		}
	}
	return info
}

func sortThinkingLevels(levels []string) {
	rank := map[string]int{"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}
	sort.SliceStable(levels, func(i, j int) bool {
		ri, iOK := rank[levels[i]]
		rj, jOK := rank[levels[j]]
		if iOK && jOK {
			return ri < rj
		}
		if iOK != jOK {
			return iOK
		}
		return levels[i] < levels[j]
	})
}

// isChatModel filters catalog entries that cannot serve chat completions.
func isChatModel(key string) bool {
	if key == "" {
		return false
	}
	if key == "lite" {
		return false
	}
	for _, p := range []string{"codewise-", "completion-"} {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	for _, s := range []string{"-image-alpha", "-image-alpha-edit", "-taco-completion"} {
		if strings.HasSuffix(key, s) {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Catalog cache
// -----------------------------------------------------------------------------

type catalogSnapshot struct {
	entries []catalogEntry
	fetched time.Time
	live    bool // came from an upstream fetch, not the embedded snapshot
}

var catalogCache sync.Map // catalog key (realm + account) -> catalogSnapshot

// catalogKeyFor scopes a cached catalogue to one credential.
//
// Entitlements are per account, not per realm: a CN free account reports
// price_factor 0 with is_free on every model while a Pro/Trial account reports
// the real rates, and an exhausted account can report models as not enabled.
// Keying the cache by realm made whichever account refreshed first decide what
// every other account in that region could see.
func catalogKeyFor(sa *storedAuth) string {
	if sa == nil {
		return ""
	}
	id := realmOf(sa).ID
	if uid := strings.TrimSpace(sa.Account.UID); uid != "" {
		return id + ":" + uid
	}
	return id
}

func cachedCatalog(realmID string) ([]catalogEntry, bool) {
	v, ok := catalogCache.Load(realmID)
	if !ok {
		return nil, false
	}
	snap, ok := v.(catalogSnapshot)
	if !ok || len(snap.entries) == 0 || time.Since(snap.fetched) >= catalogTTL {
		return nil, false
	}
	return snap.entries, true
}

func storeCatalog(realmID string, entries []catalogEntry, live bool) {
	catalogCache.Store(realmID, catalogSnapshot{entries: entries, fetched: time.Now(), live: live})
}

// cachedCatalogState reports a realm's catalog and whether it is live data or
// the embedded fallback snapshot. Used by the panel's status strip.
// warmCatalogs fetches the live catalogue for any realm that has a credential
// but no cached entries yet. The panel's status strip reads this cache, so a
// cold process would otherwise claim "upstream not reached" for a realm that is
// perfectly healthy. Failures are logged and left to the next attempt.
func warmCatalogs(accounts []*storedAuth) {
	for _, sa := range accounts {
		if sa == nil {
			continue
		}
		key := catalogKeyFor(sa)
		// Only a live snapshot counts as warm: the cache may already hold the
		// embedded fallback, which must not stop us from reaching upstream.
		if v, ok := catalogCache.Load(key); ok {
			if snap, ok2 := v.(catalogSnapshot); ok2 && snap.live && len(snap.entries) > 0 {
				continue
			}
		}
		entries, err := fetchCatalogFrom(sa, realmOf(sa).modelsURL())
		if err != nil {
			hostLogf("warn", fmt.Sprintf("catalog warm failed for %s: %v", key, err))
			continue
		}
		if len(entries) > 0 {
			storeCatalog(key, entries, true)
			hostLogf("info", fmt.Sprintf("catalog warm: %s %d models", key, len(entries)))
		}
	}
}

// catalogStateFor returns one credential's catalogue and whether it is live
// data. No cache entry means the embedded snapshot for its realm.
func catalogStateFor(sa *storedAuth) ([]catalogEntry, bool) {
	if key := catalogKeyFor(sa); key != "" {
		if v, ok := catalogCache.Load(key); ok {
			if snap, ok2 := v.(catalogSnapshot); ok2 && len(snap.entries) > 0 {
				return snap.entries, snap.live
			}
		}
	}
	return staticCatalog(realmOf(sa).ID), false
}

// cachedCatalogState reports the cached (possibly stale) catalogue for one
// credential key, without the static fallback. Used by tests and diagnostics.
func cachedCatalogState(key string) ([]catalogEntry, bool) {
	if v, ok := catalogCache.Load(key); ok {
		if snap, ok2 := v.(catalogSnapshot); ok2 && len(snap.entries) > 0 {
			return snap.entries, snap.live
		}
	}
	return nil, false
}

// mergeCatalogs unions per-account catalogues of one realm: a model stays
// available when any credential can serve it, and the enabled entry wins so the
// published facts (context, thinking levels, rate) describe a usable account.
func mergeCatalogs(lists [][]catalogEntry) []catalogEntry {
	out := make([]catalogEntry, 0, 32)
	index := make(map[string]int, 32)
	for _, list := range lists {
		for _, e := range list {
			if i, seen := index[e.Key]; seen {
				if out[i].Disabled && !e.Disabled {
					out[i] = e
				}
				continue
			}
			index[e.Key] = len(out)
			out = append(out, e)
		}
	}
	return out
}

// catalogForAuth returns the catalog of one credential's realm: dynamic when
// the account can be reached, static snapshot otherwise.
func catalogForAuth(sa *storedAuth) []catalogEntry {
	r := realmOf(sa)
	key := catalogKeyFor(sa)
	if entries, ok := cachedCatalog(key); ok {
		return entries
	}
	if sa != nil {
		if entries, err := fetchCatalogRemote(sa); err == nil && len(entries) > 0 {
			storeCatalog(key, entries, true)
			return entries
		}
	}
	return staticCatalog(r.ID)
}

// modelFacts looks one model up in the realm catalog. Unknown keys return the
// zero entry: the request then falls back to conservative defaults instead of
// failing (upstream routes unknown keys to the default model).
func modelFacts(sa *storedAuth, key string) (catalogEntry, bool) {
	for _, e := range catalogForAuth(sa) {
		if e.Key == key {
			return e, true
		}
	}
	return catalogEntry{Key: key}, false
}

// -----------------------------------------------------------------------------
// Upstream fetch
// -----------------------------------------------------------------------------

// fetchCatalogRemote performs one COSY-signed catalog request.
//
// The signature covers the body, so the body that is signed must be the body
// that is sent. Upstream is inconsistent here across client generations: the
// CLI sends the encoded empty object ("$kwm") and rejects a bare GET, while
// older shells sign an empty body. model_list_body selects which one to send;
// the default mirrors the CLI.
func fetchCatalogRemote(sa *storedAuth) ([]catalogEntry, error) {
	return fetchCatalogFrom(sa, realmOf(sa).modelsURL())
}

// fetchCatalogFrom requests one catalog URL. Split out so tests can point it at
// a stub server and pin the sign-what-you-send invariant.
func fetchCatalogFrom(sa *storedAuth, rawURL string) ([]catalogEntry, error) {
	body := ""
	if currentConfig().ModelListBody == modelListBodyKWM {
		body = qoderEncode([]byte("{}"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, rawURL, reader)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	}
	if err != nil {
		return nil, err
	}
	if err := applyCosyHeaders(req, sa, body, rawURL, "", false); err != nil {
		return nil, fmt.Errorf("cosy sign: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cosyUserAgent)

	// The catalogue is an idempotent metadata GET, so it goes direct instead of
	// through the host bridge: inside the host process the bridge answers with
	// status 0 for calls made off the request path, which made the panel report
	// a healthy realm as unreachable.
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	resp, err := hostHTTPDoDirect(req, bodyBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	return parseCatalog(resp.Body)
}

// parseCatalog decodes the catalog envelope and keeps chat-capable entries.
func parseCatalog(raw []byte) ([]catalogEntry, error) {
	var env struct {
		Chat []catalogEntry `json:"chat"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if len(env.Chat) == 0 {
		return nil, fmt.Errorf("models parse: empty chat scene")
	}
	out := make([]catalogEntry, 0, len(env.Chat))
	for _, e := range env.Chat {
		if !isChatModel(e.Key) {
			continue
		}
		if !e.Enable {
			e.Disabled = true
		}
		out = append(out, e)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Static snapshot (fallback when no account can be reached)
// -----------------------------------------------------------------------------

type staticEntry struct {
	key       string
	name      string
	reasoning bool
	vision    bool
	maxInput  int64
	context   int64
	efforts   []string
	// free marks a zero-credit model. Free-tier accounts live on these, so the
	// lifecycle must be able to tell "no credits but still usable" apart.
	free bool
}

//go:embed catalog_cn.json
var catalogCNSnapshot []byte

var staticCatalogOnce sync.Once
var staticCNEntries []catalogEntry

// staticCNSnapshot returns the embedded CN fallback snapshot. It is generated
// from a live response and carries model facts only — account entitlement is
// forced on, because the snapshot answers "which models exist", not "which ones
// this account may use".
func staticCNSnapshot() []catalogEntry {
	staticCatalogOnce.Do(func() {
		entries, err := parseCatalog(catalogCNSnapshot)
		if err != nil {
			hostLogf("warn", "embedded CN catalog snapshot unusable: "+err.Error())
			return
		}
		staticCNEntries = entries
	})
	return staticCNEntries
}

var staticGlobal = []staticEntry{
	{key: "auto", name: "Auto", vision: true, maxInput: 200000, context: 200000},
	{key: "ultimate", name: "Ultimate", reasoning: true, vision: true, maxInput: 1000000, context: 200000,
		efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{key: "performance", name: "Performance", vision: true, maxInput: 1000000, context: 272000,
		efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{key: "efficient", name: "Efficient", vision: true, maxInput: 200000, context: 200000, free: true},
	{key: "smodel", name: "Sonus", reasoning: true, vision: true, maxInput: 180000, context: 200000,
		efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{key: "cmodel", name: "Cantus", reasoning: true, vision: true, maxInput: 180000, context: 200000,
		efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{key: "qmodel_38max", name: "Qwen3.8-Max", reasoning: true, vision: true, maxInput: 180000, context: 200000,
		efforts: []string{"low", "medium", "xhigh"}},
	{key: "qfmodel", name: "Qwen3.8-Flash", reasoning: true, vision: true, maxInput: 180000, context: 200000, free: true,
		efforts: []string{"low", "medium", "xhigh"}},
	{key: "qmodel_latest", name: "Qwen3.7-Max", vision: true, maxInput: 1000000, context: 200000},
	{key: "qmodel", name: "Qwen3.7-Plus", vision: true, maxInput: 1000000, context: 200000},
	{key: "kmodel_latest", name: "Kimi-K3", vision: true, maxInput: 180000, context: 200000,
		efforts: []string{"low", "high", "max"}},
	{key: "kmodel", name: "Kimi-K2.8-Preview", vision: true, maxInput: 180000, context: 200000,
		efforts: []string{"low", "high", "max"}},
	{key: "gmodel", name: "GLM-5.3", reasoning: true, vision: true, maxInput: 180000, context: 200000,
		efforts: []string{"low", "high", "max"}},
	{key: "gfmodel", name: "GLM-5.3-Flash", reasoning: true, vision: true, maxInput: 1000000, context: 200000,
		efforts: []string{"high", "max"}},
	{key: "dmodel", name: "DeepSeek-V4-Pro", reasoning: true, vision: true, maxInput: 1000000, context: 200000,
		efforts: []string{"high", "max"}},
	{key: "dfmodel", name: "DeepSeek-Flash", reasoning: true, vision: true, maxInput: 1000000, context: 200000,
		efforts: []string{"low", "high", "max"}},
	{key: "mmodel", name: "MiniMax-M3", maxInput: 180000, context: 200000},
}

// thinkingConfigFromLevels builds a toggleable thinking config from a level
// list; the middle level is the default, matching upstream's habit of marking
// its middle effort as default.
func thinkingConfigFromLevels(levels []string) *thinkingConfig {
	if len(levels) == 0 {
		return nil
	}
	enabled := &thinkingEnabled{Efforts: make(map[string]thinkingEffort, len(levels))}
	for i, lv := range levels {
		enabled.Efforts[lv] = thinkingEffort{IsDefault: i == len(levels)/2}
	}
	return &thinkingConfig{Enabled: enabled, Disabled: &struct{}{}}
}

// staticCatalog renders the built-in snapshot for one realm.
func staticCatalog(realmID string) []catalogEntry {
	if realmByName(realmID).ID != realmGlobal {
		if entries := staticCNSnapshot(); len(entries) > 0 {
			return entries
		}
	}
	src := staticGlobal
	out := make([]catalogEntry, 0, len(src))
	for _, s := range src {
		e := catalogEntry{
			Key:            s.key,
			DisplayName:    s.name,
			Format:         "openai",
			Source:         "system",
			Enable:         true,
			IsVL:           s.vision,
			IsReasoning:    s.reasoning,
			MaxInputTokens: s.maxInput,
			IsFree:         s.free,
		}
		e.ThinkingConfig = thinkingConfigFromLevels(s.efforts)
		out = append(out, e)
	}
	return out
}

// configStaticModels renders the operator's `models:` override list.
func configStaticModels() []catalogEntry {
	cfgModels := currentConfig().StaticModels
	if len(cfgModels) == 0 {
		return nil
	}
	out := make([]catalogEntry, 0, len(cfgModels))
	for _, m := range cfgModels {
		if !m.Enabled {
			continue
		}
		e := catalogEntry{
			Key:            m.ID,
			DisplayName:    firstNonEmpty(m.Name, m.ID),
			Format:         "openai",
			Source:         "custom",
			Enable:         true,
			IsReasoning:    m.Reasoning,
			MaxInputTokens: m.ContextLength,
		}
		e.ThinkingConfig = thinkingConfigFromLevels(m.ThinkingLevels)
		out = append(out, e)
	}
	return out
}

// -----------------------------------------------------------------------------
// Host-facing model lists
// -----------------------------------------------------------------------------

// modelInfos renders a catalog for clients: operator overrides first, then the
// realm catalog, then a static snapshot for realms with no credential yet.
func modelInfos(entries []catalogEntry) []pluginapi.ModelInfo {
	seen := make(map[string]bool, len(entries))
	out := make([]pluginapi.ModelInfo, 0, len(entries))
	skipped := 0
	for _, e := range entries {
		if seen[e.Key] {
			continue
		}
		if e.Disabled {
			// Upstream lists models the account is not entitled to; advertising
			// them only produces 403s at request time.
			skipped++
			continue
		}
		seen[e.Key] = true
		out = append(out, e.modelInfo())
	}
	if len(out) == 0 && skipped > 0 {
		// Every entry was flagged unavailable. Rather than publish an empty
		// catalogue (which reads as "the plugin is broken"), fall back to
		// listing them and let upstream decide.
		hostLogf("warn", fmt.Sprintf("catalog reported %d models, none enabled for this account; listing them anyway", skipped))
		for _, e := range entries {
			if seen[e.Key] {
				continue
			}
			seen[e.Key] = true
			out = append(out, e.modelInfo())
		}
	}
	return out
}

// fetchDynamicModels builds the process-wide model list: every realm that has
// a credential contributes its dynamic catalog; realms without one fall back
// to the static snapshot so clients see the catalogue before the first login.
// loadStoredAuths returns every Qoder credential the host knows about.
func loadStoredAuths() []*storedAuth {
	files, err := hostAuthListFiles()
	if err != nil {
		return nil
	}
	out := make([]*storedAuth, 0, len(files))
	for _, f := range files {
		if !isQoderAuthFileName(f.Name) {
			continue
		}
		raw, err := hostAuthGetByIndex(f.AuthIndex)
		if err != nil {
			continue
		}
		sa, err := parseStored(raw)
		if err != nil || sa == nil {
			continue
		}
		out = append(out, sa)
	}
	return out
}

func fetchDynamicModels() []pluginapi.ModelInfo {
	entries := make([]catalogEntry, 0, 32)
	byRealm := map[string][][]catalogEntry{}
	for _, sa := range loadStoredAuths() {
		// catalogForAuth caches per credential, so a second account of the same
		// realm contributes its own entitlements instead of being skipped
		// (which let one exhausted account shrink the whole region).
		byRealm[realmOf(sa).ID] = append(byRealm[realmOf(sa).ID], catalogForAuth(sa))
	}
	// Stable realm order: CN then Global, then anything else.
	for _, id := range []string{realmCN, realmGlobal} {
		if lists := byRealm[id]; len(lists) > 0 {
			entries = append(entries, mergeCatalogs(lists)...)
			delete(byRealm, id)
		}
	}
	for _, lists := range byRealm {
		entries = append(entries, mergeCatalogs(lists)...)
	}
	if len(entries) == 0 {
		entries = append(entries, staticCatalog(realmCN)...)
	}
	if overrides := configStaticModels(); len(overrides) > 0 {
		entries = append(overrides, entries...)
	}
	return modelInfos(entries)
}

func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	sa, err := parseStored(storageJSON)
	if err != nil || sa == nil {
		return fetchDynamicModels()
	}
	entries := catalogForAuth(sa)
	if overrides := configStaticModels(); len(overrides) > 0 {
		entries = append(append([]catalogEntry{}, overrides...), entries...)
	}
	return modelInfos(entries)
}

// -----------------------------------------------------------------------------
// Alias resolution / host filters
// -----------------------------------------------------------------------------

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the Qoder provider.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Fresh slice: models[:0] would alias the input's backing array, which may
	// be a cache's own slice.
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := fetchDynamicModels()
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key: the host skips any
	// response whose Provider doesn't match the auth's provider, so echoing
	// req.AuthProvider back would silently drop the list for non-canonical
	// provider strings in legacy auth files.
	cacheModelAliases(req.Host)
	models := fetchDynamicModelsFromStorage(req.StorageJSON)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func firstNonZero64(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
