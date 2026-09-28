package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxDiscoveredModelIDBytes = 512
const modelSourceRequestTimeout = 15 * time.Second

type modelFacts struct {
	ID                        string   `json:"id"`
	Name                      string   `json:"name,omitempty"`
	Description               string   `json:"description,omitempty"`
	Credits                   string   `json:"credits,omitempty"`
	ContextLength             *int64   `json:"context_length,omitempty"`
	MaxCompletionTokens       *int64   `json:"max_completion_tokens,omitempty"`
	SupportedInputModalities  []string `json:"supported_input_modalities,omitempty"`
	SupportedOutputModalities []string `json:"supported_output_modalities,omitempty"`
	// ReasoningEfforts lists the selectable thinking tiers for this model, and
	// ReasoningDefaultEffort names the one the upstream uses when the caller
	// picks none. Both are empty when the upstream declared nothing, which is
	// what keeps "no metadata" distinguishable from "no tiers".
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	ReasoningDefaultEffort string   `json:"reasoning_default_effort,omitempty"`
	// ReasoningZeroAllowed mirrors canDisableThinking: false means the model
	// always thinks, so a client that offers an off switch would be lying.
	ReasoningZeroAllowed *bool `json:"reasoning_zero_allowed,omitempty"`
}

type modelHTTPDo func(*http.Request, string) (*hostHTTPResponse, error)

// Modality names reported in ModelInfo.SupportedInputModalities. The host
// treats these as free-form labels and copies them through to the client-facing
// model registry, so the values here are the ones a client will see.
const (
	modalityText  = "text"
	modalityImage = "image"
)

type modelSourceFailureKind string

const (
	modelSourceTransportFailure modelSourceFailureKind = "transport"
	modelSourceHTTPFailure      modelSourceFailureKind = "http"
	modelSourceSchemaFailure    modelSourceFailureKind = "schema"
)

type modelSourceError struct {
	Kind       modelSourceFailureKind
	StatusCode int
	err        error
}

func (e *modelSourceError) Error() string {
	switch e.Kind {
	case modelSourceTransportFailure:
		return "model source transport failure"
	case modelSourceHTTPFailure:
		return fmt.Sprintf("model source HTTP %d", e.StatusCode)
	default:
		return "model source schema failure"
	}
}

func (e *modelSourceError) Unwrap() error {
	return e.err
}

type workBuddyRealm string

const (
	workBuddyRealmCN     workBuddyRealm = "cn"
	workBuddyRealmGlobal workBuddyRealm = "global"
)

type workBuddyEndpointKind string

const (
	workBuddyEndpointV3Config             workBuddyEndpointKind = "v3_config"
	workBuddyEndpointLegacyPersonalModels workBuddyEndpointKind = "legacy_personal_models"
	// workBuddyEndpointV3ConfigShapes marks a catalogue assembled from both v3
	// client shapes (desktop + CLI). They are not the same list: the two
	// rosters differ in BOTH directions (measured on a Global account
	// 2026-09-28: the desktop shape listed three models the CLI shape omitted,
	// the CLI shape one the desktop shape omitted), and every model in either
	// answered 200 on a chat request.
	workBuddyEndpointV3ConfigShapes workBuddyEndpointKind = "v3_config+desktop"
	// workBuddyEndpointV3ConfigUnion marks a catalogue assembled from the v3
	// config (one or both shapes) plus the CN-only legacy endpoint. The kind is
	// persisted with the cache, so a reader can tell a union snapshot apart
	// from a single-source one.
	workBuddyEndpointV3ConfigUnion workBuddyEndpointKind = "v3_config+legacy"
)

type workBuddyCatalog struct {
	Realm    workBuddyRealm        `json:"realm"`
	Endpoint workBuddyEndpointKind `json:"endpoint"`
	Models   []modelFacts          `json:"models"`
}

