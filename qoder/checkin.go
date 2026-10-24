// checkin.go implements daily check-in for CN accounts: the manual
// handleManualCheckin endpoint, the 09:00 / 21:00 auto scheduler, and the
// per-account mutex that prevents duplicate check-ins from racing browser
// tabs. CN accounts are excluded — they use one-shot trial claims instead.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	schedulerStop chan struct{}
	schedulerMu   sync.Mutex

	// lastAutoCheckinDay is the calendar day (local) of the last completed
	// scheduled run. A restart after the reset hour would otherwise skip the
	// whole day: the timer only ever points at the next slot.
	lastAutoCheckinDay string
	lastCheckinMu      sync.Mutex
)

func ensureScheduler() {
	schedulerMu.Lock()
	defer schedulerMu.Unlock()
	if schedulerStop != nil {
		return // already running
	}
	schedulerStop = make(chan struct{})
	go schedulerLoop(schedulerStop)
	go catchUpCheckin()
}

// catchUpCheckin runs today's check-in when the process started after the
// reset hour (restart, deploy, crash recovery). Upstream claiming is
// idempotent, so an extra run costs one request and reports "already".
func catchUpCheckin() {
	// Let configure finish publishing config and the host finish loading auths.
	time.Sleep(10 * time.Second)
	if !checkinAutoEnabled() {
		return
	}
	now := time.Now()
	if !afterFirstCheckinHour(now) {
		return
	}
	lastCheckinMu.Lock()
	done := lastAutoCheckinDay == now.Format("2006-01-02")
	lastCheckinMu.Unlock()
	if done {
		return
	}
	hostLogf("info", "checkin catch-up: today's window was missed, running now")
	runAutoCheckin()
}

// afterFirstCheckinHour reports whether the earliest daily slot has passed.
func afterFirstCheckinHour(now time.Time) bool {
	if len(checkinHours) == 0 {
		return false
	}
	first := checkinHours[0]
	for _, h := range checkinHours[1:] {
		if h < first {
			first = h
		}
	}
	return now.Hour() >= first
}

// Note: there is deliberately no stopCheckinScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

func nextCheckinTime(now time.Time) time.Time {
	var earliest time.Time
	// Consider both checkin and keepalive schedules so the timer wakes up for
	// whichever fires first (e.g. 21:00 checkin vs 22:00 keepalive → 21:00 wins,
	// then 22:00 keepalive fires on the next tick).
	hours := append([]int{}, checkinHours...)
	hours = append(hours, keepaliveHours...)
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

func schedulerLoop(stop chan struct{}) {
	first := true
	for {
		next := nextCheckinTime(time.Now())
		if first {
			hostLogf("info", "checkin scheduler armed, next tick "+next.Format("2006-01-02 15:04"))
			first = false
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			hostLogf("info", "checkin tick at "+time.Now().Format("15:04"))
			runAutoCheckin()
			// Fire keepalive if the current tick falls within its scheduled
			// window (e.g. 22:00 keepalive fires on the 22:00 tick even though
			// the previous checkin tick was 21:00).
			if shouldRunKeepaliveNow(time.Now()) {
				runTokenKeepalive()
			}
		}
	}
}

// runAutoCheckin is the scheduled lifecycle tick (09:00 / 21:00).
// CN: optional daily check-in, then reconcile (disable exhausted / reenable after credits).
// CN: no auto trial (one-shot claim is manual only); reconcile may delete exhausted auths.
//
// v0.6.31: per-account work runs concurrently (sem=4) — was serial, so N accounts
// meant 3N serial HTTP round-trips on the billing API. Matches the pattern used
// by buildDashboardEx and handleManualCheckin.
func runAutoCheckin() {
	doCheckin := checkinAutoEnabled()
	// Lifecycle may still run when check-in is off (credit gate).
	if !doCheckin && !lifecycleEnabled() {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		hostLogf("warn", "checkin: cannot list auth files: "+err.Error())
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processAutoCheckinAccount(f, doCheckin)
		}()
	}
	wg.Wait()
	lastCheckinMu.Lock()
	lastAutoCheckinDay = time.Now().Format("2006-01-02")
	lastCheckinMu.Unlock()
	hostLogf("info", fmt.Sprintf("checkin run finished: %d account(s), checkin=%v", len(files), doCheckin))
}

