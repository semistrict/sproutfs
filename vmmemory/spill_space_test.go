package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// spaceFixture is a pager of the given dirty budget whose spill file is on a
// filesystem of total bytes. It is built under a context that carries the
// disk's runtime, so the pager consults the guards SPROUTFS_SIM_BUG names.
func spaceFixture(t *testing.T, total int64, dirty int) (*fixture, error) {
	t.Helper()
	runtime := sim.New(sim.Config{})
	disk := runtime.NewDisk("pager", sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: total}})
	return newFixtureOn(t, sim.WithRuntime(t.Context(), runtime), disk, spaceConfig(dirty))
}

// spaceConfig is a space fixture's pager of dirty reservations.
func spaceConfig(dirty int) vmmemory.Config {
	return vmmemory.Config{PageSize: uint64(pageSize), ResidentPages: 2, LogicalPages: 4, DirtyPages: dirty,
		Arena: suiteArena}
}

// spillHolds is what the fixture's spill file holds on its filesystem.
func (f *fixture) spillHolds() int64 {
	f.t.Helper()
	allocated, err := f.spill.(platform.FileAllocation).Allocated(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	return allocated
}

// A pager holds its spill file's whole extent from the moment it starts,
// before anything is spilled to it.
func TestAPagerHoldsItsWholeSpillFileFromTheStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, err := spaceFixture(t, 64<<20, 2)
		if err != nil {
			t.Fatal(err)
		}
		// Its dirty budget of two pages, and a slot for each fill of a version.
		want := vmmemory.SpillFileBytes(spaceConfig(2))
		if want != int64(10*pageSize) {
			t.Fatalf("a spill file of two reservations is %d bytes, want ten pages", want)
		}
		if size, holds := f.spillBytes(), f.spillHolds(); size != want || holds != want {
			t.Fatalf("a new pager's spill file is %d bytes and holds %d, want %d and %d", size, holds, want, want)
		}
		if got := f.disk.Usage().HostBytes; got != want {
			t.Fatalf("the filesystem counts %d bytes against the pager, want %d", got, want)
		}
	})
}

// Another writer that fills the filesystem after a pager starts cannot make a
// guest's spill fail: the spill file already holds its extent. A slot given
// back keeps its space too, so the next page spilled to it fits as well.
func TestASpillSucceedsOnADiskFilledFromOutside(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// One reservation, so the second page is spilled to the slot the first
		// one gave back.
		f, err := spaceFixture(t, 64<<20, 1)
		if err != nil {
			t.Fatal(err)
		}
		usage := f.disk.Usage()
		f.disk.SetOutsideBytes(usage.TotalBytes - usage.HostBytes)
		if free := f.disk.Usage().FreeBytes(); free != 0 {
			t.Fatalf("the filesystem has %d bytes free, want it full", free)
		}
		for round, value := range []byte{11, 22} {
			r, m, _ := f.memoryRegion(3)
			// Page zero is private and dirty. Faulting the other two evicts it,
			// so it is spilled.
			access(t, r, m, 0, true)[0] = value
			access(t, r, m, 1, false)
			access(t, r, m, 2, false)
			stats, err := f.h.Stats(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if stats.Spills != uint64(round+1) {
				t.Fatalf("round %d spilled %d pages in all, want %d", round, stats.Spills, round+1)
			}
			if err := r.Fault(t.Context(), 0, false); err != nil {
				t.Fatalf("round %d: faulting the spilled page back: %v", round, err)
			}
			if got := m.arena.page(m.pages[0].place)[0]; got != value {
				t.Fatalf("round %d read back %d, want the guest's own store of %d", round, got, value)
			}
			// Its VMM is gone. Detaching discards the dirty page and gives its
			// slot back.
			clear(m.pages)
			if err := r.Detach(t.Context()); err != nil {
				t.Fatal(err)
			}
			if holds, want := f.spillHolds(), vmmemory.SpillFileBytes(spaceConfig(1)); holds != want {
				t.Fatalf("round %d: the spill file holds %d bytes once its slot is given back, want its extent of %d",
					round, holds, want)
			}
		}
	})
}

// A pager whose filesystem has no room for its spill file is refused when it
// starts, rather than when a guest's page has nowhere to go.
func TestAPagerWhoseDiskCannotHoldItsSpillFileIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, err := spaceFixture(t, int64(pageSize), 2)
		if !errors.Is(err, platform.ErrNoSpace) {
			t.Fatalf("a pager whose spill file needs 2 pages of a 1-page filesystem started with %v, want %v",
				err, platform.ErrNoSpace)
		}
	})
}
