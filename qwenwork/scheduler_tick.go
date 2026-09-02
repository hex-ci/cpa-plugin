// scheduler_tick.go owns the daily background timer. qwenwork has no check-in,
// so the only scheduled work is the 22:00 token keepalive, which also carries
// the lifecycle reconcile (disable exhausted / re-enable after quota reset).
package main

import (
	"sync"
	"time"
)

var (
	schedulerStop chan struct{}
	schedulerMu   sync.Mutex
)

func ensureScheduler() {
	schedulerMu.Lock()
	defer schedulerMu.Unlock()
	if schedulerStop != nil {
		return // already running
	}
	schedulerStop = make(chan struct{})
	go schedulerLoop(schedulerStop)
}

// Note: there is deliberately no stop function. The plugin shutdown export is
// a no-op (see cliproxyPluginShutdown) because the host invokes it during its
// own runtime teardown, where touching Go sync primitives from the plugin's
// c-shared runtime caused SIGSEGV on every restart.

func nextTickTime(now time.Time) time.Time {
	var earliest time.Time
	for _, h := range keepaliveHours {
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
	for {
		next := nextTickTime(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			runTokenKeepalive()
			if lifecycleEnabled() {
				reconcileAllAccounts(true)
			}
		}
	}
}
