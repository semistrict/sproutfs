package simtest_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/internal/volume"
)

// crashCampaignName is what this campaign's per-seed records are filed under in
// a sweep's summary.
const crashCampaignName = "host-crash"

const (
	crashVMID    = "vm-1"
	crashChildID = "vm-1-child"
	// crashHosts is the deployment: the VM's own host, the host it is handed to
	// and a bystander that takes no part in any scenario, so there is always a
	// host that can recover a VM whose own hosts are gone.
	crashHosts = 3
)

// crashPages is the VM a kill campaign runs: whole pager pages, each written
// with the generation that wrote it, so a recovered page names the checkpoint
// it came from and two checkpoints mixed into one VM are visible at a glance.
const crashPages = 4

// crashWindow is the span a seed draws the kill's instant from. It is virtual
// time, and it is wider than any of these operations takes, so some seeds kill
// before the operation began, some in the middle of it and some after it
// finished. The requirements do not move between those.
const crashWindow = 2 * time.Millisecond

// handoffIntervals is how many checkpoint intervals a handover is served for
// before the host gives it up on its own, which is the host package's own
// bound.
const handoffIntervals = 4

// crashScenarios is where a host can be lost. Each stages the deployment,
// starts the thing its victim is in the middle of, and kills that host at an
// instant the seed chooses. What has to hold afterwards is the same for all of
// them.
var crashScenarios = []struct {
	name string
	// stage runs the operation and the kill inside it, and reports the host it
	// took away and whether the kill landed inside the operation.
	stage func(c *crashRun) (int, bool)
}{
	{name: "mid-checkpoint", stage: (*crashRun).killDuringACheckpoint},
	{name: "holding-a-fork-point", stage: (*crashRun).killHoldingAForkPoint},
	{name: "migration-source", stage: (*crashRun).killTheMigrationSource},
	{name: "migration-destination", stage: (*crashRun).killTheMigrationDestination},
}

// TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld is the kill
// campaign, as a schedule over one world. Every seed runs every scenario: a
// host is taken away in the middle of a checkpoint, while it holds a fork
// instant another host's child is still reading, while it is serving the pages
// of a VM it handed over, and while it is the host taking one in. The kill
// lands at an instant the seed draws and in a mode the seed draws, the host
// comes back in this process on the disk it left behind, and then the VM is
// recovered — by a bystander or by that restart, as the seed chooses.
//
// Whatever the kill interrupted, the requirements are the same four:
//
//   - the VM reads as one whole generation, never two mixed and never a byte no
//     guest wrote, both through its volume and through a guest's own fault path;
//   - that generation is one this VM published — no older than the last
//     checkpoint that was acknowledged and no newer than the last one written;
//   - the recovered VM can be published again and reads the same afterwards;
//   - the store still holds a deployment, and every kill is in the trace.
func TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld(t *testing.T) {
	seeds := uint64(8)
	if value := os.Getenv("SPROUTFS_CRASH_SEEDS"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil || parsed == 0 {
			t.Fatal("SPROUTFS_CRASH_SEEDS must be positive")
		}
		seeds = parsed
	}
	interrupted, fired := map[string]int{}, map[string]uint64{}
	for seed := uint64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed-%02d", seed), func(t *testing.T) {
			cut, sites := runCrashCampaign(t, seed)
			for name, count := range cut {
				interrupted[name] += count
			}
			for site, count := range sites {
				fired[site] += count
			}
		})
	}
	requireCrashCoverage(t, seeds, interrupted, fired)
}

// requireCrashCoverage is what a block of seeds owes the campaign: kills that
// landed inside every scenario rather than after it, and production code that
// was itself misbehaving while they did.
func requireCrashCoverage(t *testing.T, seeds uint64, interrupted map[string]int, fired map[string]uint64) {
	t.Helper()
	// The campaign runs with the per-site injection on, so the kills land
	// around production code that is itself misbehaving. A run where no site
	// fired is a run with the switch broken rather than a quieter one.
	if len(fired) == 0 {
		t.Error("no buggified site fired across any seed of the kill campaign")
	}
	t.Logf("buggified sites fired: %v", fired)
	// A kill that always lands after its operation had finished tests nothing an
	// orderly close does not. Between them the seeds have to cut into every one
	// of these scenarios, or the scenario is a name and not a fault.
	for _, scenario := range crashScenarios {
		if interrupted[scenario.name] == 0 {
			t.Errorf("across %d seeds no kill landed inside %s", seeds, scenario.name)
		}
		t.Logf("%s: %d of %d seeds killed inside the operation", scenario.name,
			interrupted[scenario.name], seeds)
	}
}

