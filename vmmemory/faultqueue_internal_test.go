package vmmemory

import (
	"testing"
	"time"
)

// A page is queued once however many of its accesses trapped: a store among
// them makes it a write fault, and its delay is measured from the first. A
// queue full of other pages refuses a new one.
func TestAFaultQueueHoldsAPageOnce(t *testing.T) {
	q := newFaultQueue(2)
	first := time.Unix(1_000_000, 0)
	for _, access := range []struct {
		page  uint64
		write bool
		at    time.Time
	}{{7, false, first}, {7, true, first.Add(time.Second)}, {8, false, first}} {
		if !q.add(access.page, access.write, access.at) {
			t.Fatalf("a queue of two refused page %d", access.page)
		}
	}
	if q.add(9, false, first) {
		t.Fatal("a queue holding two pages took a third")
	}
	never := func(uint64, bool) bool { t.Fatal("asked whether a fault is repeated"); return false }
	taken := map[uint64]queuedFault{}
	for range 2 {
		page, entry, repeat, ok := q.take(func(uint64, bool) bool { return false })
		if !ok || repeat {
			t.Fatalf("took page %d repeated=%t ok=%t, want a queued fault that is not repeated", page, repeat, ok)
		}
		taken[page] = entry
	}
	if want := (queuedFault{write: true, at: first}); taken[7] != want {
		t.Fatalf("page 7 was queued as %+v, want %+v", taken[7], want)
	}
	// Both pages are being served, so nothing is left to take.
	if page, _, _, ok := q.take(never); ok {
		t.Fatalf("took page %d, which a worker is serving", page)
	}
}

// Two vCPUs faulting one page at once are two faults. The second finds the
// page mapped, but it is the twin of a fault that changed something, so the
// session is not charged for it. Only such a fault has a free twin: the twin of
// a twin, or of a repeated fault, is charged like any other.
func TestOnlyAFaultThatChangesSomethingHasAFreeTwin(t *testing.T) {
	q := newFaultQueue(1)
	now := time.Unix(1_000_000, 0)
	mapped := false
	repeated := func(uint64, bool) bool { return mapped }
	serve := func(what string, want queuedFault, wantRepeat bool) {
		t.Helper()
		page, entry, repeat, ok := q.take(repeated)
		if !ok || page != 3 || entry != want || repeat != wantRepeat {
			t.Fatalf("%s: took page %d as %+v repeated=%t ok=%t, want page 3 as %+v repeated=%t",
				what, page, entry, repeat, ok, want, wantRepeat)
		}
		// Another thread traps on the page while this fault is served.
		if !q.add(3, false, now) {
			t.Fatalf("%s: the queue refused the page it is serving", what)
		}
		mapped = true
		if !q.finish(3) {
			t.Fatalf("%s: the page that faulted while served is not queued again", what)
		}
	}
	if !q.add(3, false, now) {
		t.Fatal("an empty queue refused a page")
	}
	serve("the first fault, which maps the page", queuedFault{at: now}, false)
	serve("its twin", queuedFault{at: now, twin: true}, false)
	serve("the fault that trapped while the twin was served", queuedFault{at: now}, true)
	serve("the fault that trapped while a repeated fault was served", queuedFault{at: now}, true)
}
