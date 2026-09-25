package sim_test

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
)

// A site outside a simulation costs one context lookup and answers no. That is
// what lets these calls live in the production code they are meant to perturb.
func TestFaultInjectionIsInertWithoutARuntime(t *testing.T) {
	ctx := t.Context()
	if sim.Buggify(ctx, "any/site", 1) {
		t.Fatal("a buggified site fired with no runtime in the context")
	}
	if sim.Bug(ctx, "any-bug") {
		t.Fatal("a bug guard answered yes with no runtime in the context")
	}
	if err := sim.BuggifyDelay(ctx, "any/site", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	sim.Probe(ctx, "any/probe")
}

// The switch is the campaign's, not the seed's: a runtime that was not asked
// for fault injection has none, whatever the seed would have activated.
func TestBuggifyIsOffUntilACampaignAsksForIt(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1})
	ctx := sim.WithRuntime(t.Context(), runtime)
	for range 100 {
		if sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 1) {
			t.Fatal("a site fired with the switch off")
		}
	}
	if len(runtime.Trace().Events()) != 0 {
		t.Fatalf("an inactive site recorded %d events", len(runtime.Trace().Events()))
	}
	runtime.SetBuggify(true)
	if !sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 1) {
		t.Fatal("seed 1 does not activate this site; pick one it does")
	}
}

// Activation is drawn once per site per run from the seed and the site's id
// alone, so adding a site cannot change which sites another seed activates, and
// a site reached a thousand times is still activated the once.
func TestBuggifyActivatesASiteOncePerRun(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1, Buggify: true})
	ctx := sim.WithRuntime(t.Context(), runtime)
	for range 50 {
		sim.Buggify(ctx, "checkpoint/one-page-parts", 0.5)
	}
	activations := 0
	for _, event := range runtime.Trace().Events() {
		if event.Kind == "buggify" && event.Operation == "activate" {
			activations++
		}
	}
	if activations != 1 {
		t.Fatalf("the site was activated %d times, want once", activations)
	}
}

// A quarter of the sites, like FoundationDB's. The exact fraction matters less
// than that it is neither all of them — every seed exploring every fault at
// once — nor none.
func TestBuggifyActivatesAboutAQuarterOfTheSites(t *testing.T) {
	activated := 0
	const seeds = 400
	for seed := uint64(1); seed <= seeds; seed++ {
		runtime := sim.New(sim.Config{Seed: seed, Buggify: true})
		if sim.Buggify(sim.WithRuntime(t.Context(), runtime), "checkpoint/one-page-parts", 0) {
			t.Fatal("a site with probability zero fired")
		}
		if runtime.BuggifySites()["checkpoint/one-page-parts"] {
			activated++
		}
	}
	if activated < seeds/5 || activated > seeds/3 {
		t.Fatalf("%d of %d seeds activated the site, want about a quarter", activated, seeds)
	}
}

// An activated site draws its firing per call, so a run explores both the
// faulty and the ordinary path at one site rather than one of them for ever.
func TestAnActivatedSiteFiresOnSomeCallsAndNotOthers(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1, Buggify: true})
	ctx := sim.WithRuntime(t.Context(), runtime)
	fired := 0
	for range 200 {
		if sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5) {
			fired++
		}
	}
	if fired < 60 || fired > 140 {
		t.Fatalf("an activated site at probability 0.5 fired %d times in 200", fired)
	}
	if runtime.FiredSites()["vmmemory/evict-past-a-free-slot"] != uint64(fired) {
		t.Fatalf("the runtime counted %d firings, want %d",
			runtime.FiredSites()["vmmemory/evict-past-a-free-slot"], fired)
	}
}