// runCrashCampaign runs every scenario for one seed and reports which of them
// the kill cut into, and which per-site faults fired while it did.
func runCrashCampaign(t *testing.T, seed uint64) (map[string]int, map[string]uint64) {
	t.Helper()
	interrupted := map[string]int{}
	var runtime *sim.Runtime
	testsoak.Measure(t, crashCampaignName, seed, func(t *testing.T) *sim.Runtime {
		runtime = newCrashRuntime(seed)
		for _, scenario := range crashScenarios {
			if runCrashScenario(t, runtime, seed, scenario.name, scenario.stage) {
				interrupted[scenario.name]++
			}
		}
		return runtime
	})
	return interrupted, runtime.FiredSites()
}

// crashRun is one scenario's whole deployment: three hosts on the simulator,
// each with a process, a disk, a clock and a pager of its own, and one VM whose
// guest has written a generation no checkpoint holds.
type crashRun struct {
	t      *testing.T
	ctx    context.Context
	world  *simtest.World
	random sim.Random
}

// runCrashScenario stages one scenario, kills its host, restarts it and
// requires what the deployment holds afterwards. It reports whether the kill
// actually cut into the operation it was aimed at rather than landing after it
// had finished.
func runCrashScenario(t *testing.T, runtime *sim.Runtime, seed uint64, name string,
	stage func(*crashRun) (int, bool)) bool {
	t.Helper()
	prefix := newPrefix(t, "crash/"+name+"/")
	// Everything below runs under the simulator's own context, which is what
	// the probes and the buggified sites inside the real host, volume,
	// checkpoint, control, pager and migration code consult.
	ctx := sim.WithRuntime(t.Context(), runtime)
	hosts := make([]string, crashHosts)
	for n := range hosts {
		hosts[n] = fmt.Sprintf("host-%d", n)
	}
	topology := simtest.Topology{Hosts: hosts,
		VMs: []simtest.VMSpec{{ID: crashVMID, Host: 0,
			Volumes: []volume.VolumeSpec{{Name: "ram0", Size: crashPages * simtest.PageSize}}}}}
	world, err := simtest.Start(ctx, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: campaignKnobs(t, runtime, topology), Prefix: prefix, Namespace: name + "/",
		Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	c := &crashRun{t: t, ctx: ctx, world: world,
		random: runtime.Random("campaign/crash/" + name)}
	// One generation the guest stored and published, so it is durable wherever
	// this VM is recovered, and one it stored afterwards, which lives only in
	// the frames of the host that is about to die.
	c.generation(1)
	if err := world.Checkpoint(ctx, crashVMID); err != nil {
		t.Fatal(err)
	}
	c.generation(2)

	before := len(kills(runtime))
	victim, cut := stage(c)
	traced := kills(runtime)[before:]
	if len(traced) != 1 {
		t.Fatalf("%s: the scenario traced %d kills, want the one it staged", name, len(traced))
	}
	if want := name + "/host-" + strconv.Itoa(victim); traced[0].Resource != want {
		t.Fatalf("%s: the kill was traced against %s, want %s", name, traced[0].Resource, want)
	}
	t.Logf("%s: host-%d killed by %s", name, victim, traced[0].Outcome)

	// A VM the dead host wrote for is recovered by a bystander or by that
	// restart. Which of the two is the seed's choice: both are ways a
	// deployment finds a VM whose host is gone.
	if c.random.Chance("recover/bystander", 0.5) {
		if err := world.Settle(ctx); err != nil {
			t.Fatalf("%s: a bystander could not recover what the kill left: %v", name, err)
		}
	}
	// The host comes back in this process, on the disk its own kill left behind.
	if err := world.Restart(ctx, victim); err != nil {
		t.Fatal(err)
	}
	if got := world.Incarnation(victim); got != 2 {
		t.Fatalf("%s: the victim came back as incarnation %d, want its second", name, got)
	}
	if err := world.Settle(ctx); err != nil {
		t.Fatalf("%s: the deployment could not recover what the kill left: %v", name, err)
	}
	// Every page of every VM still here reads as one checkpoint of it, through
	// a guest's own fault path and again through its volume.
	c.requireRecovered(name)
	// What was recovered publishes again and reads the same afterwards, because
	// a state no host can make durable is not a recovery.
	for _, id := range world.Running() {
		if err := world.Checkpoint(ctx, id); err != nil {
			t.Fatalf("%s: %s could not be published again: %v", name, id, err)
		}
	}
	c.requireRecovered(name)
	if err := world.CheckSelected(ctx); err != nil {
		t.Errorf("%s: %v", name, err)
	}
	if err := world.Close(ctx); err != nil {
		t.Errorf("%s: closing the world: %v", name, err)
	}
	// Every class the allowances name is a collector's debt a host lost at a
	// particular instant leaves and no writer ever comes back for: the
	// checkpoint a takeover opened on, the parts of a publication whose index
	// never landed, a checkpoint no sweep came back for, and the objects of a
	// child whose record the destination never wrote.
	if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
		volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex,
		volume.AllowUnreferencedCheckpoint, volume.AllowUnrecordedVM); err != nil {
		t.Errorf("seed=%d %s: %v", seed, name, err)
	}
	if t.Failed() {
		reportTrace(t, runtime)
	}
	return cut
}

