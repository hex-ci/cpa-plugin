package main

import (
	"sync"
	"testing"
	"time"
)

func TestScheduledActionsAtKeepaliveOnlyHour(t *testing.T) {
	runCheckin, runKeepalive := scheduledActionsFor(time.Date(2026, 8, 18, 22, 0, 0, 0, time.Local))
	if runCheckin {
		t.Fatal("22:00 keepalive tick must not run auto check-in")
	}
	if !runKeepalive {
		t.Fatal("22:00 should run keepalive")
	}
}

func TestScheduledActionsAtCheckinHour(t *testing.T) {
	runCheckin, runKeepalive := scheduledActionsFor(time.Date(2026, 8, 18, 21, 0, 0, 0, time.Local))
	if !runCheckin {
		t.Fatal("21:00 should run auto check-in")
	}
	if runKeepalive {
		t.Fatal("21:00 should not run keepalive")
	}
}

func TestWithCheckinLockSerializesByAuthIndex(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		withCheckinLock("auth-1", func() {
			close(entered)
			<-release
		})
	}()
	<-entered
	locked := make(chan struct{})
	go func() {
		withCheckinLock("auth-1", func() { close(locked) })
	}()
	select {
	case <-locked:
		t.Fatal("second lock holder entered before first released")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("second lock holder did not enter after release")
	}
}
