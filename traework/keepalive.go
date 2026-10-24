// keepalive.go rotates access tokens on a daily pass. TraeWork refreshes only on
// demand (the host asks the plugin around request handling), so an account the
// panel alone touches would sit expired and answer 401 until someone re-logs in.
// The pass is deliberately conservative: it rotates a token only when it is
// within the lead time of expiry, because every exchange invalidates the old
// refresh token and there is no gain in rotating a two-week token every night.
package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// keepaliveHour is the local hour of the daily pass. Evening, like the
	// sibling provider plugins, so a rotation never lands inside the morning
	// check-in window.
	keepaliveHour = 22
	// keepaliveLeadTime is how early a token is rotated. The exchange sends the
	// current access token in x-cloudide-token, so it must still be valid when
	// it runs; a day of headroom also survives a host that is down at 22:00.
	keepaliveLeadTime = 24 * time.Hour
	// keepaliveCatchUpDelay lets configure() finish before the first pass, which
	// would otherwise race the plugin's own registration.
	keepaliveCatchUpDelay = 15 * time.Second
)

// keepaliveRun is what the last pass did; the management endpoint reports it.
type keepaliveRun struct {
	At        time.Time `json:"at"`
	Refreshed int       `json:"refreshed"`
	Skipped   int       `json:"skipped"`
	Failed    int       `json:"failed"`
	Detail    string    `json:"detail,omitempty"`
}

var (
	keepaliveEnabled atomic.Bool
	keepaliveMu      sync.Mutex
	keepaliveStop    chan struct{}
	keepaliveLast    atomic.Value // keepaliveRun
	// keepaliveRefreshFn is the seam tests replace: a real exchange needs a
	// device key and a live upstream.
	keepaliveRefreshFn    = refreshStoredAuth
	keepaliveAccountsFn   = hostAuthList
	keepaliveAccountGetFn = func(authIndex string) (*storedAuth, error) {
		sa, _, err := hostAuthGetBundle(authIndex)
		return sa, err
	}
)

func init() {
	keepaliveEnabled.Store(true)
}

func tokenKeepaliveEnabled() bool { return keepaliveEnabled.Load() }

func setKeepaliveEnabled(enabled bool) { keepaliveEnabled.Store(enabled) }

// ensureKeepaliveScheduler arms the daily pass once; configure() runs on every
// register/reconfigure and must not pile up goroutines.
func ensureKeepaliveScheduler() {
	keepaliveMu.Lock()
	defer keepaliveMu.Unlock()
	if keepaliveStop != nil {
		return
	}
	stop := make(chan struct{})
	keepaliveStop = stop
	go keepaliveLoop(stop)
}

// keepaliveLoop wakes at the scheduled hour, and once shortly after startup so a
// token that expired while the host was down heals without any request.
func keepaliveLoop(stop chan struct{}) {
	catchUp := time.NewTimer(keepaliveCatchUpDelay)
	select {
	case <-stop:
		catchUp.Stop()
		return
	case <-catchUp.C:
		if tokenKeepaliveEnabled() {
			if run := runKeepalive(); run.Refreshed > 0 || run.Failed > 0 {
				hostLogInfo("traework: keepalive catch-up", map[string]any{
					"refreshed": run.Refreshed,
					"skipped":   run.Skipped,
					"failed":    run.Failed,
				})
			}
		}
	}
	for {
		timer := time.NewTimer(time.Until(nextKeepaliveTime(time.Now())))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			if !tokenKeepaliveEnabled() {
				continue
			}
			run := runKeepalive()
			hostLogInfo("traework: keepalive pass finished", map[string]any{
				"refreshed": run.Refreshed,
				"skipped":   run.Skipped,
				"failed":    run.Failed,
			})
		}
	}
}

// nextKeepaliveTime is the next scheduled rotation moment after now.
func nextKeepaliveTime(now time.Time) time.Time {
	slot := time.Date(now.Year(), now.Month(), now.Day(), keepaliveHour, 0, 0, 0, now.Location())
	if !slot.After(now) {
		slot = slot.Add(24 * time.Hour)
	}
	return slot
}

// shouldRotate reports whether an account's token is close enough to expiry to
// be worth rotating. A record with no expiry is left alone: without a deadline
// there is nothing to keep alive.
func shouldRotate(sa *storedAuth, now time.Time) bool {
	if sa == nil || strings.TrimSpace(sa.RefreshToken) == "" || sa.ExpiresAt <= 0 {
		return false
	}
	return !time.Unix(sa.ExpiresAt, 0).After(now.Add(keepaliveLeadTime))
}

// runKeepalive performs one pass over the accounts and records the outcome.
func runKeepalive() keepaliveRun {
	run := keepaliveRun{At: time.Now()}
	files, err := keepaliveAccountsFn()
	if err != nil {
		run.Detail = "list auth files: " + err.Error()
		keepaliveLast.Store(run)
		return run
	}
	now := time.Now()
	for _, f := range files {
		if f.Disabled {
			run.Skipped++
			continue
		}
		sa, err := keepaliveAccountGetFn(f.AuthIndex)
		if err != nil || sa == nil {
			run.Failed++
			run.Detail = "read auth " + f.Name + ": " + errorText(err)
			continue
		}
		if !shouldRotate(sa, now) {
			run.Skipped++
			continue
		}
		if err := keepaliveRefreshFn(sa); err != nil {
			run.Failed++
			// The management response is HTML-escaped by the host, so a raw
			// upstream body would arrive as &#34;…&#34; noise. One sentence is enough
			// to tell "re-login needed" from "upstream hiccup".
			run.Detail = "refresh " + f.Name + ": " + keepaliveFailureText(err)
			continue
		}
		run.Refreshed++
	}
	keepaliveLast.Store(run)
	return run
}

// keepaliveFailureText summarises why a rotation failed.
func keepaliveFailureText(err error) string {
	status := upstreamStatusOfError(err)
	switch {
	case status == 401 || status == 403:
		return "refresh token rejected, re-login required"
	case status >= 500:
		return "upstream error, will retry tomorrow"
	case status > 0:
		return fmt.Sprintf("upstream returned %d", status)
	default:
		return "network unreachable"
	}
}

func errorText(err error) string {
	if err == nil {
		return "no record"
	}
	return redactSecrets(err.Error())
}

// lastKeepaliveRun reports the last pass, or nil when none has run yet.
func lastKeepaliveRun() (keepaliveRun, bool) {
	value := keepaliveLast.Load()
	if value == nil {
		return keepaliveRun{}, false
	}
	run, ok := value.(keepaliveRun)
	return run, ok
}

// keepaliveStatus is the management payload: enough to tell whether the pass is
// armed, when it last ran and what it did.
func keepaliveStatus() map[string]any {
	out := map[string]any{
		"enabled":    tokenKeepaliveEnabled(),
		"schedule":   []string{fmt.Sprintf("%02d:00", keepaliveHour)},
		"lead_hours": int(keepaliveLeadTime / time.Hour),
		"next_run":   nextKeepaliveTime(time.Now()).Format(time.RFC3339),
	}
	if run, ok := lastKeepaliveRun(); ok {
		out["last_run"] = map[string]any{
			"at":        run.At.Format(time.RFC3339),
			"refreshed": run.Refreshed,
			"skipped":   run.Skipped,
			"failed":    run.Failed,
			"detail":    run.Detail,
		}
	} else {
		out["last_run"] = nil
	}
	return out
}
