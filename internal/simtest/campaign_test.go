package simtest_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/internal/volume"
)

// campaignName is what this campaign's per-seed records are filed under in a
// sweep's summary.
const campaignName = "seeded-topology"

// TestSeededTopologyCampaign runs a deployment the seed generated — its hosts,
// its VMs, their sizes and which are forks of which — through a schedule of checkpoints,
// forks, migrations, deletes and host restarts, with one to three faults on at
// a time over it.
//
// The three requirements are the same however many faults are on: no guest
// reads bytes it never wrote, every VM's selected checkpoint is one a writer of
// it published, and what the store holds at the end is still a deployment.
func TestSeededTopologyCampaign(t *testing.T) {
	for _, seed := range []uint64{1, 7, 23} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			testsoak.Measure(t, campaignName, seed, func(t *testing.T) *sim.Runtime {
				return runTopologyCampaign(t, seed, false)
			})
		})
	}
}

// newCampaignRuntime is the simulated world a campaign that drives its
// operations one at a time runs on: a network and an object store whose
// latencies are microseconds, so the campaign explores its faults rather than
// its waits, and the per-site fault injection on or off.
func newCampaignRuntime(seed uint64, buggify bool) *sim.Runtime {
	return sim.New(sim.Config{Seed: seed, Buggify: buggify,
		Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
			ConnectLatency: time.Microsecond},
		ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
			PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
			BytesPerSecond: 1 << 40}})
}

// newCrashRuntime is the simulated world a campaign that kills a host at a
// drawn moment runs on. Its latencies are the hundreds of microseconds a real
// deployment's are rather than the ones a campaign driving whole operations can
// afford, because what a kill has to land inside is one of those operations:
// with nothing taking any time, every kill would arrive after the thing it was
// aimed at had already finished.
func newCrashRuntime(seed uint64) *sim.Runtime {
	return sim.New(sim.Config{Seed: seed, Buggify: true,
		Network: sim.NetworkConfig{Latency: 200 * time.Microsecond, Jitter: 20 * time.Microsecond,
			ConnectLatency: 200 * time.Microsecond},
		ObjectStore: sim.ObjectStoreConfig{HeadLatency: 50 * time.Microsecond,
			GetLatency: 100 * time.Microsecond, PutLatency: 200 * time.Microsecond,
			DeleteLatency: 100 * time.Microsecond, ListLatency: 100 * time.Microsecond,
			BytesPerSecond: 1 << 40}})
}

func newPrefix(t *testing.T, text string) platform.ObjectPrefix {
	t.Helper()
	prefix, err := platform.NewObjectPrefix(text)
	if err != nil {
		t.Fatal(err)
	}
	return prefix
}

// reportTrace prints the last of what the simulated dependencies did, which is
// what a failing seed is read back from.
func reportTrace(t *testing.T, runtime *sim.Runtime) {
	t.Helper()
	events := runtime.Trace().Events()
	for _, event := range events[max(0, len(events)-40):] {
		t.Logf("sim: %+v", event)
	}
}

