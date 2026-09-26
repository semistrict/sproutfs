package handover

import (
	"slices"
	"testing"
	"time"
)

var start = time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)

// The waits double from the first pause to the largest, and stop at the
// source's hold: a look that would come after the source has given the pages
// up is not taken.
func TestWaitsDoubleUntilTheSourceStopsHolding(t *testing.T) {
	attempts := Default.Begin(start, time.Minute, "host-1")
	now := start
	var waits []time.Duration
	for {
		attempts.Failed()
		wait, ok := attempts.Wait(t.Context(), now)
		if !ok {
			break
		}
		waits = append(waits, wait)
		now = now.Add(wait)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		15 * time.Second, 15 * time.Second}
	if !slices.Equal(waits, want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
}

// A source that says nothing about how long it holds its pages promises
// nothing, so its handoff is tried once.
func TestAHandoffWithNoHoldIsTriedOnce(t *testing.T) {
	attempts := Default.Begin(start, 0, "host-1")
	attempts.Failed()
	if wait, ok := attempts.Wait(t.Context(), start); ok {
		t.Fatalf("a handoff with no hold waits %s for another attempt", wait)
	}
}

// One destination gets two attempts in a row. Then the next goes to the host
// that has failed least, and back only when every other one has failed more.
func TestADestinationThatKeepsFailingIsLeftForAnother(t *testing.T) {
	attempts := Default.Begin(start, time.Hour, "host-1")
	hosts := []string{"host-1", "host-2", "host-3"}
	var tried []string
	for range 7 {
		tried = append(tried, attempts.At())
		attempts.Failed()
		next, ok := attempts.Next(t.Context(), hosts)
		if !ok {
			t.Fatal("three hosts could take the VM and none was chosen")
		}
		if next != attempts.At() {
			t.Fatalf("Next chose %s and At says %s", next, attempts.At())
		}
	}
	want := []string{"host-1", "host-1", "host-2", "host-2", "host-3", "host-3", "host-1"}
	if !slices.Equal(tried, want) {
		t.Fatalf("tried %v, want %v", tried, want)
	}
}

// With nowhere else to go the same destination is tried again, and a
// destination that can no longer take the VM is left at once.
func TestTheOnlyDestinationIsKeptAndAnUnavailableOneLeft(t *testing.T) {
	attempts := Default.Begin(start, time.Hour, "host-1")
	for range 3 {
		attempts.Failed()
		if next, _ := attempts.Next(t.Context(), []string{"host-1"}); next != "host-1" {
			t.Fatalf("the only destination was left for %q", next)
		}
	}
	attempts.Failed()
	if next, _ := attempts.Next(t.Context(), []string{"host-2"}); next != "host-2" {
		t.Fatalf("a destination that cannot take the VM was kept: next is %q", next)
	}
	if _, ok := attempts.Next(t.Context(), nil); ok {
		t.Fatal("a destination was chosen when no host can take the VM")
	}
}