// workBuddyModelEntryWire is one entry of the upstream model catalog. It covers
// BOTH catalog shapes: /v3/config's data.models[] and the fuller
// /console/enterprises/{personal|<id>}/models payload. Field names follow the
// upstream's actual keys (maxOutputTokens / maxInputTokens) — an earlier
// revision used maxTokens / contextWindow, which never matched anything and
// left every limit nil, silently falling back to models.dev.
type workBuddyModelEntryWire struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	DescriptionEn string `json:"descriptionEn"`
	DescriptionZh string `json:"descriptionZh"`
	Disabled      bool   `json:"disabled"`
	// Credits is the upstream per-model charge rate ("x0.79 credits"; some
	// entries send a bare "x0.05"). Only the two catalog endpoints populate it;
	// it is what the host surfaces as a model's display_name.
	Credits   string `json:"credits"`
	MaxOutput *int64 `json:"maxOutputTokens"`
	MaxInput  *int64 `json:"maxInputTokens"`
	// SupportsImages declares whether the model accepts image input. A pointer
	// because the upstream omits the key for some entries (image/video
	// generators): absent means the upstream expressed nothing, which must stay
	// distinguishable from an explicit false. Declaring a modality the model
	// does not accept would invite clients to send payloads upstream rejects.
	SupportsImages *bool `json:"supportsImages"`
	// Reasoning carries the model's thinking-effort metadata. Measured shapes
	// (2026-09-18, /v3/config) include: a model with an explicit tier list plus
	// a default and a working off-switch; a model with a single tier and no
	// off-switch; and models with no tier list at all, only a fixed tier.
	// Absent fields mean the upstream expressed nothing and must stay
	// distinguishable from an explicit value.
	Reasoning *workBuddyReasoningWire `json:"reasoning"`
	// Tags carries classification labels ("is_recommend", "text-to-image", …).
	Tags []string `json:"tags"`
}

// workBuddyReasoningWire is the reasoning sub-object of a catalog entry.
type workBuddyReasoningWire struct {
	// Effort is a single fixed tier (e.g. "medium") for models that offer no
	// choice. When SupportedEfforts is absent but this is set, the model has
	// exactly one tier.
	Effort string `json:"effort"`
	// SupportedEfforts enumerates the selectable tiers.
	SupportedEfforts []string `json:"supportedEfforts"`
	// DefaultEffort is the tier the upstream uses when the caller names none.
	DefaultEffort string `json:"defaultEffort"`
	// CanDisableThinking: false means the model always thinks.
	CanDisableThinking *bool `json:"canDisableThinking"`
}

// effortLevels resolves the selectable tiers for this entry: the explicit list
// when present, otherwise a single-tier list built from the fixed effort, or
// nil when the upstream declared nothing.
func (r *workBuddyReasoningWire) effortLevels() []string {
	if r == nil {
		return nil
	}
	if len(r.SupportedEfforts) > 0 {
		return r.SupportedEfforts
	}
	if strings.TrimSpace(r.Effort) != "" {
		return []string{r.Effort}
	}
	return nil
}

// fact converts a catalog entry into modelFacts. The upstream sends the display
// name plus a localized description under descriptionEn/descriptionZh (the flat
// "description" field only exists in some older shapes), so each is resolved
// from whichever field the catalog actually populated.
func (m workBuddyModelEntryWire) fact() modelFacts {
	f := modelFacts{
		ID:   m.ID,
		Name: firstNonEmpty(m.Name, m.DescriptionEn),
		// maxInputTokens is the model's input window; the client uses it as the
		// context length (maxAllowedSize is a separate, larger allowance).
		Description:         firstNonEmpty(m.Description, m.DescriptionEn, m.DescriptionZh),
		Credits:             strings.TrimSpace(m.Credits),
		ContextLength:       m.MaxInput,
		MaxCompletionTokens: m.MaxOutput,
	}
	// Modality comes from the catalogue itself. It used to be filled from
	// models.dev, which is why this provider needed egress to a third-party
	// site to describe models it already knew about; the upstream has published
	// supportsImages all along.
	//
	// Only the positive case is declared. Output modalities are left unset:
	// every catalogue model produces text, and the upstream says nothing more,
	// so asserting more would be invention.
	if m.SupportsImages != nil && *m.SupportsImages {
		f.SupportedInputModalities = []string{modalityText, modalityImage}
	}
	// Reasoning metadata: only what the upstream declared. A model with a
	// fixed single tier reports a one-element list; a model with no metadata
	// at all reports nothing, which the host reads as "unknown" rather than
	// inventing tiers.
	if levels := m.Reasoning.effortLevels(); len(levels) > 0 {
		f.ReasoningEfforts = levels
	}
	if m.Reasoning != nil {
		// A default only counts when it is one of the model's own tiers —
		// otherwise the client would offer a value the model rejects.
		def := strings.TrimSpace(m.Reasoning.DefaultEffort)
		for _, lvl := range f.ReasoningEfforts {
			if lvl == def {
				f.ReasoningDefaultEffort = def
				break
			}
		}
		f.ReasoningZeroAllowed = m.Reasoning.CanDisableThinking
	}
	return f
}

