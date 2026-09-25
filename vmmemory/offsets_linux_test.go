//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"bytes"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The arena is addresses, and its memory is what is put at them. A RAM arena is
// sized to the offsets a host's memory regions may need — one extent per range — and
// is a sparse file, so what it really holds is the pages put there and nothing
// else. This is that fact through the real memfd: pages at one offset per
// range, N pages of allocated blocks behind 512·N offsets, and the blocks gone
// again when the pages are released.
func TestALinuxArenaHoldsOnlyThePagesPutAtItsOffsets(t *testing.T) {
	const ranges = 8
	const size = checkpoint.PageSize4KiB
	offsets := ranges * rangePages
	a, err := vmmemory.NewLinuxArena(offsets, size)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Offsets() != offsets {
		t.Fatalf("the arena has %d offsets, want %d", a.Offsets(), offsets)
	}
	if n, err := a.AllocatedBytes(); err != nil || n != 0 {
		t.Fatalf("a fresh arena of %d offsets holds %d bytes: %v", offsets, n, err)
	}
	// One page per range, at the offset the first page of each range has: the
	// scattered pattern a guest writing one page of every 2 MiB makes.
	data := bytes.Repeat([]byte{73}, size)
	for r := range ranges {
		if err := a.Write(t.Context(), r*rangePages, data); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := a.AllocatedBytes(); err != nil || n != ranges*size {
		t.Fatalf("%d pages scattered over %d offsets hold %d bytes: %v, want %d",
			ranges, offsets, n, err, ranges*size)
	}
	// The last address is an address like any other: the memfd really is that
	// long, and nothing was allocated for the 511 empty offsets of each range.
	if err := a.Write(t.Context(), offsets-1, data); err != nil {
		t.Fatalf("the arena's last offset: %v", err)
	}
	got := make([]byte, size)
	if err := a.Read(t.Context(), offsets-1, got); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("reading the arena's last offset: %v", err)
	}
	if n, err := a.AllocatedBytes(); err != nil || n != (ranges+1)*size {
		t.Fatalf("the arena holds %d bytes over %d pages: %v, want %d",
			n, ranges+1, err, (ranges+1)*size)
	}
	// An offset holding no page reads as the hole it is, and reading one
	// allocates nothing.
	if err := a.Read(t.Context(), 1, got); err != nil || !bytes.Equal(got, make([]byte, size)) {
		t.Fatalf("reading an empty offset: %v", err)
	}
	if n, err := a.AllocatedBytes(); err != nil || n != (ranges+1)*size {
		t.Fatalf("reading a hole took the arena to %d bytes: %v", n, err)
	}
	// Releasing punches, so the memory leaves while the addresses stay.
	for r := range ranges {
		if err := a.Release(t.Context(), r*rangePages); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Release(t.Context(), offsets-1); err != nil {
		t.Fatal(err)
	}
	if n, err := a.AllocatedBytes(); err != nil || n != 0 {
		t.Fatalf("the released arena still holds %d bytes: %v", n, err)
	}
	if a.Offsets() != offsets {
		t.Fatalf("releasing the pages left the arena with %d offsets, want %d", a.Offsets(), offsets)
	}
}
