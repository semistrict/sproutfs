package vmmigrate_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// streamGate holds the post-copy stream's first request to the source before
// it is sent, and counts the stream's requests that reached that point.
type streamGate struct {
	mu      sync.Mutex
	asked   int
	release chan struct{}
}

func (g *streamGate) admit(ctx context.Context, _ string) error {
	if peer.ClassOf(ctx) != peer.BulkRead {
		return nil
	}
	g.mu.Lock()
	g.asked++
	first := g.asked == 1
	g.mu.Unlock()
	if !first {
		return nil
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (g *streamGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked
}

// The stream keeps several faults in flight, and each decides things the run
// observes before its request leaves: the arena slot it takes, the page it
// evicts for it, and where its request sits on the link. So each waits for the
// page before it to have its request on the wire, and a page held up before
// its request leaves holds up the pages behind it. Faulting them all at once
// left their order to the Go scheduler, and a seed of the topology campaign
// could not reproduce its evictions.
func TestAStreamPageWaitsForThePageBeforeItToBeAskedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		gate := &streamGate{release: make(chan struct{})}
		ctx := vmmigrate.WithAdmission(sim.WithRuntime(t.Context(), m.cluster.runtime), gate.admit)
		received, err := vmmigrate.Receive(ctx, m.destination, handoff, m.cluster.peers(t, m.cluster.dialer("dest")),
			func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (vmmigrate.Runtime, error) {
				built, err := newMachine(t, m.destPager, vm, backings, state)
				if err != nil {
					return nil, err
				}
				return built, built.Resume(ctx)
			}, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		asked := gate.count()
		close(gate.release)
		if err := received.Streamed(t.Context()); err != nil {
			t.Fatal(err)
		}
		if asked != 1 {
			t.Fatalf("the stream asked for %d pages while the first was held before it was sent, want 1", asked)
		}
		// Every page the source held came over, one request each.
		if stats := received.Stats(); stats.Streamed != 12 || gate.count() != 12 {
			t.Fatalf("the stream brought %d pages in %d requests, want the 12 the source held in 12", stats.Streamed, gate.count())
		}
	})
}
