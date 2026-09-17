package simtest_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/testsoak"
)

// buggifiedCampaignName is what the campaign's per-seed records are filed under
// when it runs with the per-site injection on.
const buggifiedCampaignName = "seeded-topology-buggify"

// TestSeededTopologyUnderBuggify is the same campaign with the per-site fault
// injection turned on. Every fault it adds is one the code is required to
// survive without telling its caller: a part that fills at one page, a
// source that answers BUSY because it is at its budget for this peer, a pager
// that evicts while an arena slot is free, a control-record write that takes
// seconds. The guests' bytes are checked exactly as they are without it, so a
// site that breaks a VM fails here.
//
// Each seed activates about a quarter of the sites it reaches, which is what
// makes a campaign of many seeds explore combinations rather than one fault at
// a time. These two are the cheapest pair that between them activate every site
// the campaign reaches; the soak probe campaign runs twenty-five. Which sites a
// seed activates is not asserted — that the campaign reaches them at all is,
// because a site nobody drives is exactly the fault injection this harness
// already had too much of.
func TestSeededTopologyUnderBuggify(t *testing.T) {
	reached := map[string]bool{}
	fired := map[string]uint64{}
	activated := map[string]bool{}
	for _, seed := range []uint64{1, 16} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			testsoak.Measure(t, buggifiedCampaignName, seed, func(t *testing.T) *sim.Runtime {
				runtime := runTopologyCampaign(t, seed, true)
				for site, on := range runtime.BuggifySites() {
					reached[site] = true
					if on {
						activated[site] = true
					}
				}
				for site, count := range runtime.FiredSites() {
					fired[site] += count
				}
				return runtime
			})
		})
	}
	t.Logf("reached=%v activated=%v fired=%v",
		slices.Sorted(maps.Keys(reached)), slices.Sorted(maps.Keys(activated)), fired)
	for _, site := range []string{
		"checkpoint/one-page-parts",
		"control/slow-write",
		"vmmemory/evict-past-a-free-slot",
		"vmmigrate/source-busy",
	} {
		if !reached[site] {
			t.Errorf("the campaign never reached the %s site", site)
		}
	}
	if len(fired) == 0 {
		t.Fatal("no buggified site fired across any seed: the switch or the activation draw is broken")
	}
}
