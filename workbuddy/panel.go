// panel.go serves the management dashboard: the aggregated account list the
// web UI consumes (buildDashboardEx) and the embedded HTML page itself.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbAccount is one row of the dashboard.
type wbAccount struct {
	AuthIndex    string          `json:"auth_index"`
	AuthID       string          `json:"auth_id,omitempty"`
	Name         string          `json:"name"`
	Label        string          `json:"label"`
	Nickname     string          `json:"nickname"`
	UID          string          `json:"uid"`
	Region       string          `json:"region"`               // "cn" or "global"
	Enterprise   bool            `json:"enterprise,omitempty"` // enterpriseId bound at login
	Plan         string          `json:"plan"`
	Status       string          `json:"status"`
	Disabled     bool            `json:"disabled"`
	Exhausted    bool            `json:"exhausted"`
	Selected     bool            `json:"selected"` // panel active routing card
	Credits      *creditsSummary `json:"credits,omitempty"`
	Checkin      *checkinSummary `json:"checkin,omitempty"`
	TrialClaimed bool            `json:"trial_claimed,omitempty"` // Global: expert trial already claimed
	Error        string          `json:"error,omitempty"`
}

type modelStatus struct {
	State             modelReadinessState `json:"state"`
	Message           string              `json:"message"`
	MetadataSource    modelSnapshotSource `json:"metadata_source"`
	MetadataFetchedAt string              `json:"metadata_fetched_at"`
	Auths             []modelAuthStatus   `json:"auths"`
}

type modelAuthStatus struct {
	AuthIndex       string              `json:"auth_index"`
	State           modelReadinessState `json:"state"`
	ModelSource     modelSnapshotSource `json:"model_source"`
	ModelsFetchedAt string              `json:"models_fetched_at"`
	ErrorCode       modelErrorCode      `json:"error_code"`
}

var modelStatusMessages = map[modelReadinessState]string{
	modelReady:      "模型目录已就绪",
	modelStale:      "模型目录刷新失败，正在使用上次有效缓存",
	modelFailed:     "模型目录不可用",
	modelLoading:    "模型目录正在初始化",
	modelNotStarted: "模型目录尚未初始化",
}

var modelStatePriority = map[modelReadinessState]int{
	modelReady:      1,
	modelStale:      2,
	modelNotStarted: 3,
	modelLoading:    4,
	modelFailed:     5,
}

var panelHostAuthList = hostAuthList

func buildModelStatus(files []pluginapi.HostAuthFileEntry) modelStatus {
	runtime := activeModelRuntime.Load()
	metadata := modelMetadataStatus{Source: modelSourceNone}
	if runtime != nil {
		metadata = runtime.metadataStatus()
	}
	state := modelReady
	if len(files) == 0 {
		state = modelNotStarted
	}
	auths := make([]modelAuthStatus, 0, len(files))
	for _, file := range files {
		snapshot := modelReadinessSnapshot{State: modelNotStarted, ModelSource: modelSourceNone}
		if runtime != nil {
			snapshot = runtime.snapshotForAuthID(file.ID)
		}
		auths = append(auths, modelAuthStatus{
			AuthIndex:       file.AuthIndex,
			State:           snapshot.State,
			ModelSource:     snapshot.ModelSource,
			ModelsFetchedAt: modelStatusTime(snapshot.ModelsFetchedAt),
			ErrorCode:       snapshot.ErrorCode,
		})
		if modelStatePriority[snapshot.State] > modelStatePriority[state] {
			state = snapshot.State
		}
	}
	return modelStatus{
		State:             state,
		Message:           modelStatusMessages[state],
		MetadataSource:    metadata.Source,
		MetadataFetchedAt: modelStatusTime(metadata.FetchedAt),
		Auths:             auths,
	}
}

func modelStatusTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// credits/checkin/plan fields are left empty — the panel renders skeletons
// and fetches them lazily via /credits?auth_index=<idx>. This avoids hitting
// upstream billing APIs for all accounts simultaneously on page load (which
// causes 500 from rate-limited /v2/billing/meter/get-user-resource).
func buildDashboardEx(force, fetchCredits bool) map[string]any {
	return buildDashboardExWithCallback(force, fetchCredits, "")
}

