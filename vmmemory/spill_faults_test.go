package vmmemory_test

import (
	"maps"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// spillFileSites are the failures of making a pager's spill file and giving
// it back: emptying it, and reserving its extent.
var spillFileSites = []string{"sim/disk/io-error/truncate", "sim/disk/io-error/allocate", "sim/disk/no-space/allocate"}

// A pager whose spill file cannot be emptied or have its extent reserved
// never starts, and says why with the device's error; one that started gives
// the file back, or says why it could not. Each seed's disk fails at random as
// a device does, and the seeds run until every failure of making and giving
// back the file has happened. A campaign makes one pager a seed, too few for
// a failure a device makes one call in a hundred.
func TestAPagerWhoseSpillFileFailsNeverStartsAndSaysWhy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fired := make(map[string]uint64)
		missing := func() []string {
			var sites []string
			for _, site := range spillFileSites {
				if fired[site] == 0 {
					sites = append(sites, site)
				}
			}
			return sites
		}
		for seed := uint64(1); seed <= 10000 && len(missing()) != 0; seed++ {
			runtime := sim.New(sim.Config{Seed: seed, Buggify: true})
			ctx := sim.WithRuntime(t.Context(), runtime)
			spill, err := runtime.NewDisk("pager", sim.DiskConfig{}).Open(ctx, "spill", platform.OpenOptions{Create: true})
			if err == nil {
				var h *vmmemory.Host
				h, err = vmmemory.New(ctx, testresource.New(), vmmemory.Config{PageSize: uint64(pageSize),
					ResidentPages: 2, LogicalPages: 4, DirtyPages: 2, Arena: suiteArena}, newArena(pageSize), spill)
				if err == nil {
					err = h.Close(ctx)
				}
				if closed := spill.Close(); closed != nil && !injected(closed) {
					t.Fatalf("seed %d: closing the spill file: %v", seed, closed)
				}
			}
			if err != nil && !injected(err) {
				t.Fatalf("seed %d: %v", seed, err)
			}
			maps.Copy(fired, addCounts(fired, runtime.FiredSites()))
		}
		if sites := missing(); len(sites) != 0 {
			t.Fatalf("no seed failed %v", sites)
		}
	})
}
