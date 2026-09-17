package knobs_test

import (
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// The defaults are what a deployment runs, so a run with these knobs must be
// indistinguishable from a run without them: anything Validate refuses here is
// a default the packages themselves would refuse.
func TestDefaultsAreValidAndChangeNothing(t *testing.T) {
	if err := knobs.Defaults().Validate(); err != nil {
		t.Fatal(err)
	}
	if changed := knobs.Defaults().Changed(); len(changed) != 0 {
		t.Fatalf("the defaults report %v as changed", changed)
	}
	if got := knobs.Defaults().HoldTimeout(); got.Minutes() != 4 {
		t.Fatalf("a handover is held for %s, want the four checkpoint intervals", got)
	}
}

// Every seed must produce knobs the packages will accept. A randomized set that
// cannot be run is a campaign that reports a configuration error as a bug.
func TestRandomizedKnobsAreAlwaysValid(t *testing.T) {
	for seed := range uint64(256) {
		k := knobs.Randomize(sim.New(sim.Config{Seed: seed + 1}).Random("knobs"))
		if err := k.Validate(); err != nil {
			t.Fatalf("seed %d: %v\n%s", seed+1, err, k)
		}
	}
}

// One seed must reproduce one set of knobs, and different seeds must reach
// different ones: a campaign that reported a seed whose knobs it cannot
// reconstruct has reported nothing.
func TestRandomizedKnobsAreReproducibleAndVary(t *testing.T) {
	draw := func(seed uint64) knobs.Knobs {
		return knobs.Randomize(sim.New(sim.Config{Seed: seed}).Random("knobs"))
	}
	if first, again := draw(11), draw(11); first != again {
		t.Fatalf("seed 11 drew %s then %s", first, again)
	}
	distinct := map[string]bool{}
	for seed := range uint64(64) {
		distinct[draw(seed+1).String()] = true
	}
	if len(distinct) < 32 {
		t.Fatalf("64 seeds reached only %d distinct sets of knobs", len(distinct))
	}
}

// The point of randomizing is the boundary a constant hides. Across a modest
// seed range the campaign must actually reach the smallest part size, the
// smallest dirty budget and the shortest hold — the values whose paths a
// deployment's defaults never take.
func TestRandomizedKnobsReachTheBoundaries(t *testing.T) {
	var smallestPart, smallestDirty, shortestHold, singleBuilder bool
	for seed := range uint64(128) {
		k := knobs.Randomize(sim.New(sim.Config{Seed: seed + 1}).Random("knobs"))
		smallestPart = smallestPart || k.PartBytes == 1
		smallestDirty = smallestDirty || k.DirtyPages == 1
		shortestHold = shortestHold || k.HoldIntervals == 1
		singleBuilder = singleBuilder || k.MaxBuilders == 1
	}
	if !smallestPart {
		t.Error("128 seeds never drew one member per part")
	}
	if !smallestDirty {
		t.Error("128 seeds never gave the pager a single dirty page")
	}
	if !shortestHold {
		t.Error("128 seeds never held a handover for one checkpoint interval")
	}
	if !singleBuilder {
		t.Error("128 seeds never serialized publication on one part builder")
	}
}

// A knob nobody touched keeps its default, and the ones a seed moved are named
// with their values: a failing campaign prints this line, and it has to be
// enough to rebuild the run by hand.
func TestChangedNamesOnlyWhatMoved(t *testing.T) {
	k := knobs.Defaults()
	k.PartBytes = 1
	k.DirtyPages = 3
	changed := k.Changed()
	if !slices.Contains(changed, "part-bytes=1") || !slices.Contains(changed, "dirty-pages=3") {
		t.Fatalf("Changed reported %v", changed)
	}
	if len(changed) != 2 {
		t.Fatalf("Changed reported %v, want only the two that moved", changed)
	}
}

// Validate is what stands between a campaign and a run that cannot start, so
// each rule it enforces is asserted rather than assumed.
func TestValidateRefusesContradictoryKnobs(t *testing.T) {
	cases := map[string]func(*knobs.Knobs){
		"a resident arena larger than the metadata cap that describes it": func(k *knobs.Knobs) {
			k.LogicalPages, k.ResidentPages = 8, 9
		},
		"a dirty budget larger than the metadata cap": func(k *knobs.Knobs) {
			k.LogicalPages, k.DirtyPages = 8, 9
		},
		"a read-ahead run that is not a power of two": func(k *knobs.Knobs) { k.ReadAheadPages = 3 },
		"a write smaller than one sector":             func(k *knobs.Knobs) { k.MaxWriteBytes = 512 },
		"a per-VM drain bound above the whole drain's": func(k *knobs.Knobs) {
			k.DrainPerVM = k.DrainTimeout + 1
		},
		"a checkpoint interval of zero": func(k *knobs.Knobs) { k.CheckpointInterval = 0 },
		"a hold of no intervals":        func(k *knobs.Knobs) { k.HoldIntervals = 0 },
	}
	for name, break_ := range cases {
		k := knobs.Defaults()
		break_(&k)
		if err := k.Validate(); err == nil {
			t.Errorf("Validate accepted %s", name)
		}
	}
}
