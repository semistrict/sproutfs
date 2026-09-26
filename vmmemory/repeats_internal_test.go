package vmmemory

import (
	"testing"
	"time"
)

// A session's repeated faults are served at once up to the burst, then one an
// interval, and an idle budget fills back up to the burst and no further.
func TestRepeatedFaultsArePacedPastTheirBurst(t *testing.T) {
	var b repeatBudget
	start := time.Unix(1_000_000, 0)
	for n := range repeatBurst {
		if wait := b.spend(start); wait != 0 {
			t.Fatalf("repeated fault %d of a full budget waits %s, want none", n+1, wait)
		}
	}
	// The faults past the burst arrive together, as a session's fault workers
	// bring them, and are served one an interval.
	for n := 1; n <= 3; n++ {
		if wait := b.spend(start); wait != time.Duration(n)*repeatInterval {
			t.Fatalf("repeated fault %d past the burst waits %s, want %s", n, wait, time.Duration(n)*repeatInterval)
		}
	}
	// Once those three are served, the budget holds nothing: a fault an
	// interval later is served at once, and the one after it waits a whole
	// interval.
	later := start.Add(4 * repeatInterval)
	if wait := b.spend(later); wait != 0 {
		t.Fatalf("a repeated fault an interval after the last one served waits %s, want none", wait)
	}
	if wait := b.spend(later); wait != repeatInterval {
		t.Fatalf("a second repeated fault in that interval waits %s, want %s", wait, repeatInterval)
	}
	// An idle budget holds the burst again, and no more than the burst,
	// however long it was idle.
	idle := later.Add(time.Hour)
	for n := range repeatBurst {
		if wait := b.spend(idle); wait != 0 {
			t.Fatalf("repeated fault %d after an idle hour waits %s, want none", n+1, wait)
		}
	}
	if wait := b.spend(idle); wait != repeatInterval {
		t.Fatalf("the repeated fault past the burst after an idle hour waits %s, want %s", wait, repeatInterval)
	}
}
