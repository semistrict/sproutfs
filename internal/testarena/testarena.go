// Package testarena is the arena mode a test suite builds its pagers in. A
// suite runs in one mode at a time, which SPROUTFS_ARENA names as a deployment
// does: shared, the default, or isolated. Every suite is run in both.
package testarena

import (
	"os"
	"testing"

	"github.com/semistrict/sproutfs/vmmemory"
)

// Var is the environment variable that names the mode.
const Var = "SPROUTFS_ARENA"

// Mode is the mode this run's pagers are built in. A value the pager does not
// know fails the test rather than running it in another mode.
func Mode(t testing.TB) vmmemory.ArenaMode {
	t.Helper()
	mode, err := parse()
	if err != nil {
		t.Fatal(err)
	}
	return mode
}

// MustMode is Mode for a TestMain, which has no test to fail.
func MustMode() vmmemory.ArenaMode {
	mode, err := parse()
	if err != nil {
		panic(err)
	}
	return mode
}

func parse() (vmmemory.ArenaMode, error) {
	name := os.Getenv(Var)
	if name == "" {
		return vmmemory.ArenaShared, nil
	}
	return vmmemory.ParseArenaMode(name)
}
