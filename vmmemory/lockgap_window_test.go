package vmmemory_test

import (
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A fault that reads its whole run takes no page by an identity it located
// before it gave its memory region up. While it waits for a slot, a
// checkpoint may publish a page of its window that the guest stored into and
// that was spilled, which leaves the page held by nothing and named anew.
// Taken by the name located before, the page is its parent's, and the guest
// reads bytes older than its own store.
func TestARunReadTakesNoPageByANameAPublicationReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 3, LogicalPages: 32, DirtyPages: 4,
			ReadAheadPages: 2})
		stream := vmmemory.WithStream(f.ctx)
		ab := f.newBacking(8)
		a, am := f.attach(ab)
		other, om := f.attach(f.newBacking(8))
		// A's run of pages 2 and 3 fills two slots, and the other fork's fault
		// on page 1 takes the last: its root holds page 1 and not page 0.
		accessUnder(stream, t, a, am, 2, false)
		accessUnder(f.ctx, t, other, om, 1, false)
		// A stores into page 1, and its reads of pages 4 and 6 spill that
		// page: the guest's own bytes are in the spill and nowhere else.
		accessUnder(stream, t, a, am, 1, true)[0] = 99
		accessUnder(stream, t, a, am, 4, false)
		accessUnder(stream, t, a, am, 6, false)
		if s := hostStats(t, f); s.Spills != 1 {
			t.Fatalf("A spilled %d pages, want its page 1", s.Spills)
		}
		if _, ok := om.mappedPage(1); !ok {
			t.Fatal("the other fork no longer maps page 1")
		}
		// A's fault on page 0 waits for a slot with its region given up, and
		// a checkpoint publishes page 1 then.
		var checkpointed error
		vmmemory.SetAllocateSeam(t, sync.OnceFunc(func() { checkpointed = f.checkpoint(a, ab) }))
		if err := a.Fault(stream, 0, false); err != nil {
			t.Fatal(err)
		}
		if checkpointed != nil {
			t.Fatal(checkpointed)
		}
		if got := accessUnder(stream, t, a, am, 1, false)[0]; got != 99 {
			t.Fatalf("A's page 1 reads %d after the checkpoint, want its own store of 99", got)
		}
		if got := accessUnder(f.ctx, t, other, om, 1, false)[0]; got != 2 {
			t.Fatalf("the other fork's page 1 reads %d, want its parent's 2", got)
		}
	})
}
