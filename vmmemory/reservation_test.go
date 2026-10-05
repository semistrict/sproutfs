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

// File is a file of this arena whose writes of slot 2 lose their response.
func (a partialPublicationArena) File(ctx context.Context, offsets int) (vmmemory.ArenaFile, error) {
	f, err := a.arena.File(ctx, offsets)
	if err != nil {
		return nil, err
	}
	return partialPublicationFile{f.(*arenaFile)}, nil
}

type partialPublicationFile struct{ *arenaFile }

func (f partialPublicationFile) Write(ctx context.Context, slot int, data []byte) error {
	if err := f.arenaFile.Write(ctx, slot, data); err != nil {
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
		f.h, err = vmmemory.New(t.Context(), shared, vmmemory.Config{PageSize: uint64(pageSize), ResidentPages: 4, LogicalPages: 4, DirtyPages: 4, ReadAheadPages: 4, Core: suiteCore}, partialPublicationArena{f.a}, spill)
		if err != nil {
			t.Fatal(err)
		}
		r, m, _ := f.memoryRegion(4)
		// The fault reads page 0 into slot 0, and its prefetch reads pages 1 to
		// 3 into slots 1 to 3, the third of whose writes fails after taking
		// effect. The fault is not the prefetch's, so it does not fail.
		if err := r.Fault(t.Context(), 0, false); err != nil {
			t.Fatalf("the fault failed with its prefetch: %v", err)
		}
		if err := r.SettlePrefetches(t.Context()); err != nil {
			t.Fatal(err)
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.ResidentPages != 3 || stats.PrefetchDropped != 1 || shared.Stats().Used != int64(3*pageSize) {
			t.Fatalf("only the three published pages should remain: %+v, %v", stats, err)
		}
		for _, page := range []uint64{0, 1, 3} {
			if got := access(t, r, m, page, false)[0]; got != byte(page+1) {
				t.Fatalf("published page %d lost contents: %d", page, got)
			}
		}
		clear(m.pages)
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		expectIdleUntilReclaimed(t, f, shared, 3, int64(pageSize))
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