// runTopologyCampaign is one seed: the topology it generates, the world that
// runs it, the faults it injects over it, and the deployment check that has the
// last word.
func runTopologyCampaign(t *testing.T, seed uint64, buggify bool) *sim.Runtime {
	t.Helper()
	runtime := newCampaignRuntime(seed, buggify)
	prefix := newPrefix(t, "sproutfs/")
	topology := simtest.NewTopology(runtime.Random("simtest/topology"))
	t.Logf("seed=%d topology: %s", seed, topology)
	// Everything below runs under the simulator's own context, which is what
	// the probes and the buggified sites inside the real host, volume,
	// checkpoint, control, pager and migration code consult.
	ctx := sim.WithRuntime(t.Context(), runtime)
	world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: campaignKnobs(t, runtime, topology), Prefix: prefix, Log: t.Logf})
	driver := simtest.NewDriver(world, runtime.Random("simtest/schedule"),
		simtest.Faults(runtime.Random("simtest/faults"), topology), t.Logf)
	runErr := driver.Run(ctx)
	// The world is closed whatever the run did, because the deployment check
	// reads a publication still in flight as one that never finished, and
	// because an unclosed world leaves goroutines in the bubble.
	closeErr := world.Close(ctx)
	if runErr != nil {
		t.Errorf("seed=%d: %v", seed, runErr)
	}
	if closeErr != nil {
		t.Errorf("seed=%d: closing the world: %v", seed, closeErr)
	}
	// Whatever these faults did, what the store holds at the end must still be
	// a deployment. Every allowance is a debt this campaign's faults create and
	// no writer ever comes back for — a collector's, not a writer's: a host
	// lost at a moment leaves a superseded epoch's checkpoints and a
	// publication interrupted between its parts and its index, a VM deleted
	// after it was forked leaves the checkpoints its pin protects, and a sweep the
	// store refused leaves the checkpoint it replaced behind.
	if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
		volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex, volume.AllowUnrecordedVM,
		volume.AllowUnreferencedCheckpoint); err != nil {
		t.Errorf("seed=%d: %v", seed, err)
	}
	if t.Failed() {
		reportTrace(t, runtime)
	}
	t.Logf("seed=%d fingerprint=%#016x probes=%v", seed, runtime.Fingerprint(), runtime.Probes())
	return runtime
}

// knobsEnabled reports the opt-in that lets a campaign draw a seed's own
// tunables rather than the deployment's.
func knobsEnabled() bool { return os.Getenv("SPROUTFS_TEST_KNOBS") != "" }

// campaignLossWindow is the seed's choice between turning the bound off and
// leaving it wider than anything a campaign's clocks reach. These worlds run no
// checkpoint loop — they drive every checkpoint themselves — so a window that
// actually fired here would hold a guest back waiting for a checkpoint nobody
// takes, which ends in the deliberate stop the loss-window scenarios are about
// and which this model does not follow. What a campaign requires of the window
// is that every host, pager, migration and handoff carries it through every
// kill and every swizzle, and that no recovery ever rewinds more than it allows.
func campaignLossWindow(runtime *sim.Runtime) time.Duration {
	// A campaign advances a host's clock by four checkpoint intervals to reach a
	// handover's deadline, so every window it may draw is wider than that.
	choices := []time.Duration{0, 5 * time.Minute, time.Hour, 24 * time.Hour}
	return choices[runtime.Random("simtest/loss-window").Intn("window", len(choices))]
}

// campaignKnobs is the set of tunables one seed runs with. The pager's arena
// has to hold every VM of the topology twice over — a fork or a migration has
// the parent's pages and the child's on one host at once — so the arena and
// the budgets within it are the topology's rather than the seed's. Everything
// else is drawn under the opt-in: the part size, the index bound, the
// upload and builder budgets, the write bound, the page cache and the I/O
// budget.
func campaignKnobs(t *testing.T, runtime *sim.Runtime, topology simtest.Topology) knobs.Knobs {
	t.Helper()
	k := knobs.Defaults()
	if knobsEnabled() {
		k = knobs.Randomize(runtime.Random("simtest/knobs"))
	}
	pages := 0
	for _, vm := range topology.VMs {
		pages += vm.Pages()
	}
	// There is no interval loop here to answer the pager's pressure — these
	// campaigns drive their own checkpoints — so a budget smaller than what the
	// guests on one host can dirty stalls a store on a checkpoint nobody is
	// going to take.
	k.ResidentPages = 2*pages + 8
	k.DirtyPages = k.ResidentPages
	k.LogicalPages = 4 * k.ResidentPages
	// One page per store: the model counts what a source holds against what its
	// guest wrote, which read-ahead and write-ahead would round up to their
	// runs.
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	if window := campaignLossWindow(runtime); window > 0 {
		// A window is never shorter than the interval a VM is checkpointed on,
		// and a seed may have drawn an interval of an hour.
		k.LossWindow = max(window, k.CheckpointInterval)
	} else {
		k.LossWindow = 0
	}
	// Two VMs are open on one host at once, and a takeover holds the superseded
	// handle beside the one that fenced it.
	k.MaxOpenVMs = max(k.MaxOpenVMs, 8)
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	if knobsEnabled() {
		t.Logf("knobs=%v", k.Changed())
	}
	return k
}
