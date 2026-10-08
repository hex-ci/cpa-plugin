// catalog.go fetches the account's model catalogue for the solo channel. The
// chat request picks a model by config_name, so the catalogue is what the host
// must offer to clients.
package main

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// soloCatalogEntry is one model in get_detail_param's config_info_list.
type soloCatalogEntry struct {
	ConfigName          string            `json:"config_name"`
	ConfigSwitch        bool              `json:"config_switch"`
	ContextWindowTokens map[string]int64  `json:"context_window_tokens"`
	ModelDetailList     []json.RawMessage `json:"model_detail_list"`
	DisplayConfig       struct {
		DisplayName     string `json:"display_name"`
		IsBeta          bool   `json:"is_beta"`
		IsCustomModel   bool   `json:"is_custom_model"`
		ModelCapability string `json:"model_capability"`
		Multimodal      *bool  `json:"multimodal"`
	} `json:"display_config"`
}

// errNoCredential reports a call that needs an account credential.
var errNoCredential = errors.New("traework: no account credential")

type soloCatalogResponse struct {
	ConfigInfoList []soloCatalogEntry `json:"config_info_list"`
}

// catalogExcluded are entries in config_info_list that are not chat models:
// agent/sub-agent channels the desktop client drives internally, and the
// tenant placeholder. Everything else the account can chat with is offered.
var catalogExcluded = map[string]bool{
	"computer_use_subagent":    true,
	"browser_use_subagent":     true,
	"file_search_agent":        true,
	"explore_sub_agent_v2":     true,
	"summary":                  true,
	"custom_model_placeholder": true,
}

// catalogBaseline keeps the plugin useful before any catalogue fetch succeeded
// (no auth yet, or the fetch failed): the stable ids of the solo channel.
var catalogBaseline = []string{
	"Doubao-Seed-2.0-Code",
	"Doubao-Seed-2.1-Pro",
	"glm-5.3",
	"glm-5.2",
	"deepseek-v4.1-flash",
	"DeepSeek-V4-Pro",
	"kimi-k3",
	"qwen3.8-max",
}

type catalogCacheEntry struct {
	models  []pluginapi.ModelInfo
	fetched time.Time
}

var catalogCache sync.Map // uid(string) -> catalogCacheEntry

func cachedCatalog(uid string) ([]pluginapi.ModelInfo, bool) {
	value, ok := catalogCache.Load(uid)
	if !ok {
		return nil, false
	}
	entry, ok := value.(catalogCacheEntry)
	if !ok || time.Since(entry.fetched) > catalogCacheTTL {
		catalogCache.Delete(uid)
		return nil, false
	}
	return entry.models, true
}

func storeCatalog(uid string, models []pluginapi.ModelInfo) {
	if uid == "" {
		return
	}
	catalogCache.Store(uid, catalogCacheEntry{models: models, fetched: time.Now()})
}

// fetchCatalog requests the catalogue with one account's credential.
func fetchCatalog(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	if sa == nil || sa.AccessToken == "" {
		return nil, errNoCredential
	}
	body := map[string]any{
		"function":            soloFunction,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	var payload soloCatalogResponse
	headers := ideHeaders(sa.AccessToken, sa.UID, sa.MachineID, sa.DeviceID)
	if err := postJSON(soloAPIBase+soloCatalogPath, headers, body, &payload); err != nil {
		return nil, err
	}
	return catalogToModels(payload.ConfigInfoList), nil
}

// catalogToModels maps the upstream entries onto the host's model records,
// dropping non-chat channels and duplicates (the list repeats some ids).
func catalogToModels(entries []soloCatalogEntry) []pluginapi.ModelInfo {
	seen := make(map[string]bool, len(entries))
	out := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ConfigName)
		if id == "" || !entry.ConfigSwitch || catalogExcluded[id] || seen[id] {
			continue
		}
		seen[id] = true
		info := pluginapi.ModelInfo{
			ID:      id,
			Object:  "model",
			OwnedBy: providerName,
			Type:    "chat",
			Version: providerName,
			Created: time.Now().Unix(),
			Name:    id,
		}
		if name := strings.TrimSpace(entry.DisplayConfig.DisplayName); name != "" && name != "-" {
			info.DisplayName = name
		} else {
			info.DisplayName = id
		}
		if entry.ContextWindowTokens != nil {
			window := entry.ContextWindowTokens["dev"]
			if window == 0 {
				window = entry.ContextWindowTokens["prod"]
			}
			info.InputTokenLimit = window
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func baselineModels() []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(catalogBaseline))
	for _, id := range catalogBaseline {
		out = append(out, pluginapi.ModelInfo{
			ID:          id,
			Object:      "model",
			OwnedBy:     providerName,
			Type:        "chat",
			Name:        id,
			DisplayName: id,
		})
	}
	return out
}

// handleModelStatic has no credential, so it cannot read the account catalogue:
// it offers the baseline list, and the per-auth call replaces it with the real
// one as soon as an account exists.
func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: baselineModels()}), nil
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		// A credential the plugin cannot read still has to answer: fall back to
		// the baseline so the account is not left without models.
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: baselineModels()}), nil
	}
	if models, ok := cachedCatalog(sa.UID); ok {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models}), nil
	}
	models, err := fetchCatalog(sa)
	if err != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: baselineModels()}), nil
	}
	storeCatalog(sa.UID, models)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models}), nil
}