// workBuddyAgentWire is the cli agent entry; its models[] is a plain ID list.
type workBuddyAgentWire struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

// parseWorkBuddyV3Config reads the /v3/config payload. The cli agent's models[]
// is the authoritative ORDERED id list; data.models[] carries the per-model
// limits, so the two are joined by id (limits are evidence-based, ids are the
// roster — the roster wins on membership, the detail map only enriches).
func parseWorkBuddyV3Config(raw []byte) ([]modelFacts, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			Agents []workBuddyAgentWire      `json:"agents"`
			Models []workBuddyModelEntryWire `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode v3 config: %w", err)
	}
	if response.Code == nil || *response.Code != 0 {
		return nil, fmt.Errorf("v3 config business code is not successful")
	}
	if response.Data == nil {
		return nil, fmt.Errorf("v3 config data is missing")
	}

	var modelIDs []string
	foundCLI := false
	for _, agent := range response.Data.Agents {
		if agent.Name != "cli" {
			continue
		}
		if foundCLI {
			return nil, fmt.Errorf("v3 config has multiple cli agents")
		}
		foundCLI = true
		modelIDs = agent.Models
	}
	if !foundCLI {
		return nil, fmt.Errorf("v3 config cli agent is missing")
	}

	details := make(map[string]workBuddyModelEntryWire, len(response.Data.Models))
	for _, entry := range response.Data.Models {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		details[id] = entry
	}

	models := make([]modelFacts, 0, len(modelIDs))
	for _, id := range modelIDs {
		entry, ok := details[strings.TrimSpace(id)]
		if !ok {
			// Roster id with no detail row: keep it, limits stay nil.
			models = append(models, modelFacts{ID: id})
			continue
		}
		// Disabled entries are withdrawn from the catalogue: offering one
		// invites the client to pick a model the upstream then refuses.
		if entry.Disabled {
			continue
		}
		// Non-chat entries (embedding/completion/image) are not reachable
		// through the chat endpoint; selecting one returns 11102.
		if isNonChatModel(entry) {
			continue
		}
		models = append(models, entry.fact())
	}
	return validateModelFacts(models)
}

// nonChatIDPrefixes are model-id prefixes for entries that are not chat models:
// embeddings, code completion, and the internal "codewise" family. Selecting one
// through the chat endpoint returns 11102 ("service info not found").
var nonChatIDPrefixes = []string{"nes-", "completion-", "codewise-"}

// nonChatMaxOutputTokens is the output ceiling below which an entry is treated
// as a non-chat utility model (classifiers, rerankers). The bound is guarded by
// "greater than zero" below so an entry whose limit the upstream did not state
// is never filtered on this rule.
const nonChatMaxOutputTokens = 256

// isNonChatModel reports whether a catalogue entry describes something other
// than a chat model. The three rules come from what happens when such an entry
// is selected: the upstream answers 11102.
func isNonChatModel(entry workBuddyModelEntryWire) bool {
	id := strings.ToLower(strings.TrimSpace(entry.ID))
	for _, prefix := range nonChatIDPrefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	// An explicitly tiny output limit means a utility model — but only when
	// the upstream actually stated one. A missing or zero limit is "unknown",
	// not "tiny", so those entries stay.
	if entry.MaxOutput != nil && *entry.MaxOutput > 0 && *entry.MaxOutput <= nonChatMaxOutputTokens {
		return true
	}
	for _, tag := range entry.Tags {
		if strings.EqualFold(strings.TrimSpace(tag), "text-to-image") {
			return true
		}
	}
	return false
}

// parseWorkBuddyLegacyModels reads the /console/enterprises/.../models payload.
//
// The roster is the authority, exactly as on the v3 endpoint: the payload's
// data.models[] is a FULL model table that also lists models this account
// cannot call. Measured on one real account: a model in the full list but
// absent from the cli roster answered 400 code=11102 ("service info not
// found"), while roster members answered 200 — so exposing the full list would
// offer clients models that cannot run. The cli roster lists what the account's
// client may actually use, and data.models[] supplies the per-model fields for
// those ids.
func parseWorkBuddyLegacyModels(raw []byte) ([]modelFacts, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
			Models []workBuddyModelEntryWire `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode legacy models: %w", err)
	}
	if response.Code == nil || *response.Code != 0 {
		return nil, fmt.Errorf("legacy models business code is not successful")
	}
	if response.Data == nil {
		return nil, fmt.Errorf("legacy models data is missing")
	}

	var roster []string
	for _, agent := range response.Data.Agents {
		if strings.EqualFold(strings.TrimSpace(agent.Name), "cli") {
			roster = agent.Models
			break
		}
	}
	if len(roster) == 0 {
		return nil, fmt.Errorf("legacy models cli roster is missing")
	}

	details := make(map[string]workBuddyModelEntryWire, len(response.Data.Models))
	for _, entry := range response.Data.Models {
		if id := strings.TrimSpace(entry.ID); id != "" {
			details[id] = entry
		}
	}

	models := make([]modelFacts, 0, len(roster))
	for _, id := range roster {
		id = strings.TrimSpace(id)
		entry, ok := details[id]
		if !ok {
			// Roster id with no detail row: keep it, limits stay nil. An empty
			// id is NOT skipped — validateModelFacts rejects it, which is what
			// makes a truncated roster a refresh failure instead of a silently
			// shortened catalogue.
			models = append(models, modelFacts{ID: id})
			continue
		}
		// Same withdraw/filter rules as the v3 path: a disabled entry or a
		// non-chat model would only earn the client an 11102.
		if entry.Disabled || isNonChatModel(entry) {
			continue
		}
		models = append(models, entry.fact())
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("model snapshot is empty")
	}
	return validateModelFacts(models)
}