// processAutoCheckinAccount handles one account's scheduled tick. Extracted so
// runAutoCheckin can fan out per-account work without duplicating logic.
func processAutoCheckinAccount(f pluginapi.HostAuthFileEntry, doCheckin bool) {
	// A-24: only fetch sa when needed (checkin). For lifecycle-only paths,
	// let reconcileOneAccount do the single hostAuthGetBundle internally.
	if doCheckin {
		// Share the manual endpoint's implementation instead of keeping a
		// second, older claim flow here: this path used to call the legacy
		// check-in directly and required ci.Active, so accounts whose daily
		// credits come from the activity platform (active=false, empty list)
		// were skipped and never checked in. checkinOneAccount runs the
		// campaign flow first, treats 409 as "already", and marks the account.
		out := checkinOneAccount(f)
		reason, _ := out["reason"].(string)
		if reason == "" && out["error"] == nil {
			// Credits may have been granted; refresh the cached balance so the
			// panel does not wait for the async reconcile pass.
			if sa, err := hostAuthGet(f.AuthIndex); err == nil {
				if cr2, crErr := fetchUserResource(sa); crErr == nil && cr2 != nil {
					if v, ok := accountCache.Load(f.ID); ok {
						if prev, ok2 := v.(*accountCacheEntry); ok2 {
							fresh := *prev
							fresh.credits = cr2
							fresh.fetched = time.Now()
							accountCache.Store(f.ID, &fresh)
						}
					}
				}
			}
		}
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
		}
		return
	}
	// Lifecycle-only (checkin off): reconcile handles its own get.
	if lifecycleEnabled() {
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

// handleManualCheckin checks in one account (auth_index) or all Qoder
// accounts. Unlike the WorkBuddy three-phase classify/execute/summarize flow,
// Qoder's checkin is a simple Bearer GET+POST — we run it directly per
// account under an 8s timeout, no classify stage.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	t0 := time.Now()
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	// Run checkins concurrently (one goroutine per account, bounded).
	type result struct {
		idx int
		out map[string]any
	}
	outCh := make(chan result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outCh <- result{idx: i, out: checkinOneAccount(f)}
		}(i, f)
	}
	wg.Wait()
	close(outCh)

	results := make([]map[string]any, len(targets))
	for r := range outCh {
		results[r.idx] = r.out
	}
	successN, alreadyN, failN := 0, 0, 0
	for _, r := range results {
		if r["error"] != nil {
			failN++
			continue
		}
		if r["skipped"] == true {
			alreadyN++
			continue
		}
		if r["success"] == true {
			successN++
		} else {
			failN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":      len(targets),
			"success":    successN,
			"already":    alreadyN,
			"fail":       failN,
			"elapsed_ms": time.Since(t0).Milliseconds(),
		},
	}
}

// checkinOneAccount performs the actual GET status + POST claim flow for one
// account. 8s overall budget — never blocks past that.
func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	out := map[string]any{
		"auth_index": f.AuthIndex,
		"name":       f.Name,
	}
	mu := checkinLockFor(f.AuthIndex)
	mu.Lock()
	defer mu.Unlock()

	sa, err := hostAuthGet(f.AuthIndex)
	if err != nil {
		out["error"] = "get auth: " + err.Error()
		return out
	}
	out["nickname"] = sa.Account.Nickname

	// The daily credits now come from the activity platform; the legacy sash
	// check-in answers DISABLED. Try the campaign flow first and only fall back
	// to the legacy endpoints when it reports nothing to do.
	if currentConfig().CampaignCheckin {
		res, cErr := runCampaignCheckin(sa)
		switch {
		case cErr != nil && !errors.Is(cErr, errCampaignsUnsupported):
			out["error"] = "campaign: " + cErr.Error()
			return out
		case cErr == nil && res.Earned > 0:
			markClaimedToday(sa)
			out["success"] = true
			out["reward_credits"] = res.Earned
			out["message"] = fmt.Sprintf("签到成功，+%d credits", res.Earned)
			if cr, crErr := fetchUserResource(sa); crErr == nil && cr != nil {
				out["credits"] = cr
			}
			return out
		case cErr == nil && res.Blocked:
			out["success"] = true
			out["skipped"] = true
			out["reason"] = "same-machine"
			out["message"] = "同机已有账号领过今日额度"
			return out
		case cErr == nil && res.Claimed:
			markClaimedToday(sa)
			out["success"] = true
			out["skipped"] = true
			out["reason"] = "already"
			out["message"] = "今日已领取"
			return out
		}
	}

	// Step 1: GET status (5s budget).
	ci, err := fetchCheckinStatus(sa)
	if err != nil {
		out["error"] = "status: " + err.Error()
		return out
	}
	if ci.TodayCheckedIn {
		// Status already reports the claim; record it locally so the panel keeps
		// showing "已签到" even after the activity disappears from the list.
		markClaimedToday(sa)
		out["success"] = true
		out["skipped"] = true
		out["reason"] = "already"
		out["message"] = "今日已签到"
		out["streak_days"] = ci.StreakDays
		out["total_credits"] = ci.TotalCredits
		return out
	}

	// Step 2: POST claim (5s budget).
	res, err := performCheckinCall(sa)
	if err != nil {
		out["error"] = "claim: " + err.Error()
		return out
	}
	// Qoder returns {"result":"ALREADY_CLAIMED"} with HTTP 409, and the
	// campaign claim answers {"errorCode":"AlreadyExists"} — both mean today's
	// reward is already gone, not that the call failed.
	errorCode, _ := res["errorCode"].(string)
	if result, _ := res["result"].(string); result == "ALREADY_CLAIMED" || strings.EqualFold(errorCode, "AlreadyExists") {
		out["success"] = true
		out["skipped"] = true
		out["reason"] = "already"
		out["message"] = "今日已签到"
		markClaimedToday(sa)
		if rc, ok := res["rewardCredits"].(float64); ok {
			out["reward_credits"] = int64(rc)
		}
		return out
	}
	if success, _ := res["success"].(bool); success {
		markClaimedToday(sa)
		out["success"] = true
		if rc, ok := res["rewardCredits"].(float64); ok {
			out["reward_credits"] = int64(rc)
		}
		out["message"] = "签到成功"
		// Refresh the credits snapshot so the panel shows the post-checkin
		// balance immediately (check-in grants new credits). Best-effort:
		// a failure here must not flip the check-in result to error.
		if cr, crErr := fetchUserResource(sa); crErr == nil && cr != nil {
			out["credits"] = cr
		}
		return out
	}
	out["success"] = false
	out["message"] = res
	return out
}

func checkinLockFor(authIndex string) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// pruneCheckinLocks removes lock entries for auth indices that no longer
// exist in hostAuthList. Call after dashboard prune.
// Lock keys are auth_index (used for host RPC), so live map needs auth_index too.
func pruneCheckinLocks() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{} // checkinLockFor uses auth_index as key
	}
	checkinLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			checkinLocks.Delete(key)
		}
		return true
	})
}
