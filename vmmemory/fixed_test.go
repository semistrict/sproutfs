package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// attachFixed maps one backing as the RAM of a nested VM.
func (f *fixture) attachFixed(b vmmemory.Backing) (*vmmemory.MemoryRegion, *mapping) {
	f.t.Helper()
	return f.attachBacking(vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: b, Fixed: true})
}

// A fixed memory region's pages are never a victim: another region that needs
// room evicts its own pages and never one the fixed region maps, because KVM
// may be writing that page behind the page tables.
func TestAFixedRegionsPagesAreNeverEvicted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The fixed region holds three of four slots, more than its fair share
		// of two, so it is the first region eviction would take from.
		f := newFixture(t, 4, 16, 8)
		fixed, fm := f.attachFixed(f.newBacking(3))
		access(t, fixed, fm, 0, true)[0] = 41
		access(t, fixed, fm, 1, false)
		access(t, fixed, fm, 2, false)
		other, om, _ := f.memoryRegion(6)
		for page := range uint64(6) {
			access(t, other, om, page, false)
		}
		if s := hostStats(t, f); s.Evictions == 0 {
			t.Fatal("the other region read six pages through one slot and evicted nothing")
		}
		for page := range uint64(3) {
			if _, mapped := fm.pages[page]; !mapped {
				t.Fatalf("page %d of the fixed region was taken away", page)
			}
		}
		if got := access(t, fixed, fm, 0, false)[0]; got != 41 {
			t.Fatalf("the fixed region reads %d, want the 41 it stored", got)
		}
	})
}

// A fixed memory region is never sealed, because a seal is a write-protect that
// KVM's writes to a nested guest's VMCS pages would not respect.
func TestAFixedRegionIsNeverSealed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		fixed, fm := f.attachFixed(f.newBacking(2))
		access(t, fixed, fm, 0, true)
		if err := fixed.Seal(t.Context()); !errors.Is(err, vmmemory.ErrFixed) {
			t.Fatalf("sealing a fixed region = %v, want ErrFixed", err)
		}
		if given, err := fixed.GiveBack(t.Context(), 16); given != 0 || err != nil {
			t.Fatalf("a give-back of a fixed region gave back %d pages (%v), want none", given, err)
		}
		if s := hostStats(t, f); s.GiveBackCompares != 0 {
			t.Fatalf("a give-back compared %d pages of a fixed region, want none", s.GiveBackCompares)
		}
	})
}

// Fixed regions are admitted only while they fit in the arena together, and
// only RAM is ever fixed.
func TestTheArenaAdmitsFixedRegionsOnlyWhileTheyFit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 16, 8)
		f.attachFixed(f.newBacking(3))
		m := newMapping(f.a)
		f.a.mappings = append(f.a.mappings, m)
		_, err := f.h.Attach(t.Context(), vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: f.newBacking(2),
			Fixed: true}, m)
		if !errors.Is(err, vmmemory.ErrCapacity) {
			t.Fatalf("a fixed region past the arena attached with %v, want ErrCapacity", err)
		}
		_, err = f.h.Attach(t.Context(), vmmemory.MemoryRegionBacking{Kind: vmmemory.Pmem, Backing: f.newBacking(1),
			Fixed: true}, m)
		if !errors.Is(err, vmmemory.ErrConfig) {
			t.Fatalf("a fixed PMEM region attached with %v, want ErrConfig", err)
		}
		f.memoryRegion(1)
	})
}