func validateModelFacts(models []modelFacts) ([]modelFacts, error) {
	if len(models) == 0 {
		return nil, fmt.Errorf("model snapshot is empty")
	}

	validated := make([]modelFacts, len(models))
	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		model.Name = strings.TrimSpace(model.Name)
		if model.ID == "" {
			return nil, fmt.Errorf("model ID is empty")
		}
		if len(model.ID) > maxDiscoveredModelIDBytes {
			return nil, fmt.Errorf("model ID exceeds %d bytes", maxDiscoveredModelIDBytes)
		}
		if _, exists := seen[model.ID]; exists {
			return nil, fmt.Errorf("model ID is duplicated")
		}
		if model.ContextLength != nil && *model.ContextLength < 0 {
			return nil, fmt.Errorf("model context length is negative")
		}
		if model.MaxCompletionTokens != nil && *model.MaxCompletionTokens < 0 {
			return nil, fmt.Errorf("model max completion tokens is negative")
		}
		if model.SupportedInputModalities != nil {
			model.SupportedInputModalities = append([]string{}, model.SupportedInputModalities...)
		}
		if model.SupportedOutputModalities != nil {
			model.SupportedOutputModalities = append([]string{}, model.SupportedOutputModalities...)
		}
		seen[model.ID] = struct{}{}
		validated[i] = model
	}
	return validated, nil
}

// modelDisplayName appends the upstream charge rate to the model name for the
// host's display_name surfaces, so a picked model shows what it costs. Empty
// when the catalog sent no rate, which leaves display_name at its prior value
// (the host then falls back to the model ID).
func modelDisplayName(name, credits string) string {
	name = strings.TrimSpace(name)
	credits = strings.TrimSpace(credits)
	if credits == "" {
		return ""
	}
	if name == "" {
		return credits
	}
	return name + " \u00b7 " + credits
}

// modelInfoFromFacts converts one catalogue entry into the host's ModelInfo.
//
// Every field comes from the WorkBuddy catalogue. There is no second source:
// an earlier revision merged in models.dev records to fill fields the upstream
// supposedly lacked, but the upstream publishes all of them (id, name,
// description, context window, output limit, charge rate, image support), so
// the merge only added a third-party network dependency and a cache to keep
// consistent. See model_source_workbuddy.go for the field mapping.
func modelInfoFromFacts(facts modelFacts) pluginapi.ModelInfo {
	info := defaultModelInfo(facts.ID, facts.Name)
	info.Description = facts.Description
	if display := modelDisplayName(facts.Name, facts.Credits); display != "" {
		info.DisplayName = display
	}
	if facts.ContextLength != nil {
		info.ContextLength = *facts.ContextLength
	}
	if facts.MaxCompletionTokens != nil {
		info.MaxCompletionTokens = *facts.MaxCompletionTokens
	}
	info.SupportedInputModalities = append([]string(nil), facts.SupportedInputModalities...)
	info.SupportedOutputModalities = append([]string(nil), facts.SupportedOutputModalities...)
	// Thinking declares the model's real tiers to the host. Left nil when the
	// catalogue said nothing, which the host reads as "this model has no
	// thinking metadata" — it does not invent tiers on its own, it simply
	// strips thinking configuration for models it has no data for.
	if len(facts.ReasoningEfforts) > 0 || facts.ReasoningZeroAllowed != nil {
		thinking := &pluginapi.ThinkingSupport{Levels: append([]string(nil), facts.ReasoningEfforts...)}
		if facts.ReasoningZeroAllowed != nil {
			thinking.ZeroAllowed = *facts.ReasoningZeroAllowed
		}
		info.Thinking = thinking
	}
	return info
}

