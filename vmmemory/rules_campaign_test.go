package vmmemory_test

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// rulesCampaignSeeds is how many seeds the rules campaign runs: a rule meets
// another vCPU's store of the page it takes on three of the first sixty-four.
const rulesCampaignSeeds = 64

// rulesPages is each guest's memory in the rules campaign: four read-ahead
// windows of sixteen pages each, so the gap rule reaches across windows.
const rulesPages = 64

// The rules that keep a memory region's mappings whole survive guests that
// read and store at once. Each guest's region is near its mapping budget, so
// a store near a page its range already holds makes the pages between private
// in the same command, and those pages may be in another read-ahead window,
// which another vCPU of the guest may be storing into or a prefetch reading. No other campaign
// reaches the rules: they act only at a 4 KiB page, on a pressed range, with
// a read-ahead run shorter than the range. Every read must return what the
// guest stored or what its volume holds, and the rules must copy pages.
func TestTheMappingRulesSurviveTheirCampaign(t *testing.T) {
	copies := uint64(0)
	for seed := uint64(1); seed <= rulesCampaignSeeds; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var stats vmmemory.Stats
				runCampaign(t, seed, func(ctx context.Context, disk *sim.Disk) {
					stats = rulesWorld(t, ctx, seed, disk, 2, 2)
				})
				copies += stats.RuleCopies
			})
		})
	}
	if copies == 0 {
		t.Fatal("the campaign's rules copied no page: it never reached them")
	}
}

// A seed of the rules campaign replays (testReplays).
func TestRulesCampaignReplaysItsSeeds(t *testing.T) {
	testReplays(t, []uint64{2, 5}, func(t *testing.T, seed uint64) campaignRun {
		return runCampaign(t, seed, func(ctx context.Context, disk *sim.Disk) { rulesWorld(t, ctx, seed, disk, 2, 2) })
	})
}

// rulesWorld is the rules campaign's world on one seed: forks forks of one
// checkpoint at 4 KiB pages, each pressed for mappings, reading and storing
// on vcpus vCPUs at once over an arena too small for them, beside a disk and
// its flusher. It reports the host's stats once they are checked and
// detached.
func rulesWorld(t *testing.T, ctx context.Context, seed uint64, disk *sim.Disk, forks, vcpus int) vmmemory.Stats {
	logical := rulesPages * (forks + 2)
	// The isolated arena: in the shared one no store of this campaign is
	// placed at its own offset, so the rules never act.
	f, err := newFixtureOn(t, ctx, disk, vmmemory.Config{PageSize: checkpoint.PageSize4KiB, Arena: vmmemory.ArenaIsolated,
		ResidentPages: logical * 3 / 4, ArenaOffsets: 2 * logical, LogicalPages: logical, DirtyPages: logical,
		ReadAheadPages: 16, WriteAheadPages: 1, PrefetchRuns: 2, MaxSpillVersions: campaignVersions(seed)})
	if err != nil {
		// A host whose spill file could not be made never started.
		if !injected(err) {
			t.Error(err)
		}
		return vmmemory.Stats{}
	}
	// Production's client takes runs in batches; odd seeds take that path.
	f.batched = seed%2 == 1
	var guests []*campaignGuest
	attach := func(name string, kind vmmemory.MemoryRegionKind, b vmmemory.Backing) *campaignGuest {
		guest := f.attachGuest(name, kind, b, initialBytes(rulesPages))
		if guest != nil {
			guest.vcpus, guest.storeOdds = vcpus, 2
			guests = append(guests, guest)
		}
		return guest
	}
	for fork := range forks {
		b := f.slowBacking(rulesPages)
		b.admit = true
		if guest := attach("fork-"+string(rune('a'+fork)), vmmemory.Ram, b); guest != nil {
			guest.region.PressMappings()
		}
	}
	attach("disk", vmmemory.Pmem, f.slowBacking(rulesPages))
	runCampaignWorld(t, ctx, seed, guests)
	stats, err := f.h.Stats(ctx)
	if err != nil {
		t.Error(err)
	}
	return stats
}
