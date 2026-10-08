package simtest_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/platform/sim"
)

// boundaryCampaignName is what this campaign's per-seed records are filed
// under.
const boundaryCampaignName = "seeded-topology-boundaries"

// boundarySeeds are the cheapest set that between them fire every site
// scripts/faults names for this campaign. The soak runs three hundred.
var boundarySeeds = []uint64{11, 16, 26, 59, 69, 75, 102, 119}

// TestSeededTopologyUnderBoundaryFaults is the buggified campaign over the
// seeds that between them fire every error the world's simulated boundaries
// return (scripts/faults): a VMM process that refuses a command, never
// answers one and is killed, crashes, fails to start or fails to close; a
// sealed checkpoint whose read, settle, share or retire fails; a peer that
// carried a request out and whose reply was lost. The guests' bytes are
// checked as in every other run, and a VM whose process ended must come back
// at a checkpoint it published. scripts/check-faults.py requires the sites to
// fire.
func TestSeededTopologyUnderBoundaryFaults(t *testing.T) {
	seeds := boundarySeeds
	if testsoak.Enabled() {
		seeds = nil
		for seed := range uint64(300) {
			seeds = append(seeds, seed+1)
		}
	}
	fired := map[string]uint64{}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			testsoak.Measure(t, boundaryCampaignName, seed, func(t *testing.T) *sim.Runtime {
				runtime := runTopologyCampaign(t, seed, true, cacheDrawn)
				for site, count := range runtime.FiredSites() {
					fired[site] += count
				}
				return runtime
			})
		})
	}
	t.Logf("fired=%v", slices.Sorted(maps.Keys(fired)))
}