func fetchWorkBuddyCatalog(sa *storedAuth, callbackID string, do modelHTTPDo) (workBuddyCatalog, error) {
	if sa == nil {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: fmt.Errorf("stored auth is nil")}
	}
	realm, err := workBuddyRealmFromAccessToken(sa.Auth.AccessToken)
	if err != nil {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
	}

	base := upstreamBaseCN
	origin := originReferer
	if realm == workBuddyRealmGlobal {
		base = upstreamBaseGlobal
		origin = originRefererGlobal
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelSourceRequestTimeout)
	defer cancel()

	request := func(path, userAgent string) (*hostHTTPResponse, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
		}
		backendHeaders(req, sa)
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		resp, err := do(req, callbackID)
		if err != nil {
			return nil, &modelSourceError{Kind: modelSourceTransportFailure, err: err}
		}
		if resp == nil {
			return nil, &modelSourceError{Kind: modelSourceTransportFailure, err: fmt.Errorf("empty HTTP response")}
		}
		return resp, nil
	}

	// One /v3/config fetch per client shape. The response is UA-scoped: only a
	// UA carrying the "WorkBuddy/" product segment gets the desktop-shaped
	// payload (the version inside it is ignored — measured 2026-09-28 with
	// variants "WorkBuddy/5.6.2", "WorkBuddy/x" and the full desktop string,
	// all identical), while the CLI-shaped UA gets a smaller payload. The two
	// rosters differ in BOTH directions and every model in either is callable
	// (verified one by one on the live service), so both are fetched and
	// merged; a single fetch would hide working models from clients.
	//
	// The merge is deliberately not all-or-nothing: a single source failing
	// degrades to whatever the others returned rather than failing the
	// refresh, because a partial catalogue that lists working models beats no
	// catalogue at all. All sources failing is a real refresh failure.
	fetchV3 := func(userAgent string) ([]modelFacts, error) {
		resp, err := request("/v3/config", userAgent)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &modelSourceError{Kind: modelSourceHTTPFailure, StatusCode: resp.StatusCode}
		}
		models, parseErr := parseWorkBuddyV3Config(resp.Body)
		if parseErr != nil {
			return nil, &modelSourceError{Kind: modelSourceSchemaFailure, err: parseErr}
		}
		return models, nil
	}

	desktopModels, desktopErr := fetchV3(catalogUA)
	cliModels, cliErr := fetchV3("")

	var v3Models []modelFacts
	v3OK := false
	v3Kind := workBuddyEndpointV3Config
	switch {
	case desktopErr == nil && cliErr == nil:
		// The desktop shape leads: it is the catalogue the official client
		// reads, so its ordering and field values are the authoritative ones.
		v3Models = unionModelFacts(desktopModels, cliModels)
		v3OK = true
		v3Kind = workBuddyEndpointV3ConfigShapes
	case desktopErr == nil:
		logModelSourceDegrade(realm, "cli catalogue shape unavailable", cliErr)
		v3Models, v3OK = desktopModels, true
	case cliErr == nil:
		logModelSourceDegrade(realm, "desktop catalogue shape unavailable", desktopErr)
		v3Models, v3OK = cliModels, true
	}
	var v3Err error
	if !v3OK {
		v3Err = desktopErr
		if v3Err == nil {
			v3Err = cliErr
		}
	}

	// The legacy endpoint is a CN-only route. Measured on 2026-09-28: on the
	// Global deployment (www.workbuddy.ai) every
	// /console/enterprises/{personal|<uid>}/models request answers HTTP 500
	// from APISIX, while /console/enterprises/personal/config/models answers
	// 200 with an empty list — the route family exists, the model list does
	// not. The official client reads its catalogue from /v3/config on both
	// realms, so the Global realm is served by that endpoint alone: requesting
	// the legacy path there buys one wasted round trip per refresh and a
	// permanent degradation warning for what is a deployment fact, not a
	// fault. If the route ever appears on Global, this is the one line to
	// change.
	legacyEnabled := realm == workBuddyRealmCN

	var legacyModels []modelFacts
	legacyOK := false
	var legacyErr error
	if legacyEnabled {
		legacyResp, err := request("/console/enterprises/personal/models", "")
		legacyErr = err
		if legacyErr == nil && legacyResp.StatusCode == http.StatusOK {
			models, parseErr := parseWorkBuddyLegacyModels(legacyResp.Body)
			if parseErr == nil {
				legacyModels, legacyOK = models, true
			} else {
				legacyErr = &modelSourceError{Kind: modelSourceSchemaFailure, err: parseErr}
			}
		} else if legacyErr == nil {
			legacyErr = &modelSourceError{Kind: modelSourceHTTPFailure, StatusCode: legacyResp.StatusCode}
		}
	}

	switch {
	case v3OK && legacyOK:
		// The legacy roster contributes models the v3 catalogue omits (on CN
		// it supplies "auto"), so the two are merged the same way the client
		// shapes are.
		return workBuddyCatalog{
			Realm:    realm,
			Endpoint: workBuddyEndpointV3ConfigUnion,
			Models:   unionModelFacts(v3Models, legacyModels),
		}, nil
	case v3OK:
		// The v3 catalogue alone. On CN this is the fallback shape for an
		// account without legacy entitlement (measured 401 for a personal
		// account with a valid token); on Global it is the only shape.
		logModelSourceDegrade(realm, "legacy endpoint unavailable", legacyErr)
		return workBuddyCatalog{Realm: realm, Endpoint: v3Kind, Models: v3Models}, nil
	case legacyOK:
		logModelSourceDegrade(realm, "v3 endpoint unavailable", v3Err)
		return workBuddyCatalog{Realm: realm, Endpoint: workBuddyEndpointLegacyPersonalModels, Models: legacyModels}, nil
	}
	if v3Err != nil {
		return workBuddyCatalog{}, v3Err
	}
	return workBuddyCatalog{}, legacyErr
}

