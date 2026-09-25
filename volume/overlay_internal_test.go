package volume

import (
	"runtime"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
)

// The dirty estimate counts sectors, and a discard of a whole volume is one
// extent covering millions of them. Counting must cost what the extents cost,
// not what the sectors do: a list with a slot per sector is megabytes of
// garbage on a path that wants one number, and it grows with the volume.
func TestDirtySectorCountDoesNotListTheSectors(t *testing.T) {
	const span = 4 << 30
	overlay := replaceExtent(nil, extent{start: 0, end: span, generation: 1})
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := dirtySectorCount(overlay)
	runtime.ReadMemStats(&after)
	if want := uint64(span / checkpoint.SectorSize); got != want {
		t.Fatalf("dirty sectors = %d, want %d", got, want)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 64<<10 {
		t.Fatalf("counting %d sectors allocated %d bytes", got, grown)
	}
}

// Overlapping and adjacent extents report a sector once, whichever of them
// covers it: the count is of the sectors a checkpoint publishes, not of the
// writes that touched them.
func TestDirtySectorCountReportsEachSectorOnce(t *testing.T) {
	const sector = checkpoint.SectorSize
	overlay := replaceExtent(nil, extent{start: 0, end: 3 * sector, generation: 1})
	overlay = replaceExtent(overlay, extent{start: 2*sector + 7, end: 4 * sector, generation: 2})
	overlay = replaceExtent(overlay, extent{start: 9 * sector, end: 9*sector + 1, generation: 3})
	if got := dirtySectorCount(overlay); got != 5 {
		t.Fatalf("dirty sectors = %d, want 5", got)
	}
	// The same overlay covers one 2 MiB page and ten 4 KiB ones: what a
	// checkpoint republishes is counted in the volume's own page.
	large, err := checkpoint.GeometryFor(checkpoint.PageSize2MiB)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirtyPages(overlay, large); len(got) != 1 || got[0] != 0 {
		t.Fatalf("dirty 2 MiB pages = %v, want [0]", got)
	}
	small, err := checkpoint.GeometryFor(checkpoint.PageSize4KiB)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirtyPages(overlay, small); len(got) != 5 ||
		got[0] != 0 || got[3] != 3 || got[4] != 9 {
		t.Fatalf("dirty 4 KiB pages = %v, want [0 1 2 3 9]", got)
	}
}
