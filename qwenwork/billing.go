// billing.go owns the upstream billing surface: account context (user + plan +
// credits). qwenwork reports credits for BOTH enterprise and personal accounts
// through /api/v1/adapter/user/account-context?include=user,plan,quota — the
// older /api/v2/quota/usage returns null quota for personal (free) accounts,
// which is why free-account credits looked "empty". account-context gives every
// account a quota.remaining (and total/used for enterprise, null totals for free).
//
// qwenwork has NO daily check-in (both enterprise and personal accounts return
// 404 on /sash/api/v1/me/daily-check-in/*), so check-in code stays removed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

func billingHeaders(req *http.Request, sa *storedAuth) {
	// qwenwork account-context authenticates with the device token as a plain
	// Bearer. No COSY signing (this is an /api/v1 route).
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", clientUA)
}

// accountContextResponse mirrors GET /api/v1/adapter/user/account-context.
type accountContextResponse struct {
	Code string             `json:"code"`
	Data accountContextData `json:"data"`
}

type accountContextData struct {
	User  accountContextUser  `json:"user"`
	Plan  accountContextPlan  `json:"plan"`
	Quota accountContextQuota `json:"quota"`
}

type accountContextUser struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Email    string `json:"email"`
	IsBiz    bool   `json:"is_biz"`
}

type accountContextPlan struct {
	PID               string `json:"pid"`
	Name              string `json:"name"`
	UserType          string `json:"user_type"`
	IsPersonalVersion bool   `json:"is_personal_version"`
}

// accountContextQuota: enterprise accounts carry total/used/remaining; personal
// free accounts carry only remaining (total/used are JSON null → nil pointers).
type accountContextQuota struct {
	Total     *float64 `json:"total"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	Exceeded  bool     `json:"exceeded"`
}

// fetchAccountContext fetches user + plan + credits in one round-trip.
func fetchAccountContext(sa *storedAuth) (*accountContextData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := upstreamBaseCN + "/api/v1/adapter/user/account-context?include=user,plan,quota"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("account-context http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var r accountContextResponse
	if err := json.Unmarshal(resp.Body, &r); err != nil {
		return nil, fmt.Errorf("account-context parse: %w", err)
	}
	if r.Code != "" && r.Code != "ok" {
		return nil, fmt.Errorf("account-context code=%s", r.Code)
	}
	return &r.Data, nil
}

func roundPtr(v *float64) int64 {
	if v == nil {
		return 0
	}
	return int64(math.Round(*v))
}

// buildCreditsSummary maps an account-context quota block onto creditsSummary.
func buildCreditsSummary(q *accountContextQuota) *creditsSummary {
	sum := &creditsSummary{
		TotalRemain: roundPtr(q.Remaining),
		TotalUsed:   roundPtr(q.Used),
		TotalSize:   roundPtr(q.Total),
		FreeTier:    q.Total == nil,
		Exceeded:    q.Exceeded,
		Packages:    []packageSummary{},
	}
	// Only enterprise (fixed-cap) accounts expose a package summary; free
	// accounts have an unbounded pool, so there is nothing to enumerate.
	if q.Total != nil {
		sum.PackCount = 1
		sum.Packages = append(sum.Packages, packageSummary{
			Name:   "额度",
			Remain: sum.TotalRemain,
			Used:   sum.TotalUsed,
			Size:   sum.TotalSize,
		})
	}
	return sum
}

// fetchUserResource returns the credits summary derived from account-context.
func fetchUserResource(sa *storedAuth) (*creditsSummary, error) {
	data, err := fetchAccountContext(sa)
	if err != nil {
		return nil, err
	}
	return buildCreditsSummary(&data.Quota), nil
}

// isCreditsExhausted is the shared "耗尽" definition for panel + scheduler.
// The upstream quota.exceeded flag is authoritative (esp. for free accounts,
// which have no total to derive exhaustion from). Missing data is NOT exhausted.
func isCreditsExhausted(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	if cr.Exceeded {
		return true
	}
	if cr.FreeTier {
		return false // only known signal is remaining; exceeded above covers expiry
	}
	if cr.TotalRemain > 0 {
		return false
	}
	if cr.TotalUsed > 0 || cr.TotalSize > 0 {
		return true
	}
	return len(cr.Packages) > 0
}