// unionModelFacts merges two catalogues by model id.
//
// primary's entries win on field conflicts: it is the endpoint whose per-model
// fields (context, credits, reasoning) are authoritative, and the secondary
// endpoint exists to contribute models the primary omits, not to correct it.
// Ordering is primary-first, then the secondary's additions in their own order,
// so the list is stable across refreshes.
func unionModelFacts(primary, secondary []modelFacts) []modelFacts {
	merged := make([]modelFacts, 0, len(primary)+len(secondary))
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	for _, model := range primary {
		key := strings.TrimSpace(model.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, model)
	}
	for _, model := range secondary {
		key := strings.TrimSpace(model.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, model)
	}
	if len(merged) == 0 {
		// Callers only reach here with at least one non-empty side, but an
		// empty merge must never be published as a catalogue: the store treats
		// an empty list as "no models", which would wipe a working one.
		return primary
	}
	return merged
}

// logModelSourceDegrade records a single-source fallback. It is a notice, not
// an error: the sources that answered still produced a usable catalogue.
//
// Each (realm, reason) pair is logged once per process. The catalogue is
// re-fetched on every config reload and auth change, so an unconditional log
// turned one permanent deployment fact (e.g. a route that only exists on one
// realm) into a warning on every refresh. The per-account error codes in
// model_status remain the live diagnostic surface.
func logModelSourceDegrade(realm workBuddyRealm, reason string, cause error) {
	if cause == nil {
		return
	}
	key := string(realm) + "|" + reason
	if _, loaded := modelSourceDegradeLogged.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	hostLogf("warn", fmt.Sprintf("workbuddy model catalog %s: %s (%v)", realm, reason, cause))
}

// modelSourceDegradeLogged tracks the (realm, reason) pairs already reported.
var modelSourceDegradeLogged sync.Map

// workBuddyRealmFromAccessToken decodes unverified JWT routing facts only.
func workBuddyRealmFromAccessToken(accessToken string) (workBuddyRealm, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("decode JWT claims: %w", err)
	}
	issuer, err := url.Parse(claims.Issuer)
	if err != nil || !issuer.IsAbs() || issuer.Hostname() == "" {
		return "", fmt.Errorf("JWT issuer is not an absolute URL")
	}
	switch {
	case isGlobalDomain(issuer.Hostname()):
		return workBuddyRealmGlobal, nil
	}
	switch strings.ToLower(issuer.Hostname()) {
	case "codebuddy.cn", "www.codebuddy.cn", "copilot.tencent.com",
		"workbuddy.cn", "www.workbuddy.cn":
		return workBuddyRealmCN, nil
	default:
		return "", fmt.Errorf("JWT issuer host is unsupported")
	}
}