// The same seed makes the same decisions, and a different seed does not.
func TestBuggifyDecisionsFollowTheSeed(t *testing.T) {
	decisions := func(seed uint64) []bool {
		runtime := sim.New(sim.Config{Seed: seed, Buggify: true})
		ctx := sim.WithRuntime(t.Context(), runtime)
		var fired []bool
		for range 40 {
			fired = append(fired, sim.Buggify(ctx, "control/slow-write", 0.5))
		}
		return fired
	}
	if !slices.Equal(decisions(3), decisions(3)) {
		t.Fatal("one seed made different decisions on two runs")
	}
	if slices.Equal(decisions(4), decisions(16)) {
		t.Fatal("two seeds made the same forty decisions")
	}
}

// A delay fires with the site and is bounded by what the caller allows.
func TestBuggifyDelayWaitsOnlyWhenItFires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Seed 7 is one that activates this site; a seed that does not would
		// make every call below return at once.
		runtime := sim.New(sim.Config{Seed: 7, Buggify: true})
		ctx := sim.WithRuntime(t.Context(), runtime)
		waited := time.Duration(0)
		for range 20 {
			start := time.Now()
			if err := sim.BuggifyDelay(ctx, "control/slow-write", 1, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			waited += time.Since(start)
		}
		if waited == 0 {
			t.Fatal("no call of an activated delay site waited")
		}
		if waited > 20*5*time.Second {
			t.Fatalf("the delays totalled %v, past the twenty five-second bounds allowed", waited)
		}
	})
}

// A canceled context is reported rather than slept through.
func TestBuggifyDelayHonoursCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 7, Buggify: true})
		ctx, cancel := context.WithCancel(sim.WithRuntime(t.Context(), runtime))
		cancel()
		var err error
		for range 20 {
			if err = sim.BuggifyDelay(ctx, "control/slow-write", 1, time.Hour); err != nil {
				break
			}
		}
		if err == nil {
			t.Fatal("twenty delays under a canceled context all returned nil")
		}
	})
}

// Probes are counted on the runtime rather than traced, so registering one
// changes no recording and adds no event to any fingerprint.
func TestProbesAreCountedOffTheTrace(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1})
	ctx := sim.WithRuntime(t.Context(), runtime)
	before := runtime.Fingerprint()
	sim.Probe(ctx, "checkpoint/compaction-rewrite")
	sim.Probe(ctx, "checkpoint/compaction-rewrite")
	sim.Probe(ctx, "vmmigrate/volume-fallback")
	if runtime.Fingerprint() != before {
		t.Fatal("a probe changed the trace fingerprint")
	}
	if got := runtime.Probes(); got["checkpoint/compaction-rewrite"] != 2 || got["vmmigrate/volume-fallback"] != 1 {
		t.Fatalf("probe counts %v", got)
	}
	registered := []string{"checkpoint/compaction-rewrite", "vmmigrate/volume-fallback", "control/publication-fenced"}
	if missed := runtime.MissedProbes(registered); !slices.Equal(missed, []string{"control/publication-fenced"}) {
		t.Fatalf("missed probes %v, want only the one nothing reached", missed)
	}
}

// A guard is named by the environment, not drawn from the seed: a negative test
// installs exactly the bug it is about and nothing else.
func TestBugGuardsAreTheOnesTheEnvironmentNames(t *testing.T) {
	t.Setenv("SPROUTFS_SIM_BUG", " volume-shift-write , checkpoint-reclaim-live-checkpoint ,")
	runtime := sim.New(sim.Config{Seed: 1})
	ctx := sim.WithRuntime(t.Context(), runtime)
	if got := runtime.Bugs(); !slices.Equal(got, []string{"checkpoint-reclaim-live-checkpoint", "volume-shift-write"}) {
		t.Fatalf("the runtime read %v from the environment", got)
	}
	if !sim.Bug(ctx, "volume-shift-write") || !sim.Bug(ctx, "checkpoint-reclaim-live-checkpoint") {
		t.Fatalf("a named guard answered no: %v", runtime.Bugs())
	}
	if sim.Bug(ctx, "volume-ignore-discard") {
		t.Fatal("a guard the environment did not name answered yes")
	}
}
