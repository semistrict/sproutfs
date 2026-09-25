package simtest_test

import (
	"fmt"
	"testing"

	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Every campaign over a block of the seed range rather than the fixed list the
// ordinary suite can afford, so a nightly sweep is one job per block and a
// bisect is one block of one. Each seed logs the simulated time it explored
// against the wall time it spent, which is the budget a scheduled job is sized
// against.

// TestSeededTopologySoak is the generated deployment and its concurrent faults.
func TestSeededTopologySoak(t *testing.T) {
	for seed := range testsoak.Require(t, 100).Seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			testsoak.Measure(t, campaignName, seed, func(t *testing.T) *sim.Runtime {
				return runTopologyCampaign(t, seed, false)
			})
		})
	}
}

// TestHostCrashSoak is the kill campaign. Each seed takes a host away in the
// middle of a checkpoint, of a fork point another host's child is reading,
// and of a migration from both ends, and requires the same four things of every
// one of them; a block is where the rarer landing moments are, since what a
// seed chooses is where inside the operation the kill falls.
func TestHostCrashSoak(t *testing.T) {
	interrupted, fired := map[string]int{}, map[string]uint64{}
	block := testsoak.Require(t, 50)
	for seed := range block.Seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			cut, sites := runCrashCampaign(t, seed)
			for name, count := range cut {
				interrupted[name] += count
			}
			for site, count := range sites {
				fired[site] += count
			}
		})
	}
	requireCrashCoverage(t, block.Count, interrupted, fired)
}

// TestSwizzleSoak is the two-writer campaign over a block of the seed range.
func TestSwizzleSoak(t *testing.T) {
	for seed := range testsoak.Require(t, 100).Seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			testsoak.Measure(t, swizzleCampaignName, seed, func(t *testing.T) *sim.Runtime {
				return runSwizzleCampaign(t, seed)
			})
		})
	}
}

// TestBuggifiedTopologySoak is the generated deployment with the per-site fault
// injection on over a block of seeds, which is where the sites a seed activates
// one quarter of at a time are all reached between them.
func TestBuggifiedTopologySoak(t *testing.T) {
	for seed := range testsoak.Require(t, 50).Seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			testsoak.Measure(t, buggifiedCampaignName, seed, func(t *testing.T) *sim.Runtime {
				return runTopologyCampaign(t, seed, true)
			})
		})
	}
}
