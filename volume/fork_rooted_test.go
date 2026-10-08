package volume_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// A fork is rooted only once its root publication has given back its hold on
// the point it was forked at. Whoever waits for the root goes on to release
// the child's other holds, so a fork that reported its root while it still
// held the point would leave its parent sealed after the last of them. The
// parent's seal is held open here, so the child's retire of the point waits
// inside its publication for as long as the test says.
func TestAForkIsRootedOnlyOnceItsHoldOnThePointIsGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		ctx := sim.WithRuntime(t.Context(), h.runtime)
		defer h.close(ctx)
		manager := h.manager(t, h.config())
		defer manager.Close(ctx)
		parent, _ := createVM(t, manager, "parent")
		defer parent.Close(ctx)
		if err := parent.Volume("root").Write(ctx, 0, []byte("parent bytes")); err != nil {
			t.Fatal(err)
		}
		if err := parent.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		seal := &gatedSeal{entered: make(chan struct{}, 1), released: make(chan struct{})}
		release := sync.OnceFunc(func() { close(seal.released) })
		defer release()
		point, err := parent.ForkPoint(ctx, volume.Prepared(nil, map[string]volume.DirtySource{"root": seal}))
		if err != nil {
			t.Fatal(err)
		}
		fork, err := manager.Fork(ctx, "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(ctx)

		published := make(chan error, 1)
		go func() { published <- fork.Checkpoint(ctx) }()
		// The root publication has installed the root and is giving its hold
		// on the point back, which waits on the parent's seal.
		<-seal.entered
		synctest.Wait()
		select {
		case err := <-published:
			t.Fatalf("the fork's root publication ended while its parent's seal was held open: %v", err)
		default:
		}
		select {
		case <-fork.Rooted():
			t.Fatalf("the fork reported its root while it still held the point: parent %+v", parent.Status())
		default:
		}
		if status := parent.Status(); !status.Sealed {
			t.Fatalf("the parent was unsealed while its point was unpublished: %+v", status)
		}

		release()
		if err := <-published; err != nil {
			t.Fatal(err)
		}
		select {
		case <-fork.Rooted():
		default:
			t.Fatal("the fork published its root and gave its hold back but reports no root")
		}
		if status := parent.Status(); status.Sealed {
			t.Fatalf("the parent is still sealed once its only child is rooted: %+v", status)
		}
	})
}

// gatedSeal is a seal of no pages whose retire says it has begun and then
// waits until the test releases it, which is how the end of a fork point is
// held open.
type gatedSeal struct{ entered, released chan struct{} }

func (s *gatedSeal) DirtyPages() []uint64 { return nil }
func (s *gatedSeal) ReadDirty(context.Context, uint64, []byte) error {
	return errors.New("a seal of no pages has none to read")
}
func (s *gatedSeal) Settle(context.Context) (int, error)              { return 0, nil }
func (s *gatedSeal) UnpublishedAge() time.Duration                    { return 0 }
func (s *gatedSeal) Hold()                                            {}
func (s *gatedSeal) Share(context.Context, control.Ref, string) error { return nil }
func (s *gatedSeal) Retire(ctx context.Context, _ bool) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A checkpoint that landed reports that it landed even when its seal did not
// end: the bytes are durable and selected. A fork's root reported as failed
// kept its parent's point, and the parent's sealed pages are given back by
// nothing else: a parent left sealed can never be checkpointed again. A stop
// reported as failed kept running a VM whose last checkpoint was selected.
func TestARootWhoseSealFailedToRetireStillLandsAndGivesThePointBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		ctx := sim.WithRuntime(t.Context(), h.runtime)
		defer h.close(ctx)
		manager := h.manager(t, h.config())
		defer manager.Close(ctx)
		parent, _ := createVM(t, manager, "parent")
		defer parent.Close(ctx)
		if err := parent.Volume("root").Write(ctx, 0, []byte("parent bytes")); err != nil {
			t.Fatal(err)
		}
		if err := parent.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		point, err := parent.ForkPoint(ctx, volume.Prepared(nil, map[string]volume.DirtySource{"root": noPages{}}))
		if err != nil {
			t.Fatal(err)
		}
		fork, err := manager.Fork(ctx, "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(ctx)

		refused := errors.New("the pager could not look its pages up")
		root, err := fork.Snapshot(ctx, volume.Prepared([]byte("state"),
			map[string]volume.DirtySource{"root": unretired{refused}}), volume.Terms{})
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Wait(ctx); err != nil {
			t.Fatalf("a root that landed and whose seal did not retire = %v, want it landed", err)
		}
		if got := fork.Status().Checkpoint; got != root.Ref() {
			t.Fatalf("the fork selects %s, want its root %s", got, root.Ref())
		}
		select {
		case <-fork.Rooted():
		default:
			t.Fatal("the fork's root landed but the fork reports no root")
		}
		if status := parent.Status(); status.Sealed {
			t.Fatalf("the parent is still sealed once its only child's root landed: %+v", status)
		}
	})
}

// noPages is a seal of no pages that ends when asked.
type noPages struct{}

func (noPages) DirtyPages() []uint64 { return nil }
func (noPages) ReadDirty(context.Context, uint64, []byte) error {
	return errors.New("a seal of no pages has none to read")
}
func (noPages) Settle(context.Context) (int, error)              { return 0, nil }
func (noPages) UnpublishedAge() time.Duration                    { return 0 }
func (noPages) Hold()                                            {}
func (noPages) Share(context.Context, control.Ref, string) error { return nil }
func (noPages) Retire(context.Context, bool) error               { return nil }

// unretired is a seal of no pages whose retire fails.
type unretired struct{ err error }

func (unretired) DirtyPages() []uint64 { return nil }
func (unretired) ReadDirty(context.Context, uint64, []byte) error {
	return errors.New("a seal of no pages has none to read")
}
func (unretired) Settle(context.Context) (int, error)              { return 0, nil }
func (unretired) UnpublishedAge() time.Duration                    { return 0 }
func (unretired) Hold()                                            {}
func (unretired) Share(context.Context, control.Ref, string) error { return nil }
func (s unretired) Retire(context.Context, bool) error             { return s.err }
