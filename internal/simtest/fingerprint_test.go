package simtest_test

import (
	"fmt"
	"maps"
	"slices"
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

// fingerprintShake is the shake of a seed's second run. The first runs
// unshaken.
const fingerprintShake = 0x9e3779b97f4a7c15

// The campaign's promise is that a seed reproduces a run, and this is what
// checks it: FoundationDB's unseed comparison, run in-process for every seed
// rather than on a sample of them. Each seed runs with no cache disk, with
// the cluster cache on, where hosts fill each other beside everything else
// they do, through a hot tier, and with the cache on shards, which move
// between the hosts as they are lost and started again.
//
// The second run of each is shaken: goroutines ready at one simulated instant
// reach the simulated dependencies in another order than the first run's. A
// race the seed does not decide shows up here on an idle machine, rather than
// only on a loaded one, where the Go scheduler happens to order the two runs
// differently by itself.
func TestSeededTopologyFingerprintIsStable(t *testing.T) {
	for _, seed := range []uint64{1, 23} {
		for _, cache := range []campaignCache{cacheOff, cacheOn, cacheHot, cacheShards} {
			t.Run(fmt.Sprintf("seed-%d/cache-%s", seed, cache), func(t *testing.T) {
				var work, strict [2]uint64
				var dials [2]int
				var events [2][]sim.Event
				var memoryRegions int
				for run := range work {
					synctest.Test(t, func(t *testing.T) {
						shake := uint64(0)
						if run == 1 {
							shake = fingerprintShake
						}
						runtime := runShakenTopologyCampaign(t, seed, false, cache, shake)
						trace := runtime.Trace()
						work[run] = trace.WorkFingerprint(func(e sim.Event) bool { return !connectionAttempt(e) })
						strict[run] = trace.Fingerprint()
						events[run] = trace.Events()
						for _, event := range events[run] {
							if connectionAttempt(event) {
								dials[run]++
							}
						}
						memoryRegions = memoryRegionsOf(simtest.NewTopology(runtime.Random("simtest/topology")))
					})
				}
				t.Logf("seed=%d work=%#016x strict=%v connection-attempts=%v", seed, work[0], strict, dials)
				if work[0] != work[1] {
					for _, line := range workDifference(events[0], events[1]) {
						t.Log(line)
					}
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

// workDifference is the work one run did that the other did not, as the work
// fingerprint counts it: how many times each run did each operation, to
// which outcome and over how many bytes, wherever the two differ. A failing
// seed is read back from it.
func workDifference(first, second []sim.Event) []string {
	count := func(events []sim.Event) map[string]int {
		work := map[string]int{}
		for _, e := range events {
			if !connectionAttempt(e) {
				work[fmt.Sprintf("%s %s %s %s %d bytes", e.Kind, e.Resource, e.Operation, e.Outcome, e.Bytes)]++
			}
		}
		return work
	}
	firstWork, secondWork := count(first), count(second)
	keys := slices.Collect(maps.Keys(firstWork))
	for key := range secondWork {
		if _, both := firstWork[key]; !both {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var lines []string
	for _, key := range keys {
		if firstWork[key] != secondWork[key] {
			lines = append(lines, fmt.Sprintf("first run %d, second run %d: %s", firstWork[key], secondWork[key], key))
		}
	}
	return lines
}