func buildDashboardExWithCallback(force, fetchCredits bool, callbackID string) map[string]any {
	files, err := panelHostAuthList()
	if err != nil {
		return map[string]any{
			"error":        err.Error(),
			"model_status": buildModelStatus(nil),
		}
	}
	statusFiles := files
	// Prune cache entries for accounts that no longer exist (auth deleted via
	// CPA UI) or whose TTL expired long ago. Without this, accountCache grows
	// monotonically for the lifetime of the process.
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
	}
	accountCache.Range(func(key, value any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			accountCache.Delete(key)
			checkinLocks.Delete(key)
			lifecycleState.Delete(key)
			return true
		}
		if e, ok := value.(*accountCacheEntry); ok && time.Since(e.fetched) > 4*accountCacheTTL {
			accountCache.Delete(key)
		}
		return true
	})
	// Also prune stale lifecycle state and checkin locks for gone accounts.
	pruneLifecycleState()
	pruneCheckinLocks()
	out := make([]wbAccount, len(files))
	// Accounts are independent — fetch their dashboards concurrently. With 4
	// accounts this cuts cold-load latency from ~4×(3 serial upstream calls)
	// to roughly one slowest account.
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			acct := wbAccount{
				AuthIndex: f.AuthIndex,
				AuthID:    f.ID,
				Name:      f.Name,
				Label:     f.Label,
				Status:    f.Status,
				Disabled:  f.Disabled,
			}
			sa, phys, err := hostAuthGetBundle(f.AuthIndex)
			if err != nil {
				acct.Error = "load auth: " + err.Error()
				out[i] = acct
				return
			}
			// Physical file is source of truth for disabled (host list may lag).
			if phys != nil {
				acct.Disabled = phys.Disabled
				if phys.Name != "" {
					acct.Name = phys.Name
				}
			}
			acct.Nickname = displayNameFor(sa)
			acct.UID = sa.Account.UID
			acct.Region = accountRegion(sa)
			acct.Enterprise = isEnterpriseAccount(sa)
			if fetchCredits {
				plan, ci, cr, errs := cachedAccountDetailsWithCallback(f.ID, sa, force, callbackID)
				acct.Plan = plan
				acct.Checkin = ci
				acct.Credits = cr
				acct.Exhausted = isCreditsExhausted(cr)
				if isGlobalDomain(sa.Auth.Domain) {
					acct.TrialClaimed = hasTrialPack(cr)
				}
				// Keep note in sync (throttled); do not block dashboard on save errors.
				_ = syncAuthNote(f.AuthIndex, f.ID, sa, cr, acct.Disabled)
				acct.Error = strings.Join(errs, "; ")
			} else {
				// Light load: use cached values if available, but don't fetch upstream.
				if v, ok := accountCache.Load(f.ID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 {
						acct.Plan = e.plan
						acct.Checkin = e.checkin
						acct.Credits = e.credits
						acct.Exhausted = isCreditsExhausted(e.credits)
						if isGlobalDomain(sa.Auth.Domain) {
							acct.TrialClaimed = hasTrialPack(e.credits)
						}
					}
				}
			}
			out[i] = acct
		}(i, f)
	}
	wg.Wait()
	// After refresh (force), run lifecycle so exhaust→disable/delete is immediate.
	var life []map[string]any
	if force && lifecycleEnabled() {
		life = reconcileAllAccountsWithCallback(true, callbackID)
		// Drop accounts deleted during reconcile (Global exhaust) and refresh
		// disabled/exhausted from disk/cache (host list may lag after save).
		if files2, err2 := panelHostAuthList(); err2 == nil {
			statusFiles = files2
			live := make(map[string]struct{}, len(files2))
			disabledBy := make(map[string]bool, len(files2))
			for _, f := range files2 {
				live[f.AuthIndex] = struct{}{}
				// Prefer host list Disabled after reconcile; avoids N extra host.auth.get.
				// Dashboard row load already used hostAuthGetBundle for physical truth.
				disabledBy[f.AuthIndex] = f.Disabled
			}
			filtered := out[:0]
			for _, a := range out {
				if _, ok := live[a.AuthIndex]; !ok {
					continue
				}
				if d, ok := disabledBy[a.AuthIndex]; ok {
					a.Disabled = d
				}
				// Credits may have been refreshed during reconcile — re-read cache.
				if v, ok := accountCache.Load(a.AuthID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 {
						if e.credits != nil {
							a.Credits = e.credits
							a.Exhausted = isCreditsExhausted(e.credits)
						}
						if e.plan != "" {
							a.Plan = e.plan
						}
						if e.checkin != nil {
							a.Checkin = e.checkin
						}
					}
				}
				filtered = append(filtered, a)
			}
			out = filtered
		}
	}
	checkinAutoMu.RLock()
	auto := checkinAuto
	checkinAutoMu.RUnlock()
	// Ensure default selection for panel + scheduler (first usable card).
	activeID := ensureDefaultActiveAuth(out)
	// Aggregate credits for panel/API consumers (all accounts currently in out).
	sum := summarizeCredits(out)
	// Mark selected account in list for UI.
	for i := range out {
		out[i].Selected = out[i].AuthID == activeID
	}
	resp := map[string]any{
		"accounts":       out,
		"active_auth":    activeID,
		"checkin_auto":   auto,
		"lifecycle_auto": lifecycleEnabled(),
		"schedule":       []string{"09:00", "21:00"},
		"server_time":    time.Now().Format("2006-01-02 15:04:05"),
		"summary":        sum,
		"model_status":   buildModelStatus(statusFiles),
	}
	if len(life) > 0 {
		resp["lifecycle"] = life
	}
	return resp
}

