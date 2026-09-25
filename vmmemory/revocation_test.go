package vmmemory_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

type revokeBatchMapping struct {
	*mapping
	batches, singles int
	fail             bool
}

func (m *revokeBatchMapping) Revoke(ctx context.Context, page uint64) error {
	m.singles++
	return m.mapping.Revoke(ctx, page)
}
func (m *revokeBatchMapping) RevokeBatch(ctx context.Context, runs []vmmemory.PageRun) (int, int, error) {
	m.batches++
	for _, run := range runs {
		for i := range run.Count {
			if err := m.mapping.Revoke(ctx, run.Page+uint64(i)); err != nil {
				return 0, 0, err
			}
			if m.fail {
				return 0, 0, errInjected
			} // kernel may have applied a prefix
		}
	}
	return 1, len(runs), nil
}

// Abandoning a checkpoint takes the read-only mappings the seal installed away,
// so the guest's next store faults and maps the page writable again. Those
// revocations are batched: a large dirty set costs runs, not pages.
func TestAbandonedCheckpointRevokesItsMappingsInBoundedBatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 256, LogicalPages: 256, DirtyPages: 128, ReadAheadPages: 1})
		b := f.newBacking(256)
		base := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
		m := &revokeBatchMapping{mapping: base}
		f.a.mappings = append(f.a.mappings, base)
		r, err := f.h.Attach(t.Context(), ram(b), m)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { clear(base.pages); _ = r.Detach(t.Context()) }()
		for page := uint64(0); page < 256; page += 2 {
			access(t, r, base, page, true)[0] = byte(page + 31)
		}
		m.singles, m.batches = 0, 0
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := r.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if m.singles != 0 || m.batches != 1 {
			t.Fatalf("the abandoned checkpoint used %d single revokes and %d batches, want 0 and 1", m.singles, m.batches)
		}
		if len(base.pages) != 0 {
			t.Fatalf("%d mappings survived the abandoned checkpoint", len(base.pages))
		}
	})
}

func TestAmbiguousRevokeBatchPinsAllVictimsUntilDetach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 4, LogicalPages: 4, DirtyPages: 4, ReadAheadPages: 1})
		base := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
		m := &revokeBatchMapping{mapping: base}
		f.a.mappings = append(f.a.mappings, base)
		r, err := f.h.Attach(t.Context(), ram(f.newBacking(4)), m)
		if err != nil {
			t.Fatal(err)
		}
		for page := range uint64(4) {
			access(t, r, base, page, true)[0] = byte(page + 51)
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		m.fail = true
		if err := r.Checkpoint().Retire(t.Context(), false); !errors.Is(err, errInjected) {
			t.Fatalf("ambiguous batch was not returned: %v", err)
		}
		stats, _ := f.h.Stats(t.Context())
		if stats.ResidentPages != 4 || stats.DirtyPages != 4 {
			t.Fatalf("ambiguous batch released victims: %+v", stats)
		}
		if err := r.Fault(t.Context(), 0, false); err == nil {
			t.Fatal("continued after ambiguous mapping batch")
		}
		clear(base.pages)
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		stats, _ = f.h.Stats(t.Context())
		if stats.ResidentPages != 0 || stats.DirtyPages != 0 {
			t.Fatalf("detach retained victims: %+v", stats)
		}
	})
}

func TestPrivateAllocationExtendsAdjacentArenaRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 8, DirtyPages: 4, ReadAheadPages: 1})
		other, om, _ := f.memoryRegion(4)
		for page := range uint64(4) {
			access(t, other, om, page, false)
		}
		r, m, _ := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 71
		clear(om.pages)
		if err := other.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		access(t, r, m, 1, true)[0] = 72
		if m.pages[1].slot != m.pages[0].slot+1 {
			t.Fatalf("adjacent private pages were scattered into slots %d and %d", m.pages[0].slot, m.pages[1].slot)
		}
	})
}
