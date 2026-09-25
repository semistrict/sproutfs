package vmmemory_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmemory"
)

type partialPublicationArena struct{ *arena }

// File is this arena itself, so that the pager's one file loses the response.
func (a partialPublicationArena) File(context.Context, int) (vmmemory.ArenaFile, error) {
	return a, nil
}

func (a partialPublicationArena) Write(ctx context.Context, slot int, data []byte) error {
	if err := a.arena.Write(ctx, slot, data); err != nil {
		return err
	}
	if slot == 2 {
		return errInjected // The arena write took effect, but its response was lost.
	}
	return nil
}

func TestPartialReadAheadPublicationReturnsOnlyUnusedCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 4, 4)
		spill, err := f.disk.Open(t.Context(), "partial-spill", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		defer spill.Close()
		shared := testresource.New()
		f.h, err = vmmemory.New(t.Context(), shared, vmmemory.Config{PageSize: uint64(pageSize), ResidentPages: 4, LogicalPages: 4, DirtyPages: 4, ReadAheadPages: 4}, partialPublicationArena{f.a}, spill)
		if err != nil {
			t.Fatal(err)
		}
		r, m, _ := f.memoryRegion(4)
		if err := r.Fault(t.Context(), 0, false); !errors.Is(err, errInjected) {
			t.Fatalf("partial publication: %v", err)
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.ResidentPages != 2 || shared.Stats().Used != int64(2*pageSize) {
			t.Fatalf("only the two published pages should remain: %+v, %v", stats, err)
		}
		for page := range uint64(2) {
			if got := access(t, r, m, page, false)[0]; got != byte(page+1) {
				t.Fatalf("published page %d lost contents: %d", page, got)
			}
		}
		clear(m.pages)
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		expectIdleUntilReclaimed(t, f, shared, 2, int64(pageSize))
	})
}

func TestFailedReadAheadReturnsCapacityForNextMemoryRegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 4, 4)
		r, m, b := f.memoryRegion(4)
		b.failRead = true
		if err := r.Fault(t.Context(), 0, false); !errors.Is(err, errInjected) {
			t.Fatalf("failed backing read: %v", err)
		}
		clear(m.pages) // All memory users have stopped before detach.
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.ResidentPages != 0 || stats.LogicalPages != 0 {
			t.Fatalf("failed read retained capacity after detach: %+v, %v", stats, err)
		}
		next, mapping, _ := f.memoryRegion(4)
		for page := range uint64(4) {
			if got := access(t, next, mapping, page, false)[0]; got != byte(page+1) {
				t.Fatalf("next memory region page %d = %d", page, got)
			}
		}
	})
}
