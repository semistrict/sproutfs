package simtest_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// registeredProbes is every place the production code marks as one a campaign
// has to reach. They are the answers to the question the failure-coverage
// section of the testing notes implies but nothing measured: not which faults a
// harness can inject, but which code the injected faults actually ran.
var registeredProbes = []string{
	control.ProbePublicationFenced,
	control.ProbeReplyReconciled,
	checkpoint.ProbeCompactionRewrite,
	vmmemory.ProbeEvictionDuringPublication,
	vmmigrate.ProbeVolumeFallback,
}

// unreachedProbes are the registered probes no campaign in this repository
// reaches. The store either answers or fails outright here, so no conditional
// write ever loses its reply and is reconciled by its writer's nonce; and the
// pagers evict, but never while the memory region an eviction takes a page from is
// sealed.
//
// The list is asserted in both directions. A probe on it that starts firing is
// a campaign that grew coverage and a line to delete here; a probe off it that
// stops firing is coverage lost.
var unreachedProbes = []string{
	control.ProbeReplyReconciled,
	vmmemory.ProbeEvictionDuringPublication,
}

// A probe nobody reaches is the failure mode the whole harness exists to rule
// out, and it is invisible in a passing run: a campaign reports the faults it
// injected, not the code they made run. This runs the campaigns across enough
// seeds for the per-seed activation draw to have offered every buggified site,
// and requires every probe not listed as unreached to have fired.
func TestTheCampaignsReachTheirProbes(t *testing.T) {
	if !testsoak.Enabled() {
		t.Skipf("set %s=1 for the probe coverage campaign", testsoak.EnableVar)
	}
	reached := map[string]uint64{}
	for seed := uint64(1); seed <= 25; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				for name, count := range runTopologyCampaign(t, seed, true).Probes() {
					reached[name] += count
				}
			})
		})
	}
	// The two-writer campaign is where a publication is fenced by a later open
	// rather than by the loss of its host, which is the one probe the generated
	// schedule cannot reach: its takeovers all follow a host that is gone.
	for seed := uint64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("swizzle-seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				for name, count := range runSwizzleCampaign(t, seed).Probes() {
					reached[name] += count
				}
			})
		})
	}
	t.Logf("reached=%v", reached)
	expected := map[string]bool{}
	for _, name := range registeredProbes {
		expected[name] = !slices.Contains(unreachedProbes, name)
	}
	for _, name := range slices.Sorted(maps.Keys(expected)) {
		switch {
		case expected[name] && reached[name] == 0:
			t.Errorf("the campaigns never reach %s, which they are registered to cover", name)
		case !expected[name] && reached[name] > 0:
			t.Errorf("the campaigns now reach %s %d times: take it off unreachedProbes", name, reached[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(reached)) {
		if !slices.Contains(registeredProbes, name) {
			t.Errorf("%s is marked in the code but not registered here", name)
		}
	}
}
