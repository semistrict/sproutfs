// Package testcore is the pager core a test suite builds its pagers in. A
// suite runs in one core at a time, which SPROUTFS_PAGER_CORE names as a
// deployment does: current, the default, or zircon. just check runs the tests
// the zircon core serves under it too (scripts/pager-core-zircon.json).
package testcore

import (
	"os"
	"testing"

	"github.com/semistrict/sproutfs/vmmemory"
)

// Var is the environment variable that names the core.
const Var = "SPROUTFS_PAGER_CORE"

// Core is the core this run's pagers are built in. A value the pager does not
// know fails the test rather than running it in another core.
func Core(t testing.TB) vmmemory.Core {
	t.Helper()
	core, err := parse()
	if err != nil {
		t.Fatal(err)
	}
	return core
}

// MustCore is Core for a TestMain, which has no test to fail.
func MustCore() vmmemory.Core {
	core, err := parse()
	if err != nil {
		panic(err)
	}
	return core
}

func parse() (vmmemory.Core, error) {
	name := os.Getenv(Var)
	if name == "" {
		return vmmemory.CoreCurrent, nil
	}
	return vmmemory.ParseCore(name)
}