// generation writes one whole generation into every page of the VM's memory.
func (c *crashRun) generation(value byte) {
	c.t.Helper()
	if err := c.world.StoreAll(crashVMID, value); err != nil {
		c.t.Fatal(err)
	}
}

// requireRecovered requires every VM still in the deployment to read as one
// whole generation the guest actually wrote — through a guest's own fault path
// and again through its volume, which are two different claims.
func (c *crashRun) requireRecovered(name string) {
	c.t.Helper()
	if err := c.world.Verify(c.ctx, simtest.ReadsMustSucceed); err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	for _, id := range c.world.Running() {
		if err := c.world.VerifyDurable(c.ctx, id); err != nil {
			c.t.Fatalf("%s: %v", name, err)
		}
	}
}

// kill is the mode and instant this seed takes a host away at.
func (c *crashRun) kill(victim int, operation func(context.Context) error) (int, bool) {
	c.t.Helper()
	mode := sim.CrashProcess
	if c.random.Chance("kill/power-loss", 0.5) {
		mode = sim.PowerLoss
	}
	cut, err := c.world.KillDuring(c.ctx, victim, mode,
		c.random.Duration("kill/at", crashWindow), operation)
	c.t.Logf("what the kill interrupted ended with %v", err)
	return victim, cut
}

// killDuringACheckpoint loses the host while it is publishing the generation
// its guest holds. The checkpoint either landed or it did not; what it must
// never be is half of one.
func (c *crashRun) killDuringACheckpoint() (int, bool) {
	return c.kill(0, func(ctx context.Context) error { return c.world.Checkpoint(ctx, crashVMID) })
}

// killHoldingAForkPoint loses the host while it holds a fork instant another
// host's child is still reading out of. The parent is sealed and the child has
// no root index of its own until it publishes one, so this is the moment a host
// loss costs the most.
func (c *crashRun) killHoldingAForkPoint() (int, bool) {
	child := simtest.VMSpec{ID: crashChildID, Parent: crashVMID, Host: 1}
	return c.kill(0, func(ctx context.Context) error { return c.world.Fork(ctx, child) })
}

// killTheMigrationSource loses the host that is serving the pages of a VM it
// handed over. Those pages exist nowhere else, so a destination that cannot
// fetch them has a guest it can never publish and gives it up.
func (c *crashRun) killTheMigrationSource() (int, bool) {
	return c.kill(0, func(ctx context.Context) error { return c.world.Migrate(ctx, crashVMID, 1) })
}

// killTheMigrationDestination loses the host that is taking a VM in. The source
// has already given the VM up and is only holding frames now, for a destination
// that will never report having them; the deadline that gives those frames back
// is four checkpoint intervals, and it is reached here by advancing the clock
// the source keeps it against rather than by waiting four minutes for it.
func (c *crashRun) killTheMigrationDestination() (int, bool) {
	victim, cut := c.kill(1, func(ctx context.Context) error {
		return c.world.Migrate(ctx, crashVMID, 1)
	})
	source := c.world.Host(0)
	serving := source.Status().Serving
	released := c.world.Clock(0).Advance(handoffIntervals * host.DefaultCheckpointInterval)
	c.world.Clock(0).Settle()
	if len(serving) > 0 && released == 0 {
		c.t.Fatalf("the source serves %v for a destination that is gone and keeps no deadline for them", serving)
	}
	if left := source.Status().Serving; len(left) != 0 {
		c.t.Fatalf("the source still serves %v past the deadline of its handover", left)
	}
	return victim, cut
}

// kills is every host loss the run traced, in order. A host cannot be taken
// away without the trace saying which one and how, which is what lets a
// campaign that kills hosts at seeded instants say afterwards what it did.
func kills(runtime *sim.Runtime) []sim.Event {
	var found []sim.Event
	for _, event := range runtime.Trace().Events() {
		if event.Kind == "process" && event.Operation == "crash" {
			found = append(found, event)
		}
	}
	return found
}
