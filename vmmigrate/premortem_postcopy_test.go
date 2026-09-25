package vmmigrate_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// The pre-mortem of the GCE soak's remote fan-out. The soak forks one parent
// onto the other host twice in a round and then, as soon as each child's agent
// answers, reads every byte of that child's memory and its disk back. That is a
// check running while the source is still streaming the rest of what it holds,
// with several memory regions pulling from one page source at the same time, over a
// per-peer budget a busy host spends its life at.

// premortemStreamDeadline bounds a post-copy that should finish. It is
// simulated time: a destination queueing behind a busy source costs this test
// microseconds of wall time, and one that will never be served costs it this
// much simulated time and then says so instead of hanging.
const premortemStreamDeadline = 2 * time.Minute

// premortemSource is a page source with a per-peer connection budget of the
// caller's choosing, which is what decides whether every memory region of a received
// VM can be served at once.
func premortemSource(t *testing.T, m *migration, connections int) *vmmigrate.PageSource {
	t.Helper()
	source, err := vmmigrate.NewPageSource(t.Context(), vmmigrate.SourceConfig{
		PageSize: pageSize, MaxPagesPerRequest: 2, MaxConnectionsPerPeer: connections,
		Network: m.cluster.runtime.Network(), Address: platform.Address("busy-source")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

// TestPremortemAPostCopyFinishesUnderAPerPeerConnectionBudget: a source at its
// per-peer connection budget must slow a destination down, never stop it.
//
// The pages no checkpoint holds exist nowhere else, so a memory region that cannot get
// a connection asks for one for ever. What it is waiting for is held by the
// memory regions that were served first: a memory region pools every connection it dialled
// and gives none of them back until the whole receive is over. A budget below
// what the earlier memory regions pool is therefore a post-copy that never finishes, a
// fork call that never returns and a parent sealed for good.
func TestPremortemAPostCopyFinishesUnderAPerPeerConnectionBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		// The writes since that checkpoint are the pages only the parent has,
		// which is what the child must pull before its root can be published.
		for page := range uint64(4) {
			m.machine.write("ram0", page)
			m.machine.write("disk", page)
		}
		at := m.machine.snapshot()
		// Fewer connections than the first memory region alone can pool, which is what
		// a host receiving a second VM from the same source has left.
		pages := premortemSource(t, m, 4)

		point, err := host.Seal(t.Context(), m.vm, m.machine)
		if err != nil {
			t.Fatal(err)
		}
		handoff, err := vmmigrate.Fork(t.Context(), "vm-2", point, pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		received, child := m.receive(t, handoff)
		// A receive that will never finish leaves its stream blocked, and a
		// failure inside a bubble with a blocked goroutine reports a deadlock
		// instead of what went wrong. Closing it first is what lets the
		// assertion be the failure.
		defer received.Close()
		ctx, cancel := context.WithTimeout(t.Context(), premortemStreamDeadline)
		defer cancel()
		if err := received.Done(ctx); err != nil {
			t.Fatalf("the child never got the pages only its parent had: %v", err)
		}
		child.adopt(at)
		if err := child.verify(child.ctx(), at); err != nil {
			t.Fatalf("the child does not hold the point it inherited: %v", err)
		}
	})
}

// TestPremortemFanOutChecksEveryPageWhileTheSourceIsStillStreaming forks one
// point onto another host twice, as a round of the soak does, and has both
// children read every page of every volume back while the stream behind them is
// still running and the source is at its per-peer budget. Every byte has to be
// the point's, whether it came off the wire, out of the child's own pages or
// out of object storage.
func TestPremortemFanOutChecksEveryPageWhileTheSourceIsStillStreaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(4) {
			m.machine.write("ram0", page)
			m.machine.write("disk", page)
		}
		at := m.machine.snapshot()
		// The production budget, which the two memory regions of one VM already fill:
		// a fan-out of two children is four memory regions against it.
		pages := premortemSource(t, m, 8)

		point, err := host.Seal(t.Context(), m.vm, m.machine)
		if err != nil {
			t.Fatal(err)
		}
		const children = 2
		receiveds := make([]*vmmigrate.Received, children)
		guests := make([]*machine, children)
		for index := range children {
			child := fmt.Sprintf("vm-child-%d", index)
			handoff, err := vmmigrate.Fork(t.Context(), child, point, pages, vmmigrate.Options{})
			if err != nil {
				t.Fatalf("handing %s over: %v", child, err)
			}
			receiveds[index], guests[index] = m.receive(t, handoff)
			defer receiveds[index].Close()
		}
		ctx, cancel := context.WithTimeout(t.Context(), premortemStreamDeadline)
		defer cancel()
		for index, received := range receiveds {
			if err := received.Done(ctx); err != nil {
				t.Fatalf("child %d never got the pages only its parent had: %v", index, err)
			}
		}
		// The check: every page of every volume of every child, read through
		// the guest's own fault path while the resident stream is still going.
		var wg sync.WaitGroup
		failures := make([]error, children)
		for index := range children {
			guests[index].adopt(at)
			wg.Go(func() { failures[index] = guests[index].verify(guests[index].ctx(), at) })
		}
		wg.Wait()
		for index, err := range failures {
			if err != nil {
				t.Fatalf("child %d does not hold the point it inherited: %v", index, err)
			}
		}
		for index, received := range receiveds {
			if err := received.Streamed(ctx); err != nil {
				t.Fatalf("child %d: the stream behind it: %v", index, err)
			}
			if err := received.VM().Checkpoint(t.Context()); err != nil {
				t.Fatalf("child %d: publishing its root: %v", index, err)
			}
			// The check read every page, so nothing of the point is left on
			// the source: the release the orchestrator drives has to be taken.
			if err := pages.Release(fmt.Sprintf("vm-child-%d", index)); err != nil {
				t.Fatalf("child %d: the source would not release it: %v", index, err)
			}
		}
		// And every child still reads the point once the source serves it
		// nothing at all, which is what the soak's next check does.
		for index := range children {
			if err := guests[index].verify(guests[index].ctx(), at); err != nil {
				t.Fatalf("child %d stopped holding the point once the source released it: %v", index, err)
			}
		}
		for range children {
			if err := point.Retire(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if status := m.vm.Status(); status.Sealed {
			t.Fatalf("the parent kept its seal after every child of the point published: %+v", status)
		}
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatalf("the parent could not checkpoint after the fan-out: %v", err)
		}
	})
}
