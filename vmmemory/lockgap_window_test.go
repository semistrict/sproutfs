package vmmemory_test

import (
	"context"
	"errors"
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

// A fault that reads its whole run, and finds its own page in its region's
// layer, reads nothing with that page held while its region is given up. A
// retire holds the region and waits for that page's lock, so the read could
// never take the region again: the fault and the retire waited on each other
// until the fault was cancelled. The region's own page is left bound and
// unmapped by a store whose mapping command was refused.
func TestARunReadHoldingItsRegionsOwnPageLetsARetireRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 4,
			ReadAheadPages: 4})
		ab := f.newBacking(16)
		a, am := f.attach(ab)
		// A fault in another window first, so the store on page 0 follows no
		// recent fault and reads that page alone.
		accessUnder(f.ctx, t, a, am, 8, false)
		am.refuseMap = true
		if err := a.Fault(f.ctx, 0, true); !errors.Is(err, vmmemory.ErrMappingRefused) {
			t.Fatalf("the store whose mapping was refused reports %v", err)
		}
		am.refuseMap = false
		if _, mapped := am.mappedPage(0); mapped {
			t.Fatal("page 0 is mapped after its store's mapping was refused")
		}
		if err := a.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		published, err := f.publishCheckpoint(f.ctx, a, ab)
		if err != nil {
			t.Fatal(err)
		}
		// Where the fault reads the rest of its run, its read waits until the
		// retire has started.
		proceed := make(chan struct{})
		ab.onLoad = func(uint64, int) {
			ab.onLoad = nil
			<-proceed
		}
		ctx, cancel := context.WithCancel(vmmemory.WithStream(f.ctx))
		defer cancel()
		faulted := make(chan error, 1)
		go func() { faulted <- a.Fault(ctx, 0, false) }()
		synctest.Wait()
		retired := make(chan error, 1)
		go func() { retired <- a.Checkpoint().Retire(f.ctx, published) }()
		synctest.Wait()
		close(proceed)
		synctest.Wait()
		select {
		case err := <-faulted:
			if err != nil {
				t.Fatal(err)
			}
		default:
			cancel()
			t.Errorf("the fault and the retire wait on each other; cancelled, the fault reports %v", <-faulted)
		}
		if err := <-retired; err != nil {
			t.Fatal(err)
		}
		for page := range uint64(4) {
			if got := accessUnder(f.ctx, t, a, am, page, false)[0]; got != byte(page+1) {
				t.Fatalf("page %d reads %d, want %d", page, got, page+1)
			}
		}
	})
}
