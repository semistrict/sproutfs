package vmmigrate_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// The one post-copy rule: a page only the source holds is asked for until it
// arrives, or until something that knows says the source is gone — the source
// itself answering that it no longer serves the VM, or the orchestrator ending
// the migration by discarding the received VM. Nothing else counts, because
// nothing else here can tell a source that stumbled from one that died, and the
// two answers are opposite: the volume holds the checkpoint's bytes, which the
// guest has already written past.

// resettingConn fails the first resets replies it is asked for, each of which
// takes the connection with it, and behaves after that. It is a source
// restarting its listener, a connection its own budget dropped, and a reset in
// one.
type resettingConn struct {
	platform.Conn
	left *atomic.Int64
}

func (c resettingConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	if c.left.Add(-1) >= 0 {
		return platform.ReceivedFrame{}, platform.ErrDisconnected
	}
	return c.Conn.Receive(ctx)
}

// TestTwentyResetsInARowAreAskedAgain requires the destination to keep asking a
// source that keeps breaking. There is no attempt count: the pages of this run
// exist nowhere but that source, so the only alternatives to asking again are
// waiting for ever, which is what this does, and handing the guest bytes from
// before its own write, which is what it must never do.
func TestTwentyResetsInARowAreAskedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServed(t, nil, 4)
		var resets atomic.Int64
		resets.Store(20)
		dial := s.migration.cluster.dialer("dest")
		backing := s.unpublishedDialing(t, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}},
			func(ctx context.Context, address platform.Address) (platform.Conn, error) {
				conn, err := dial(ctx, address)
				if err != nil {
					return nil, err
				}
				return resettingConn{Conn: conn, left: &resets}, nil
			})
		data := make([]byte, 4*pageSize)
		unpublished, err := backing.LoadUnpublished(t.Context(), 0, data)
		if err != nil {
			t.Fatalf("a source that reset twenty replies: %v", err)
		}
		if left := resets.Load(); left >= 0 {
			t.Fatalf("the load gave up with %d resets left to survive", left+1)
		}
		want := s.machine.snapshot()["ram0"][:4*pageSize]
		if !bytes.Equal(data, want) {
			t.Fatal("a source that reset twenty replies sent the destination to a volume that never had these bytes")
		}
		for page, private := range unpublished {
			if !private {
				t.Fatalf("page %d did not come from the source", page)
			}
		}
		if stats := backing.Stats(); stats.FellBack || stats.PeerPages != 4 || stats.VolumePages != 0 {
			t.Fatalf("twenty resets: %+v", stats)
		}
	})
}

// TestASilentSourceLeavesTheFaultWaiting requires a fault on a page only the
// source holds to wait rather than fail. A source that has not answered in ten
// minutes is not a source that is gone: nothing here can tell the two apart, and
// the fault that gives up is a fault that loses the guest's memory. When the
// source answers, the fault completes with its bytes.
func TestASilentSourceLeavesTheFaultWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServed(t, nil, 4)
		gate := make(chan struct{})
		backing := s.unpublishedDialing(t, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}},
			holdPageReplies(s.migration.cluster.dialer("dest"), gate))
		data := make([]byte, 4*pageSize)
		done := make(chan error, 1)
		go func() {
			_, err := backing.LoadUnpublished(t.Context(), 0, data)
			done <- err
		}()
		synctest.Wait()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("a fault on a page only the source holds gave up after ten minutes: %v", err)
		default:
		}
		close(gate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		want := s.machine.snapshot()["ram0"][:4*pageSize]
		if !bytes.Equal(data, want) {
			t.Fatal("the fault completed with bytes the source never served")
		}
	})
}

// TestASourceThatSaysItIsGoneFailsItsOwnPagesAtOnce is the one answer that ends
// the asking. A source that no longer serves this VM has released its pages,
// which it does only after a release it agreed to: a page only it held is lost
// for good and says so at once, and every page a checkpoint holds comes from
// this host's own volume from there on.
func TestASourceThatSaysItIsGoneFailsItsOwnPagesAtOnce(t *testing.T) {
	s := newServed(t, nil, 4)
	backing := s.unpublishedBacking(t, nil, "ram0", []vmmigrate.PageRun{{First: 0, Count: 2}})
	// The source gives the VM up, so every request it answers from here says it
	// does not serve it.
	s.migration.pages.Discard("vm-2")
	data := make([]byte, 4*pageSize)
	if err := backing.Load(t.Context(), 0, data); !errors.Is(err, vmmigrate.ErrUnpublishedLost) {
		t.Fatalf("a page only a released source held = %v, want ErrUnpublishedLost", err)
	}
	// The pages a checkpoint holds are read from the volume, and the source is
	// not asked again for any of them.
	if err := backing.Load(t.Context(), 2*pageSize, data[:2*pageSize]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[:2*pageSize], make([]byte, 2*pageSize)) {
		t.Fatal("a published page did not read as the volume's own bytes")
	}
	stats := backing.Stats()
	if !stats.FellBack || stats.Requests != 1 || stats.PeerPages != 0 || stats.VolumePages != 2 {
		t.Fatalf("after the source said it no longer serves the VM: %+v", stats)
	}
}

// TestDiscardingTheReceivedVMEndsAWaitingFault is the other answer, and the one
// the orchestrator gives: it ends the migration, which discards the received VM
// and stops this backing. A fault that was waiting for a source that never
// answered ends with that cancellation's cause — not as a lost page, which says
// the guest's memory is torn, and not by reading a volume that holds bytes from
// before the guest's own write.
func TestDiscardingTheReceivedVMEndsAWaitingFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServed(t, nil, 4)
		gate := make(chan struct{})
		defer close(gate)
		backing := s.unpublishedDialing(t, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}},
			holdPageReplies(s.migration.cluster.dialer("dest"), gate))
		data := make([]byte, 4*pageSize)
		done := make(chan error, 1)
		go func() {
			_, err := backing.LoadUnpublished(t.Context(), 0, data)
			done <- err
		}()
		synctest.Wait()
		if err := backing.Close(); err != nil {
			t.Fatal(err)
		}
		err := <-done
		if !errors.Is(err, vmmigrate.ErrClosed) {
			t.Fatalf("a fault waiting when the received VM was discarded = %v, want ErrClosed", err)
		}
		if errors.Is(err, vmmigrate.ErrUnpublishedLost) {
			t.Fatalf("discarding the received VM was reported as a torn guest: %v", err)
		}
		if !bytes.Equal(data, make([]byte, len(data))) {
			t.Fatal("a fault the discard ended read bytes from somewhere")
		}
	})
}
