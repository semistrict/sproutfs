package vmmigrate_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// TestACallerGivingUpIsNotALostPage. Done is what a destination's caller waits
// on, and that caller is an HTTP handler whose client may disconnect or a drain
// whose deadline may pass. Its giving up says nothing about the VM: the stream
// runs on a context of this package's own and keeps fetching, and the guest is
// running and correct. Only a page no checkpoint holds that never arrives means
// the VM's memory is torn, so only that may be read as one.
func TestACallerGivingUpIsNotALostPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(8) {
			m.machine.write("ram0", page)
		}
		handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		received, err := vmmigrate.Receive(t.Context(), m.destination, handoff,
			holdPageReplies(m.cluster.dialer("dest"), gate),
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
		defer received.Close()

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- received.Done(ctx) }()
		synctest.Wait()
		cancel()
		err = <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Done for a caller that gave up = %v", err)
		}
		if errors.Is(err, vmmigrate.ErrUnpublishedLost) {
			t.Fatalf("a caller that gave up was reported as a VM whose memory is torn: %v", err)
		}
		// The post-copy never stopped: the source answers, and the same VM's
		// Done reports the whole unpublished set fetched.
		close(gate)
		if err := received.Done(t.Context()); err != nil {
			t.Fatal(err)
		}
		if stats := received.Stats(); stats.Unpublished == 0 || stats.Fetched != stats.Unpublished {
			t.Fatalf("Done returned having fetched %d of %d unpublished pages", stats.Fetched, stats.Unpublished)
		}
	})
}

// TestAStoppedStreamIsNotALostPage. Closing a Received stops the stream and
// drops the connections; the VM keeps running on everything its own checkpoint
// holds. The pages the stream had not reached are still only on the source, and
// saying so is the point — but it is not the same statement as a page that was
// asked for and never came, and a caller that closed the stream itself must not
// be told its guest is torn.
func TestAStoppedStreamIsNotALostPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(8) {
			m.machine.write("ram0", page)
		}
		handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		defer close(gate)
		received, err := vmmigrate.Receive(t.Context(), m.destination, handoff,
			holdPageReplies(m.cluster.dialer("dest"), gate),
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
		received.Close()
		err = received.Done(t.Context())
		if !errors.Is(err, vmmigrate.ErrClosed) {
			t.Fatalf("Done after the stream was stopped = %v", err)
		}
		if errors.Is(err, vmmigrate.ErrUnpublishedLost) {
			t.Fatalf("stopping the stream was reported as a VM whose memory is torn: %v", err)
		}
	})
}

// TestACancelledListingIsNotAFallback. A destination asks its source what it
// holds before it streams anything in behind the guest, and the caller that
// asks is the stream — which the destination stops itself, cancelling with a
// cause of its own. That cancellation says nothing about the source: giving it
// up there sends the region to a volume that does not hold the pages no
// checkpoint has, so every later fault on one of them fails while the source is
// still there and still serving.
func TestACancelledListingIsNotAFallback(t *testing.T) {
	s := newServed(t, nil, 4)
	backing := s.unpublishedBacking(t, nil, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}})
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(vmmigrate.ErrClosed)
	if _, err := backing.Resident(ctx); !errors.Is(err, vmmigrate.ErrClosed) {
		t.Fatalf("a listing for a caller that gave up = %v", err)
	}
	if stats := backing.Stats(); stats.FellBack {
		t.Fatalf("a cancelled listing ended the post-copy: %+v", stats)
	}
	// The source is still there, so the pages only it has still arrive.
	data := make([]byte, 4*pageSize)
	unpublished, err := backing.LoadUnpublished(t.Context(), 0, data)
	if err != nil {
		t.Fatal(err)
	}
	for page, private := range unpublished {
		if !private {
			t.Fatalf("page %d did not come from the source", page)
		}
	}
	if stats := backing.Stats(); stats.PeerPages != 4 {
		t.Fatalf("after a cancelled listing: %+v", stats)
	}
}
