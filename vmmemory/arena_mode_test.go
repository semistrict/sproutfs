package vmmemory_test

import (
	"errors"
	"testing"

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
