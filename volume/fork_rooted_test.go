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
