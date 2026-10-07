package vmmemory_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// heldRevokes is a mapping whose revocations run onRevoke first, which is
// where a test holds an eviction between reading a victim's reservation and
// writing the victim's bytes into it.
type heldRevokes struct {
	*mapping
	onRevoke func(page uint64)
}

func (m *heldRevokes) Revoke(ctx context.Context, page uint64) error {
	if m.onRevoke != nil {
		m.onRevoke(page)
	}
	return m.mapping.Revoke(ctx, page)
}

// attachHeld maps a memory region whose revocations a test can hold.
func (f *fixture) attachHeld(b vmmemory.Backing) (*vmmemory.MemoryRegion, *heldRevokes) {
	f.t.Helper()
	m := &heldRevokes{mapping: newMapping(f.a)}
	f.a.mappings = append(f.a.mappings, m.mapping)
	r, err := f.h.Attach(f.ctx, ram(b), m)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		clear(m.pages)
		if err := r.Detach(context.Background()); err != nil {
			f.t.Error(err)
		}
	})
	return r, m
}

// A prefetch's supply answers its requests under the root's lock, before the
// prefetch answers them under the host's when it finishes. A fault that waits
// on the requests in between finds them still listed and sends one that meets
// no read. It used to panic there; it waits on nothing, and its request is
// answered at once.
func TestAWaitOnAPrefetchItsSupplyAnsweredWaitsOnNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		r, m := f.attach(f.slowBacking(8))
		waited, asked := false, false
		var waitErr error
		vmmemory.SetPrefetchUnlockSeam(t, func(uint64) {
			if asked {
				return
			}
			asked = true
			waited, waitErr = vmmemory.WaitOnPrefetchOf(f.ctx, r, 0)
		})
		if err := r.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if !asked || waited || waitErr != nil {
			t.Fatalf("a wait between the supply and the finish: asked %t, waited %t, %v; want asked, and nothing to wait on",
				asked, waited, waitErr)
		}
		for page := range uint64(8) {
			requirePage(t, m, page)
		}
	})
}

// An eviction of a memory region's own page reads the page's reservation,
// revokes the page and then writes its bytes into that reservation. A detach
// of the region in that moment gave the reservation back and freed the page
// under the eviction, which then read a slot that was no longer the page's
// and wrote it into a reservation that was no longer the region's. A detach
// waits for an eviction of its own page to end.
func TestADetachWaitsForAnEvictionOfItsPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 1, 8, 4)
		b := f.newBacking(2)
		b.zero[0] = true
		r, m := f.attachHeld(b)
		// The store makes the region's own page of the hole, the only page
		// the arena holds.
		if _, err := memoryByte(f.ctx, r, m.mapping, 0, new(byte(7))); err != nil {
			t.Fatal(err)
		}
		other := f.newBacking(2)
		other.source = control.Ref{VM: other.owner + "-unrelated", Sequence: 1}
		q, qm := f.attach(other)
		detached := make(chan error, 1)
		held := false
		m.onRevoke = func(uint64) {
			if held {
				return
			}
			held = true
			// The guest has stopped; its region detaches while the eviction
			// holds its page.
			clear(m.pages)
			go func() { detached <- r.Detach(f.ctx) }()
			synctest.Wait()
			select {
			case err := <-detached:
				t.Errorf("the detach returned while an eviction held its page: %v", err)
			default:
			}
		}
		if err := q.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		if !held {
			t.Fatal("the fault evicted nothing of the region")
		}
		requirePage(t, qm, 0)
		if err := <-detached; err != nil {
			t.Fatal(err)
		}
		if s := hostStats(t, f); s.DirtyPages != 0 || s.ResidentPages != 1 {
			t.Fatalf("after the detach the pager holds %d dirty and %d resident pages, want none and the other's one",
				s.DirtyPages, s.ResidentPages)
		}
	})
}
