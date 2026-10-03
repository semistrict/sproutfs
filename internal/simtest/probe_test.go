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
// reaches. The store either answers or fails outright here, so no conditional
// write ever loses its reply and is reconciled by its writer's nonce.
//
// The list is asserted in both directions. A probe on it that starts firing is
// a campaign that grew coverage and a line to delete here; a probe off it that
// stops firing is coverage lost.
var unreachedProbes = []string{
	control.ProbeReplyReconciled,
}

// unsteadyProbes are the registered probes a campaign reaches on some runs of
// its seeds and not on others, which is asserted neither way. An eviction
// during a publication is reached by seed 2 with the cluster cache on in about
// half its runs, and by no run with no cache disk or of any other seed. The
// fills are not what varies: a host does them one at a time, and two runs of
// the seed first differ before the publication, where a destination's memory
// regions stream their pages from the source on several goroutines and take
// arena slots in the order the Go scheduler runs them. Under the site that
// evicts past a free slot, that order decides which page a later store
// evicts, and so whether it is a page of a memory region a publication has
// sealed.
var unsteadyProbes = []string{
	vmmemory.ProbeEvictionDuringPublication,
}

// elsewhere reports a probe another campaign is registered to cover, which
// these campaigns reach too: the peer server's (TestThePeerServerCampaignNeverAnswersWrong),
// the list of caches' (TestHostsAgreeOnceTheOrchestratorAnswersAgain), and
// the page cache disk's, its stripes', its fills' and its reads of the
// cluster (the disk, stripe, fill and read campaigns in checkpoint).
func elsewhere(name string) bool {
	for _, prefix := range []string{"peer/", "rank/", "checkpoint/disk-", "checkpoint/fill-", "checkpoint/keep-",
		"checkpoint/cluster-"} {
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
	for seed := uint64(1); seed <= 25; seed++ {
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
		if !slices.Contains(unsteadyProbes, name) {
			expected[name] = !slices.Contains(unreachedProbes, name)
		}
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
