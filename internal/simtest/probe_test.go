package simtest_test

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/handover"
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
	control.ProbeRecordAdopted,
	checkpoint.ProbeCompactionRewrite,
	// Hosts whose disks are one cache fill each other: a read of the store
	// is given its window's fill right, and a stripe one host sends another
	// is kept.
	checkpoint.ProbeFillRightGranted,
	checkpoint.ProbeKeepKept,
	// And they read from each other: a page is rebuilt from stripes a peer
	// sent.
	checkpoint.ProbeClusterHit,
	vmmemory.ProbeEvictionDuringPublication,
	vmmigrate.ProbeVolumeFallback,
	vmmigrate.ProbePublishedSinceHandoff,
	handover.ProbeRetried,
}

// unreachedProbes are the registered probes no campaign in this repository
// reaches. None today: the campaigns now lose a conditional write's reply, and
// its writer reconciles it by its nonce.
//
// The list is asserted in both directions. A probe on it that starts firing is
// a campaign that grew coverage and a line to delete here; a probe off it that
// stops firing is coverage lost.
var unreachedProbes = []string{}

// probeSeeds are the seeds of the generated schedule the probe campaign runs.
// The first twenty-five reach every registered probe but one: none of them
// evicts a page of a memory region while a publication reads it. Seed 46 does,
// with the cluster cache on and with no cache disk alike.
var probeSeeds = append(seedsThrough(25), 46)

// seedsThrough is the seeds from 1 to last.
func seedsThrough(last uint64) []uint64 {
	seeds := make([]uint64, 0, last)
	for seed := uint64(1); seed <= last; seed++ {
		seeds = append(seeds, seed)
	}
	return seeds
}

// elsewhere reports a probe another campaign is registered to cover, which
// these campaigns reach too: the peer server's (TestThePeerServerCampaignNeverAnswersWrong),
// the membership's (TestConcurrentWritersNeverLoseAnUpdateOrGoBack, and the
// read campaign in checkpoint for the protocol), and the page cache disk's,
// its stripes', its fills', its reads of the cluster and its pulls (the disk,
// stripe, fill, read and pull campaigns in checkpoint), the bounded store
// requests' (platform/bounded's tests), and the pager's prefetches (the
// prefetch campaign in vmmemory).
func elsewhere(name string) bool {
	for _, prefix := range []string{"peer/", "membership/", "checkpoint/disk-", "checkpoint/fill-",
		"checkpoint/keep-", "checkpoint/cluster-", "checkpoint/presence-", "checkpoint/pull-", "bounded/",
		"vmmemory/prefetch-"} {
		if strings.HasPrefix(name, prefix) && !slices.Contains(registeredProbes, name) {
			return true
		}
	}
	return false
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
	// Every seed runs with the hosts' disks one cache and with no cache
	// disk, so the paths each takes are both covered whatever a seed draws.
	for _, seed := range probeSeeds {
		for _, cache := range []campaignCache{cacheOff, cacheOn} {
			t.Run(fmt.Sprintf("seed-%d/cache-%v", seed, cache == cacheOn), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					for name, count := range runTopologyCampaign(t, seed, true, cache).Probes() {
						reached[name] += count
					}
				})
			})
		}
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
		if !slices.Contains(registeredProbes, name) && !elsewhere(name) {
			t.Errorf("%s is marked in the code but not registered here", name)
		}
	}
}
