// credits_handler.go implements the management API endpoints that read
// account state: select active auth, and query credits for one or all accounts.
package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleSelectAuth sets the panel-selected account used for chat routing.
func handleSelectAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required", "active_auth": getActiveAuthID()}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		if f.Disabled {
			return map[string]any{"error": "账号已禁用，无法选中", "auth_index": authIndex}
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"error": err.Error(), "auth_index": authIndex}
		}
		setActiveAuthID(f.ID)
		return map[string]any{
			"ok":          true,
			"active_auth": f.ID,
			"region":      "cn",
			"nickname":    sa.Account.Nickname,
			"uid":         sa.Account.UID,
		}
	}
	return map[string]any{"error": "account not found", "auth_index": authIndex}
}

// handleCreditsQuery returns real-time credits for one or all accounts.
// Pass ?auth_index=<idx> to query a single account; omit for all.
// Single-account mode returns full account info (nickname, region, credits,
// exhausted, trial_claimed) so the panel can update one card without
// reloading the entire dashboard.
func handleCreditsQuery(req pluginapi.ManagementRequest) map[string]any {
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Single-account: return one full account row (like dashboard entry).
	if authIndex != "" {
		for _, f := range files {
			if f.AuthIndex != authIndex {
				continue
			}
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				return map[string]any{"accounts": []map[string]any{{
					"auth_index": authIndex, "error": "load auth: " + err.Error(),
				}}}
			}
			data, err := fetchAccountContext(sa)
			acct := map[string]any{
				"auth_index": authIndex,
				"nickname":   sa.Account.Nickname,
				"uid":        sa.Account.UID,
				"region":     "cn",
				"name":       f.Name,
				"label":      f.Label,
				"disabled":   f.Disabled,
				"selected":   getActiveAuthID() == f.ID,
			}
			if err != nil {
				acct["error"] = err.Error()
			} else {
				cr := buildCreditsSummary(&data.Quota)
				plan := data.Plan.Name
				acct["credits"] = cr
				acct["exhausted"] = isCreditsExhausted(cr)
				acct["plan"] = plan
				// Update cache so subsequent dashboard loads see fresh data.
				now := time.Now()
				if cr != nil {
					cr.FetchedAt = now.UTC().Format(time.RFC3339)
				}
				accountCache.Store(f.ID, &accountCacheEntry{
					credits: cr, plan: plan, fetched: now,
				})
			}
			return map[string]any{"accounts": []map[string]any{acct}}
		}
		return map[string]any{"error": "account not found"}
	}
	// All accounts: return simplified list.
	type acctCredits struct {
		AuthIndex string          `json:"auth_index"`
		Nickname  string          `json:"nickname"`
		UID       string          `json:"uid"`
		Credits   *creditsSummary `json:"credits,omitempty"`
		Error     string          `json:"error,omitempty"`
	}
	var out []acctCredits
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			out = append(out, acctCredits{AuthIndex: f.AuthIndex, Error: "load auth: " + err.Error()})
			continue
		}
		cr, err := fetchUserResource(sa)
		ac := acctCredits{AuthIndex: f.AuthIndex, Nickname: sa.Account.Nickname, UID: sa.Account.UID}
		if err != nil {
			ac.Error = err.Error()
		} else {
			ac.Credits = cr
		}
		out = append(out, ac)
	}
	return map[string]any{"accounts": out}
}
