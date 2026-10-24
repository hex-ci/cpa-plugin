package main

import (
	"testing"
	"time"
)

// The catch-up run keys off the first daily slot, so an earlier slot (a second
// region's reset) must not be missed.
func TestAfterFirstCheckinHour(t *testing.T) {
	old := checkinHours
	checkinHours = []int{10, 21}
	t.Cleanup(func() { checkinHours = old })

	loc := time.Local
	cases := []struct {
		hour int
		want bool
	}{
		{0, false}, {9, false}, {10, true}, {13, true}, {23, true},
	}
	for _, c := range cases {
		now := time.Date(2026, 10, 24, c.hour, 30, 0, 0, loc)
		if got := afterFirstCheckinHour(now); got != c.want {
			t.Fatalf("hour %d: got %v, want %v", c.hour, got, c.want)
		}
	}

	// An unsorted slot list must still resolve to the earliest hour.
	checkinHours = []int{21, 6}
	if !afterFirstCheckinHour(time.Date(2026, 10, 24, 9, 0, 0, 0, loc)) {
		t.Fatal("unsorted hours: 09:00 is after the 06:00 slot")
	}
	if afterFirstCheckinHour(time.Date(2026, 10, 24, 5, 0, 0, 0, loc)) {
		t.Fatal("unsorted hours: 05:00 is before the 06:00 slot")
	}
}
