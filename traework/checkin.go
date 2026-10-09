// checkin.go — the daily bonus. POST /checkin claims it for one account or all,
// POST /checkin/config flips the automatic pass, and a twice-a-day ticker claims
// it for every account. The claim is the plugin's only upstream write, and its
// business code (0 ok, 9074 busy) decides whether the panel offers a retry.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	checkinClaimPath = "/trae/api/v2/ug/checkin_credits/claim"
	// checkinBusyCode is upstream's "too many users, try again later". It is why
	// the automatic pass runs twice a day: the evening slot retries whatever the
	// morning slot lost to a busy window.
	checkinBusyCode = 9074
)

// checkinHours is the automatic pass schedule, local time.
var checkinHours = []int{9, 21}

// claimRetryDelay is the first of two short backoffs before a busy claim is
// reported as retryable. A variable so tests do not sleep.
var claimRetryDelay = 3 * time.Second

var (
	// checkinAuto defaults to on: the bonus expires daily, so not claiming it is
	// a straight loss.
	checkinAuto   = true
	checkinAutoMu sync.RWMutex

	checkinLocks sync.Map // auth_index -> *sync.Mutex

	checkinHostAuthList = hostAuthList
	checkinHostAuthGet  = hostAuthGetBundle
	checkinClaimFn      = claimCheckin
)

func checkinAutoEnabled() bool {
	checkinAutoMu.RLock()
	defer checkinAutoMu.RUnlock()
	return checkinAuto
}

// setCheckinAuto flips the automatic pass for this run. It is deliberately not
// persisted: the host offers no plugin-config write callback, so the value in
// config_yaml wins again on the next restart.
func setCheckinAuto(enabled bool) bool {
	checkinAutoMu.Lock()
	defer checkinAutoMu.Unlock()
	checkinAuto = enabled
	return checkinAuto
}

// checkinClaimResult is the claim response: HTTP 200 carrying a business code.
type checkinClaimResult struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Credits int64  `json:"credits"`
}

// claimCheckin posts the claim and classifies the answer. Upstream reports
// failures in the body, not in the status line, so a 200 with code != 0 is a
// failure here.
func claimCheckin(sa *storedAuth) map[string]any {
	headers := ugHeaders(sa)
	var res checkinClaimResult
	// postJSON must run before res is read: passing it as an argument to
	// claimOutcome would copy the struct before the decode fills it in.
	err := postJSON(apiBaseCN+checkinClaimPath, headers, ugBody(), &res)
	return claimOutcome(res, err)
}

// claimOutcome turns a claim response into the panel's verdict. Split out so the
// classification is testable without reaching upstream.
func claimOutcome(res checkinClaimResult, err error) map[string]any {
	if err != nil {
		return map[string]any{"success": false, "message": err.Error()}
	}
	if res.Code != 0 {
		return map[string]any{
			"success":   false,
			"retryable": res.Code == checkinBusyCode,
			"code":      res.Code,
			"message":   checkinMessage(res),
		}
	}
	return map[string]any{"success": true, "message": "签到成功", "credits": res.Credits}
}

func checkinMessage(res checkinClaimResult) string {
	if msg := strings.TrimSpace(res.Message); msg != "" {
		return msg
	}
	if res.Code == checkinBusyCode {
		return "当前参与用户太多，请稍后再试"
	}
	return "签到失败"
}

