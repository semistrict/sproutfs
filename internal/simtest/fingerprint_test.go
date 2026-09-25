package simtest_test

import (
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform/sim"
)

// connectionAttempt is the one class of event this campaign's own concurrency
// decides rather than its seed, and the reason the strict fingerprint is logged
// rather than asserted. A destination's memory regions each ask the source for their
// own pages, and a source that is being taken away — closed, partitioned, or
// losing a reply — is discovered independently by each of them: how many of
// them dial before the first failure marks the source fallen is a race between
// goroutines, not a choice the seed made. Every attempt after that answer is
// refused and changes nothing, but the attempts cost connect latency, so the
// simulated clock and the network's own operation numbering move with them.
//
// Everything else in the run is identical between two runs of a seed: every
// object-store request, every disk operation, every page served, every byte and
// every outcome. That is what the work fingerprint asserts.
func connectionAttempt(e sim.Event) bool { return e.Kind == "network" && e.Operation == "dial" }

// The campaign's promise is that a seed reproduces a run, and this is what
// checks it: FoundationDB's unseed comparison, run in-process for every seed
// rather than on a sample of them.
func TestSeededTopologyFingerprintIsStable(t *testing.T) {
	for _, seed := range []uint64{1, 23} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			var work, strict [2]uint64
			var dials [2]int
			var memoryRegions int
			for run := range work {
				synctest.Test(t, func(t *testing.T) {
					runtime := runTopologyCampaign(t, seed, false)
					trace := runtime.Trace()
					work[run] = trace.WorkFingerprint(func(e sim.Event) bool { return !connectionAttempt(e) })
					strict[run] = trace.Fingerprint()
					for _, event := range trace.Events() {
						if connectionAttempt(event) {
							dials[run]++
						}
					}
					memoryRegions = memoryRegionsOf(simtest.NewTopology(runtime.Random("simtest/topology")))
				})
			}
			t.Logf("seed=%d work=%#016x strict=%v connection-attempts=%v", seed, work[0], strict, dials)
			if work[0] != work[1] {
				t.Fatalf("seed %d did different work on its second run: %#016x then %#016x", seed, work[0], work[1])
			}
			// The excluded class cannot grow quietly: one memory region per volume of
			// the topology may race one failure, so a handful of extra attempts
			// across a whole schedule is the whole of what the fingerprint above
			// hides.
			if difference := max(dials[0], dials[1]) - min(dials[0], dials[1]); difference > memoryRegions {
				t.Fatalf("seed %d varied by %d connection attempts across two runs (%v), more than the %d memory regions that can race one source's loss",
					seed, difference, dials, memoryRegions)
			}
		})
	}
}

// memory regionsOf is every memory region of a topology, which is how many callers can
// independently discover one source being taken away.
func memoryRegionsOf(topology simtest.Topology) int {
	memoryRegions := 0
	for _, vm := range topology.VMs {
		memoryRegions += len(vm.Volumes)
	}
	return memoryRegions
}
