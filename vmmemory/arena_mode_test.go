package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A deployment names the mode, and the pager reads exactly the two it has.
func TestArenaModesAreReadByTheirNames(t *testing.T) {
	for name, want := range map[string]vmmemory.ArenaMode{"shared": vmmemory.ArenaShared, "isolated": vmmemory.ArenaIsolated} {
		got, err := vmmemory.ParseArenaMode(name)
		if err != nil || got != want {
			t.Fatalf("%q reads as %s, %v, want %s", name, got, err, want)
		}
		if got.String() != name {
			t.Fatalf("%s names itself %q, want %q", got, got.String(), name)
		}
	}
	if _, err := vmmemory.ParseArenaMode("split"); !errors.Is(err, vmmemory.ErrConfig) {
		t.Fatalf("an unknown mode reads as %v, want ErrConfig", err)
	}
}

// A pager is built in one of the modes it has, and the zero value is shared.
func TestNewRefusesAnArenaModeItDoesNotHave(t *testing.T) {
	if vmmemory.ArenaMode(0) != vmmemory.ArenaShared {
		t.Fatalf("the zero mode is %s, want shared", vmmemory.ArenaMode(0))
	}
	if _, err := newBrokenFixture(t, vmmemory.Config{PageSize: uint64(pageSize), ResidentPages: 2,
		LogicalPages: 4, DirtyPages: 2, Arena: vmmemory.ArenaMode(2)}); !errors.Is(err, vmmemory.ErrConfig) {
		t.Fatalf("a pager of an unknown arena mode was built: %v", err)
	}
}

// The isolated arena is being built, and until it is a pager in that mode keeps
// every page where a shared pager does: two forks share one page, a store
// copies it, and a checkpoint publishes the copy, at the same slots in both.
func TestAnIsolatedPagerKeepsEveryPageWhereASharedOneDoes(t *testing.T) {
	slots := map[vmmemory.ArenaMode][2]int{}
	for _, mode := range []vmmemory.ArenaMode{vmmemory.ArenaShared, vmmemory.ArenaIsolated} {
		synctest.Test(t, func(t *testing.T) {
			f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: 8, DirtyPages: 4, Arena: mode})
			a, am, ab := f.memoryRegion(4)
			b, bm, _ := f.memoryRegion(4)
			access(t, a, am, 0, false)
			access(t, b, bm, 0, false)
			if am.pages[0].slot != bm.pages[0].slot {
				t.Fatalf("%s: the forks map slots %d and %d, want one shared slot", mode, am.pages[0].slot, bm.pages[0].slot)
			}
			access(t, a, am, 0, true)[0] = 99
			if got := access(t, b, bm, 0, false)[0]; got != 1 {
				t.Fatalf("%s: the sibling reads %d after the store, want 1", mode, got)
			}
			f.mustCheckpoint(a, ab)
			if ab.data[0] != 99 {
				t.Fatalf("%s: the checkpoint published %d, want 99", mode, ab.data[0])
			}
			slots[mode] = [2]int{am.pages[0].slot, bm.pages[0].slot}
		})
	}
	if slots[vmmemory.ArenaShared] != slots[vmmemory.ArenaIsolated] {
		t.Fatalf("the shared pager put the pages at %v and the isolated one at %v, want the same slots",
			slots[vmmemory.ArenaShared], slots[vmmemory.ArenaIsolated])
	}
}
