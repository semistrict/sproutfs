package vmmemory

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A tally counts every fault and the time the guest waited for them, and keeps
// the fault read first, even when a later one is served before it.
func TestAGuestFaultTallyKeepsTheFaultReadFirst(t *testing.T) {
	var tally faultTally
	if got := tally.report(); got != (GuestFaults{}) {
		t.Fatalf("an empty tally reports %+v", got)
	}
	start := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	tally.observe(start.Add(3*time.Millisecond), 2*time.Millisecond)
	tally.observe(start.Add(time.Millisecond), 9*time.Millisecond)
	tally.observe(start.Add(5*time.Millisecond), time.Millisecond)
	want := GuestFaults{Count: 3, Waited: 12 * time.Millisecond, First: start.Add(time.Millisecond),
		FirstWaited: 9 * time.Millisecond}
	if got := tally.report(); got != want {
		t.Fatalf("the tally reports %+v, want %+v", got, want)
	}
}

// A session serves its faults on several workers at once, so the tally is
// counted from all of them.
func TestAGuestFaultTallyCountsFaultsServedTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var tally faultTally
		start := time.Now()
		var wg sync.WaitGroup
		for worker := range 8 {
			wg.Go(func() {
				for fault := range 100 {
					at := start.Add(time.Duration(worker*100+fault+1) * time.Microsecond)
					tally.observe(at, time.Microsecond)
				}
			})
		}
		wg.Wait()
		want := GuestFaults{Count: 800, Waited: 800 * time.Microsecond, First: start.Add(time.Microsecond),
			FirstWaited: time.Microsecond}
		if got := tally.report(); got != want {
			t.Fatalf("the tally reports %+v, want %+v", got, want)
		}
	})
}
