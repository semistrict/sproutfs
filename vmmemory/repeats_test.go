package vmmemory_test

import (
	"testing"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A fault is repeated exactly when the process already maps its page for the
// access that trapped: for a read, mapped or zero-mapped, and for a store,
// mapped writable. Loads, stores into shared or sealed pages, and faults after
// an eviction are never repeated, so pacing repeated faults never slows a guest
// that is bringing its memory in, writing it or faulting it back under
// pressure.
func TestAFaultIsRepeatedExactlyWhenItsPageIsMappedForItsAccess(t *testing.T) {
	const pages = 4
	f := newFixture(t, 2, 8, 4)
	b := f.newBacking(pages)
	b.zero[3] = true
	r, m := f.attach(b)
	require := func(when string) {
		t.Helper()
		m.arena.mu.Lock()
		defer m.arena.mu.Unlock()
		for page := range uint64(pages) {
			for _, write := range []bool{false, true} {
				p, ok := m.pages[page]
				want := ok && (!write || p.writable)
				if got := vmmemory.Repeated(r, page, write); got != want {
					t.Fatalf("%s, a fault on page %d (write=%t) is repeated=%t, want %t: the process maps it %+v (mapped=%t)",
						when, page, write, got, want, p, ok)
				}
			}
		}
	}
	require("before any fault")
	access(t, r, m, 0, false)
	require("after a read")
	access(t, r, m, 3, false)
	require("after a read of a hole")
	access(t, r, m, 1, false)
	require("after a read that fills the arena")
	// The arena holds two pages and both are mapped, so a third evicts one.
	access(t, r, m, 2, false)
	if s, err := f.h.Stats(t.Context()); err != nil || s.Evictions != 1 || s.IdleDrops != 0 {
		t.Fatalf("the read of a third page evicted %d mapped pages and dropped %d idle ones, want 1 and 0: %v",
			s.Evictions, s.IdleDrops, err)
	}
	require("after a read that evicts")
	access(t, r, m, 2, true)
	require("after a store into a shared page")
	f.mustCheckpoint(r, b)
	require("after a checkpoint")
	access(t, r, m, 2, true)
	require("after a store into a published page")
}