// checkinLockFor serialises claims per account so two browser tabs, or a manual
// click racing the automatic pass, cannot claim the same day twice.
func checkinLockFor(authIndex string) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// claimCheckinForAccount claims one account's bonus and reports what happened.
// A status read comes first: when it already says today is taken there is
// nothing to gain from the write.
func claimCheckinForAccount(authIndex string) map[string]any {
	out := map[string]any{"auth_index": authIndex}
	sa, _, err := checkinHostAuthGet(authIndex)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["label"] = labelForAuth(sa)

	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()

	if _, checkin, err := cachedCredentials(sa, authIndex, true); err == nil && checkin != nil && checkin.TodayCheckedIn {
		out["success"] = true
		out["already"] = true
		out["message"] = "今日已签到"
		return out
	}

	outcome := checkinClaimFn(sa)
	// 9074 also happens for real (peak hours), so retry a couple of times before
	// reporting it: the window clears within seconds when it is genuine.
	for attempt := 0; attempt < 2 && outcome["retryable"] == true; attempt++ {
		time.Sleep(claimRetryDelay * time.Duration(attempt+1))
		outcome = checkinClaimFn(sa)
	}
	for k, v := range outcome {
		out[k] = v
	}
	if out["success"] == true {
		// A claim moves the balance, so the cached snapshot is stale by
		// definition: read it again and hand the panel the new numbers.
		if credits, checkin, err := cachedCredentials(sa, authIndex, true); err == nil {
			out["credits"] = credits
			out["checkin"] = checkin
		}
	}
	return out
}

// handleCheckinClaim serves POST /checkin {auth_index}. An empty auth_index
// means every account.
func handleCheckinClaim(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	want := strings.TrimSpace(body.AuthIndex)

	files, err := checkinHostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	targets := make([]pluginapi.HostAuthFileEntry, 0, len(files))
	for _, f := range files {
		if want == "" || f.AuthIndex == want {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	results := make([]map[string]any, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = claimCheckinForAccount(f.AuthIndex)
		}(i, f)
	}
	wg.Wait()

	claimed, already, busy, failed := 0, 0, 0, 0
	for _, out := range results {
		switch {
		case out["error"] != nil:
			failed++
		case out["already"] == true:
			already++
		case out["success"] == true:
			claimed++
		case out["retryable"] == true:
			busy++
		default:
			failed++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":   len(targets),
			"claimed": claimed,
			"already": already,
			"busy":    busy,
			"fail":    failed,
		},
	}
}

// handleCheckinConfig serves POST /checkin/config {enabled}.
func handleCheckinConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(req.Body, &body)
	if body.Enabled != nil {
		setCheckinAuto(*body.Enabled)
	}
	return map[string]any{"checkin_auto": checkinAutoEnabled(), "persistent": false}
}

// runAutoCheckin claims every account's bonus. This is the scheduled tick.
func runAutoCheckin() {
	if !checkinAutoEnabled() {
		return
	}
	files, err := checkinHostAuthList()
	if err != nil {
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
			_ = claimCheckinForAccount(f.AuthIndex)
		}()
	}
	wg.Wait()
}

// The scheduler is armed by configure(); a second call is a no-op. It is never
// stopped: the plugin shutdown export runs during the host's own teardown,
// where touching Go sync primitives from the c-shared runtime has crashed the
// process before (the goroutine dies with the host anyway).
var (
	checkinSchedulerStop chan struct{}
	checkinSchedulerMu   sync.Mutex
)

func ensureCheckinScheduler() {
	checkinSchedulerMu.Lock()
	defer checkinSchedulerMu.Unlock()
	if checkinSchedulerStop != nil {
		return
	}
	checkinSchedulerStop = make(chan struct{})
	go checkinSchedulerLoop(checkinSchedulerStop)
}

func checkinSchedulerLoop(stop chan struct{}) {
	for {
		timer := time.NewTimer(time.Until(nextCheckinTime(time.Now())))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			if inCheckinHour(time.Now()) {
				runAutoCheckin()
			}
		}
	}
}

// inCheckinHour reports whether now falls inside one of the scheduled hours.
// The tick can land a hair after the hour, so both bounds are checked rather
// than assuming an exact startup moment.
func inCheckinHour(now time.Time) bool {
	for _, hour := range checkinHours {
		start := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if !now.Before(start) && now.Before(start.Add(time.Hour)) {
			return true
		}
	}
	return false
}

// nextCheckinTime is the earliest scheduled moment after now.
func nextCheckinTime(now time.Time) time.Time {
	var earliest time.Time
	for _, hour := range checkinHours {
		slot := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if !slot.After(now) {
			slot = slot.Add(24 * time.Hour)
		}
		if earliest.IsZero() || slot.Before(earliest) {
			earliest = slot
		}
	}
	return earliest
}
