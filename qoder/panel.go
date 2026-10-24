// panel.go serves the management dashboard: the aggregated account list the
// web UI consumes (buildDashboardEx) and the embedded HTML page itself.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbAccount is one row of the dashboard.
type wbAccount struct {
	AuthIndex string          `json:"auth_index"`
	AuthID    string          `json:"auth_id,omitempty"`
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	Nickname  string          `json:"nickname"`
	UID       string          `json:"uid"`
	Region    string          `json:"region"` // "cn" or "global"
	Plan      string          `json:"plan"`
	Status    string          `json:"status"`
	Disabled  bool            `json:"disabled"`
	Exhausted bool            `json:"exhausted"`
	Selected  bool            `json:"selected"` // panel active routing card
	Credits   *creditsSummary `json:"credits,omitempty"`
	Checkin   *checkinSummary `json:"checkin,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// credits/checkin/plan fields are left empty — the panel renders skeletons
// and fetches them lazily via /credits?auth_index=<idx>. This avoids hitting
// upstream billing APIs for all accounts simultaneously on page load (which
// causes 500 from rate-limited /v2/billing/meter/get-user-resource).
func buildDashboardEx(force, fetchCredits bool) map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
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
	var authMu sync.Mutex
	var loaded []*storedAuth
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
			authMu.Lock()
			loaded = append(loaded, sa)
			authMu.Unlock()
			acct.Nickname = sa.Account.Nickname
			acct.UID = sa.Account.UID
			acct.Region = realmOf(sa).ID
			if fetchCredits {
				plan, ci, cr, errs := cachedAccountDetails(f.ID, sa, force)
				acct.Plan = plan
				acct.Checkin = ci
				acct.Credits = cr
				acct.Exhausted = isCreditsExhausted(cr)
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
					}
				}
			}
			out[i] = acct
		}(i, f)
	}
	wg.Wait()
	// A forced refresh is the user asking for current state, so make sure the
	// catalog cache behind the status strip is populated instead of reporting a
	// cold process as "upstream not reached".
	if force {
		// Run it off the RPC path: a nested host call inside management.handle
		// returns status 0, so warming there would always fail.
		go warmCatalogs(loaded)
	}
	// After refresh (force), run lifecycle so exhaust→disable/delete is immediate.
	var life []map[string]any
	if force && lifecycleEnabled() {
		life = reconcileAllAccounts(true)
		// Drop accounts deleted during reconcile (CN exhaust) and refresh
		// disabled/exhausted from disk/cache (host list may lag after save).
		if files2, err2 := hostAuthList(); err2 == nil {
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
		"accounts":          out,
		"active_auth":       activeID,
		"checkin_auto":      auto,
		"lifecycle_auto":    lifecycleEnabled(),
		"queue_wait":        currentConfig().QueueWait,
		"cosy_version":      cosyVersion(),
		"oauth_client_mode": defaultRealmName(),
		"schedule":          []string{"10:00"}, // check-in window, China time
		"server_time":       time.Now().Format("2006-01-02 15:04:05"),
		"summary":           sum,
		"model_status":      modelReadiness(),
	}
	if len(life) > 0 {
		resp["lifecycle"] = life
	}
	return resp
}

// summarizeCredits aggregates remain/used across dashboard accounts.
func summarizeCredits(accounts []wbAccount) map[string]any {
	var remain, used, size, cnRemain, cnUsed, cnSize, glRemain, glUsed, glSize int64
	var availRemain, availSize int64
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
		// "可用" only counts accounts that can actually serve traffic: a
		// disabled or exhausted account inflates the number without being
		// spendable, which is the panel number users act on.
		if !a.Disabled && !a.Exhausted {
			availRemain += cr.TotalRemain
			if cr.TotalSize > 0 {
				availSize += cr.TotalSize
			} else {
				availSize += cr.TotalRemain + cr.TotalUsed
			}
		}
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
		"account_count":    len(accounts),
		"known_count":      known,
		"disabled_count":   disabledN,
		"exhausted_count":  exhaustedN,
		"pack_count":       packs,
		"total_remain":     remain,
		"available_remain": availRemain,
		"available_size":   availSize,
		"total_used":       used,
		"total_size":       size,
		"total":            total,
		"cn_remain":        cnRemain,
		"cn_used":          cnUsed,
		"cn_size":          cnSize,
		"global_remain":    glRemain,
		"global_used":      glUsed,
		"global_size":      glSize,
	}
}

// Web panel (self-contained HTML, no external assets)
// -----------------------------------------------------------------------------

func servePanel(sub string) []byte {
	if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
		return []byte("<h1>404</h1>")
	}
	return renderPanelHTML()
}

// renderPanelHTML injects the management base path so the page works when the
// host mounts its API somewhere other than /v0/management (reverse proxies,
// custom base paths). Hardcoding the path breaks the panel entirely there.
func renderPanelHTML() []byte {
	base, _ := json.Marshal(loadedManagementBasePath())
	return bytes.ReplaceAll(panelHTML, []byte("__QODER_MANAGEMENT_BASE_PATH_JSON__"), base)
}

// modelReadiness reports what the catalog currently is for the panel's status
// strip: how many models are known per region, and whether the last fetch was
// live. Catalogues are cached per credential, so a region's number is the union
// over its accounts — the same rule the published model list follows.
func modelReadiness() map[string]any {
	out := map[string]any{"state": "empty", "count": 0}
	states := map[string]any{}
	count, anySnapshot := 0, false
	byRealm := map[string][][]catalogEntry{}
	live := map[string]bool{}
	for _, sa := range loadStoredAuths() {
		if sa == nil {
			continue
		}
		id := realmOf(sa).ID
		entries, isLive := catalogStateFor(sa)
		if len(entries) == 0 {
			continue
		}
		byRealm[id] = append(byRealm[id], entries)
		live[id] = live[id] || isLive
	}
	for _, id := range []string{realmCN, realmGlobal} {
		lists := byRealm[id]
		if len(lists) == 0 {
			continue
		}
		entries := mergeCatalogs(lists)
		if !live[id] {
			anySnapshot = true
		}
		state := "ready"
		if !live[id] {
			state = "stale"
		}
		states[id] = map[string]any{"state": state, "count": len(entries)}
		count += len(entries)
	}
	if len(states) > 0 {
		// "stale" means at least one realm is running on the built-in
		// snapshot; the panel says so instead of pretending it is live.
		if anySnapshot {
			out["state"] = "stale"
		} else {
			out["state"] = "ready"
		}
		out["count"] = count
		out["realms"] = states
	}
	return out
}

//go:embed panel.html
var panelHTML []byte