// summarizeCredits aggregates remain/used across dashboard accounts.
func summarizeCredits(accounts []wbAccount) map[string]any {
	var remain, used, size, cnRemain, cnUsed, cnSize, glRemain, glUsed, glSize int64
	var known, disabledN, exhaustedN, packs int
	for _, a := range accounts {
		if a.Disabled {
			disabledN++
		}
		if a.Exhausted {
			exhaustedN++
		}
		if a.Credits == nil {
			continue
		}
		cr := a.Credits
		if cr.TotalRemain == 0 && cr.TotalUsed == 0 && cr.TotalSize == 0 && len(cr.Packages) == 0 {
			continue
		}
		known++
		remain += cr.TotalRemain
		used += cr.TotalUsed
		size += cr.TotalSize
		packs += cr.PackCount
		if a.Region == "global" {
			glRemain += cr.TotalRemain
			glUsed += cr.TotalUsed
			glSize += cr.TotalSize
		} else {
			cnRemain += cr.TotalRemain
			cnUsed += cr.TotalUsed
			cnSize += cr.TotalSize
		}
	}
	total := remain + used
	if size > total {
		total = size
	}
	return map[string]any{
		"account_count":   len(accounts),
		"known_count":     known,
		"disabled_count":  disabledN,
		"exhausted_count": exhaustedN,
		"pack_count":      packs,
		"total_remain":    remain,
		"total_used":      used,
		"total_size":      size,
		"total":           total,
		"cn_remain":       cnRemain,
		"cn_used":         cnUsed,
		"cn_size":         cnSize,
		"global_remain":   glRemain,
		"global_used":     glUsed,
		"global_size":     glSize,
	}
}

const egressIPURL = "https://api.ipify.org?format=json"

func fetchEgressIP() (string, error) {
	return fetchEgressIPWithCallback("")
}

func fetchEgressIPWithCallback(callbackID string) (string, error) {
	state := currentProxyState()
	if runtime.GOOS == "windows" && state.mode == proxyModeInherit {
		return "", fmt.Errorf("egress IP unavailable for inherited Windows routing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, egressIPURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hostHTTPDoWithStateAndCallback(state, req, callbackID)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("egress IP service returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return "", fmt.Errorf("decode egress IP: %w", err)
	}
	parsed := net.ParseIP(strings.TrimSpace(payload.IP))
	if parsed == nil {
		return "", fmt.Errorf("egress IP service returned an invalid address")
	}
	return parsed.String(), nil
}

// Web panel (self-contained HTML, no external assets)
// -----------------------------------------------------------------------------

func servePanel(sub string) []byte {
	if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
		return []byte("<h1>404</h1>")
	}
	base, _ := json.Marshal(loadedManagementBasePath())
	return bytes.ReplaceAll(panelHTML, []byte("__WB_MANAGEMENT_BASE_PATH_JSON__"), base)
}

//go:embed panel.html
var panelHTML []byte
