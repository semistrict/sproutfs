package volume_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// retiredPages is a seal that records how it ended: published, or given back.
type retiredPages struct {
	sealedPages
	mu      *sync.Mutex
	retired *[]bool
}

func (s retiredPages) Retire(_ context.Context, published bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.retired = append(*s.retired, published)
	return nil
}

func newRetiredPages(fill byte) (retiredPages, func() []bool) {
	var mu sync.Mutex
	var retired []bool
	source := retiredPages{sealedPages: sealedPages{size: checkpoint.PageSize2MiB, pages: []uint64{0}, fill: fill},
		mu: &mu, retired: &retired}
	return source, func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(retired)
	}
}

// A disk checkpoint whose publication fails keeps its seal while its Retry says
// so, and publishes again under the same reference. The seal is retired once,
// as published, when the store answers. No pause is taken for the retry, which
// is the whole of why it exists: a guest whose stores are held cannot be paused.
func TestAHeldPublicationIsPublishedAgainUnderItsOwnReference(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		source, retired := newRetiredPages(0x3c)

		h.runtime.ObjectStore().Fail()
		var attempts []int
		retry := func(_ context.Context, attempt int, err error) bool {
			if !errors.Is(err, platform.ErrUnavailable) {
				t.Errorf("attempt %d failed with %v, want the outage", attempt, err)
			}
			if got := retired(); len(got) != 0 {
				t.Errorf("the seal ended %v before the retry, want it held", got)
			}
			attempts = append(attempts, attempt)
			if attempt == 2 {
				h.runtime.ObjectStore().Recover()
			}
			return true
		}
		ckpt, err := vm.SnapshotDisks(t.Context(),
			volume.Prepared(nil, map[string]volume.DirtySource{"root": source}), retry)
		if err != nil {
			t.Fatal(err)
		}
		if err := ckpt.Wait(t.Context()); err != nil {
			t.Fatalf("the held publication failed after the store recovered: %v", err)
		}
		if !slices.Equal(attempts, []int{1, 2}) {
			t.Fatalf("the retry was asked after attempts %v, want [1 2]", attempts)
		}
		if got := retired(); !slices.Equal(got, []bool{true}) {
			t.Fatalf("the seal ended %v, want once, published", got)
		}
		if got := vm.Status().Checkpoint; got != ckpt.Ref() {
			t.Fatalf("the control record selects %v, want the held checkpoint's own %v", got, ckpt.Ref())
		}
		read := make([]byte, 4)
		if err := vm.Volume("root").Read(t.Context(), 0, read); err != nil || read[0] != 0x3c {
			t.Fatalf("the published page reads %x (%v), want the sealed bytes", read, err)
		}
	})
}

// A Retry that says no gives the pages back at once, which is what a checkpoint
// inside its loss window does: nothing is held behind it, so the next one
// seals them again.
func TestAPublicationItsRetryDeclinesGivesItsPagesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		source, retired := newRetiredPages(0x4d)

		h.runtime.ObjectStore().Fail()
		asked := 0
		ckpt, err := vm.SnapshotDisks(t.Context(),
			volume.Prepared(nil, map[string]volume.DirtySource{"root": source}),
			func(context.Context, int, error) bool {
				asked++
				return false
			})
		if err != nil {
			t.Fatal(err)
		}
		if err := ckpt.Wait(t.Context()); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("the declined publication reported %v, want the outage", err)
		}
		if asked != 1 {
			t.Fatalf("the retry was asked %d times, want once", asked)
		}
		if got := retired(); !slices.Equal(got, []bool{false}) {
			t.Fatalf("the seal ended %v, want once, given back", got)
		}
		h.runtime.ObjectStore().Recover()
	})
}

// A publication a later writer fenced can never land, so it is never retried,
// whatever its Retry would have said.
func TestAFencedPublicationIsNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		other := h.manager(t, h.config())
		defer other.Close(t.Context())
		taken, err := other.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer taken.Close(t.Context())
		source, retired := newRetiredPages(0x5e)

		ckpt, err := vm.SnapshotDisks(t.Context(),
			volume.Prepared(nil, map[string]volume.DirtySource{"root": source}),
			func(context.Context, int, error) bool {
				t.Error("a fenced publication asked whether to try again")
				return true
			})
		if err != nil {
			t.Fatal(err)
		}
		if err := ckpt.Wait(t.Context()); !errors.Is(err, volume.ErrNeedsRecovery) {
			t.Fatalf("the fenced publication reported %v, want ErrNeedsRecovery", err)
		}
		if got := retired(); !slices.Equal(got, []bool{false}) {
			t.Fatalf("the seal ended %v, want once, given back", got)
		}
	})
}
