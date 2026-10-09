package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A 4 KiB guest writing sequentially through a full arena faults about once
// per write-ahead run, as it does before the arena fills. An eviction step
// frees a batch of slots, not the one its fault needs, so the stores after it
// find free slots for their write-ahead runs. One victim a step left every
// store after the arena filled a fault of its own: on GCE on 2026-10-09 a
// 12 GiB write ran at 8 MiB/s, one 4 KiB eviction and one fault a page
// (TASK-122.7).
func TestASequentialWriteThroughAFullArenaFaultsOncePerRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const resident, pages, run = 1024, 4096, 64
		f, r, m, _ := holeMemoryRegion(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB, ResidentPages: resident,
			LogicalPages: pages, DirtyPages: pages, ReadAheadPages: run, WriteAheadPages: run}, pages)
		value := byte(7)
		for page := range uint64(resident) {
			if _, err := memoryByte(f.ctx, r, m, page, &value); err != nil {
				t.Fatal(err)
			}
		}
		before := hostStats(t, f)
		for page := uint64(resident); page < pages; page++ {
			if _, err := memoryByte(f.ctx, r, m, page, &value); err != nil {
				t.Fatal(err)
			}
		}
		after := hostStats(t, f)
		if faults := after.Faults - before.Faults; faults != (pages-resident)/run {
			t.Fatalf("writing %d pages through the full arena took %d faults, want one per run of %d: %d",
				pages-resident, faults, run, (pages-resident)/run)
		}
	})
}
