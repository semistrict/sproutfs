package simtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// The shape of one seed's schedule. A step is one operation with a few stores
// in front of it, and a fault covers a run of steps rather than one operation,
// which is what lets three of them overlap.
const (
	// stepsPerFault is how much room the schedule gives each fault. Every fault
	// is placed, so the schedule is long enough that placing them all leaves
	// steps between them that are nobody's.
	stepsPerFault = 2
	// baseSteps is the schedule before the faults are placed in it: the
	// operations a campaign runs whether anything goes wrong or not.
	baseSteps = 6
	// maxConcurrentFaults is how many faults may be on at once. One is what the
	// campaigns before this one could do; three is enough for the combination
	// this package exists for — a source partitioned while the store is
	// unavailable while a second host takes the VM over — without a schedule in
	// which nothing ever works.
	maxConcurrentFaults = 3
	// maxFaultSteps is the longest a fault stays on. A fault that outlived the
	// schedule would never have to leave anything true.
	maxFaultSteps = 3
	// faultChance is how often a step is one where faults begin.
	faultChance = 0.5
	// maxStoresPerStep is how many pages a guest writes before each operation.
	// One store is enough to make the step's checkpoint or migration carry
	// something; more is what makes it carry runs.
	maxStoresPerStep = 4
)

// Driver runs one seed: the topology's operations in a seeded order, with one
// to three faults on at seeded offsets over them.
//
// What it asserts as it goes is the campaign's first invariant — no guest reads
// bytes it never wrote — plus, at every takeover, that the checkpoint the new
// writer inherits is one a writer of that VM published. The other two are
// asserted at the end, where they mean something: what each fault had to leave
// true, and that what the store holds is still a deployment.
type Driver struct {
	world  *World
	random sim.Random
	faults []Fault
	steps  int
	// draws numbers the choices one step makes, so a step that makes three of
	// them draws three different numbers without a shared stream.
	draws int
	log   func(format string, args ...any)
}

// NewDriver plans one seed's run: the faults it will inject and the schedule
// they are injected over.
func NewDriver(w *World, r sim.Random, faults []Fault, log func(string, ...any)) *Driver {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Driver{world: w, random: r, faults: faults,
		steps: baseSteps + stepsPerFault*len(faults), log: log}
}

// permutation shuffles the numbers below n from the seed, which is the order
// the faults of one seed are placed in. It is Fisher-Yates against the keyed
// random, so where a seed puts one fault does not depend on how many other
// faults happened to be drawn before it.
func (d *Driver) permutation(id string, n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j := d.random.Intn(fmt.Sprintf("%s/%d", id, i), i+1)
		order[i], order[j] = order[j], order[i]
	}
	return order
}

// window is one fault's place in the schedule: the step it begins at and the
// step it ends at.
type window struct {
	fault Fault
	begin int
	end   int
}

// plan places every fault in the schedule. Faults begin in a seeded order at
// seeded offsets, one to three at a time, and each stays on for a seeded run of
// steps; a fault that would be the fourth one on waits for the next offset.
//
// Every fault of the set is placed, which is what makes a sweep a sweep: a seed
// that skipped a fault would be a seed that proved nothing about it, and the
// seeds would then have to be counted rather than run.
func (d *Driver) plan() []window {
	order := d.permutation("faults/order", len(d.faults))
	var windows []window
	next := 0
	active := func(at int) int {
		count := 0
		for _, w := range windows {
			if w.begin <= at && at < w.end {
				count++
			}
		}
		return count
	}
	for offset := 1; offset < d.steps && next < len(order); offset++ {
		room := maxConcurrentFaults - active(offset)
		if room <= 0 {
			continue
		}
		// The last offsets place whatever is left whether or not they were
		// drawn as fault offsets: every fault runs on every seed.
		forced := d.steps-offset <= stepsPerFault*(len(order)-next)
		if !forced && !d.random.Chance(fmt.Sprintf("faults/%d/begins", offset), faultChance) {
			continue
		}
		count := 1 + d.random.Intn(fmt.Sprintf("faults/%d/count", offset), room)
		for range count {
			if next == len(order) {
				break
			}
			length := 1 + d.random.Intn(fmt.Sprintf("faults/%d/%d/length", offset, next), maxFaultSteps)
			windows = append(windows, window{fault: d.faults[order[next]],
				begin: offset, end: min(offset+length, d.steps)})
			next++
		}
	}
	return windows
}

// Run drives the whole schedule and then requires of every fault what it had to
// leave true.
func (d *Driver) Run(ctx context.Context) error {
	windows := d.plan()
	d.log("schedule: %d steps, %s", d.steps, describe(windows))
	live := map[Fault]bool{}
	for step := range d.steps {
		for _, w := range windows {
			if w.begin == step {
				d.begin(ctx, w.fault)
				live[w.fault] = true
			}
		}
		for _, w := range windows {
			if w.end == step && live[w.fault] {
				d.end(ctx, w.fault)
				delete(live, w.fault)
			}
		}
		if err := d.world.Settle(ctx); err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}
		if err := d.step(ctx, step); err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}
		// Nothing a fault did may leave a guest reading bytes its own guest did
		// not write. This is the assertion that has to be made while the world
		// is still running, because a page read is the only place it shows.
		if err := d.world.Verify(ctx, ReadsMayFail); err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}
	}
	for _, w := range windows {
		if live[w.fault] {
			d.end(ctx, w.fault)
		}
	}
	// The world has to be given the chance to finish what the last faults
	// interrupted before anything is required of it: a host that has just come
	// back has VMs to take over.
	if err := d.world.Settle(ctx); err != nil {
		return err
	}
	var errs []error
	for _, w := range windows {
		if err := w.fault.Holds(ctx, d.world); err != nil {
			errs = append(errs, fmt.Errorf("%s did not leave the world as it found it: %w", w.fault.Name(), err))
		}
	}
	// Every fault has ended, so every page has to read now: a VM whose memory
	// is still unreachable in a world with nothing wrong with it was lost
	// rather than rewound.
	errs = append(errs, d.world.Verify(ctx, ReadsMustSucceed), d.world.CheckSelected(ctx))
	return errors.Join(errs...)
}

func (d *Driver) begin(ctx context.Context, fault Fault) {
	err := fault.Begin(ctx, d.world)
	d.trace(fault, "begin", err)
}

func (d *Driver) end(ctx context.Context, fault Fault) {
	err := fault.End(ctx, d.world)
	d.trace(fault, "end", err)
}

// trace records every fault start and end beside the adapter operations they
// perturb, so the trace of a failing seed says what was on when it failed.
func (d *Driver) trace(fault Fault, operation string, err error) {
	outcome := "ok"
	if err != nil {
		outcome = err.Error()
	}
	d.world.Runtime().Trace().Record(sim.Event{Kind: "fault",
		Resource: fault.Name(), Operation: operation, Outcome: outcome})
	d.log("fault %s %s: %s", fault.Name(), operation, outcome)
}

// step runs one operation of the schedule, with a few of the running VMs' own
// stores in front of it.
func (d *Driver) step(ctx context.Context, step int) error {
	running := d.world.Running()
	if len(running) == 0 {
		return nil
	}
	choose := func(limit int) int {
		d.draws++
		return d.random.Intn(fmt.Sprintf("step/%d/choice/%d", step, d.draws), max(limit, 1))
	}
	for _, id := range running {
		if err := d.world.Store(ctx, id, 1+choose(maxStoresPerStep), choose); err != nil {
			return err
		}
	}
	id := running[choose(len(running))]
	switch operation := d.operation(choose); operation {
	case "checkpoint":
		d.log("step %d: checkpoint %s", step, id)
		if err := d.world.Checkpoint(ctx, id); err != nil {
			// A checkpoint the store would not take is a checkpoint that did
			// not happen: the VM goes on running out of its own pages, and
			// what it is worth is still its last one.
			d.log("step %d: %s could not publish: %v", step, id, err)
		}
	case "checkpoint-disks":
		d.log("step %d: checkpoint the disks of %s", step, id)
		if err := d.world.CheckpointDisks(ctx, id); err != nil {
			d.log("step %d: %s could not publish its disks: %v", step, id, err)
		}
	case "migrate":
		to := choose(d.world.Hosts())
		d.log("step %d: migrate %s to host-%d", step, id, to)
		return d.world.Migrate(ctx, id, to)
	case "fork":
		spec, ok := d.pending(choose)
		if !ok {
			return nil
		}
		d.log("step %d: fork %s from %s onto host-%d", step, spec.ID, spec.Parent, spec.Host)
		return d.world.Fork(ctx, spec)
	case "delete":
		// Two VMs have to be left running: a campaign that deleted its way
		// down to one would assert nothing about two writers for the rest of
		// its steps, and nothing at all once the last one went.
		if len(running) < 3 {
			return nil
		}
		d.log("step %d: delete %s", step, id)
		return d.world.Delete(ctx, id)
	case "stop":
		// The VM has to be one a host is actually running, and one has to be
		// left: a schedule that stopped its way down to nothing would assert
		// nothing about two writers for the rest of its steps.
		started := d.world.Started()
		if len(started) < 2 {
			return nil
		}
		stopping := started[choose(len(started))]
		if choose(2) == 0 {
			d.log("step %d: stop %s", step, stopping)
			return d.world.Stop(ctx, stopping)
		}
		d.log("step %d: suspend %s", step, stopping)
		return d.world.Suspend(ctx, stopping)
	case "start":
		// Only a stopped VM can be started, and nothing else will bring one
		// back, so a schedule with none is a step that does nothing.
		stopped := d.world.Stopped()
		if len(stopped) == 0 {
			return nil
		}
		starting := stopped[choose(len(stopped))]
		host := choose(d.world.Hosts())
		// Half of the starts are cold: the VM comes back without its memory,
		// having discarded it and the VMM state in a checkpoint of its own, and
		// what it must then hold is zeroes where its memory was and the last
		// checkpoint's bytes everywhere else.
		if choose(2) == 0 {
			d.log("step %d: cold start %s on host-%d", step, starting, host)
			return d.world.StartCold(ctx, starting, host)
		}
		d.log("step %d: start %s on host-%d", step, starting, host)
		return d.world.Start(ctx, starting, host)
	case "restart":
		host := choose(d.world.Hosts())
		d.log("step %d: restart host-%d", step, host)
		if err := d.world.LoseHost(ctx, host); err != nil {
			return err
		}
		return d.world.RestartHost(ctx, host)
	default:
		return fmt.Errorf("unknown operation %q", operation)
	}
	return nil
}

// operation draws what this step does. Checkpoints and migrations are the
// common ones because they are what a deployment spends its life doing, and
// most checkpoints are of the disks alone because that is what the interval
// takes: a VM whose last checkpoint was one of them comes back cold. A fork,
// a stop, a start, a delete and a host restart are rarer and each changes what
// the deployment is rather than what it holds.
//
// Stop and start are drawn independently rather than as a pair, so a seed's
// stopped VMs sit out however many steps it draws before starting them — which
// is where the faults land on a VM that is nothing but its objects, and where a
// host that comes back without it has to leave it alone.
func (d *Driver) operation(choose func(int) int) string {
	weighted := []string{"checkpoint-disks", "checkpoint-disks", "checkpoint", "migrate", "migrate",
		"fork", "stop", "start", "delete", "restart"}
	return weighted[choose(len(weighted))]
}

// pending is a fork of the topology that has not happened yet and whose parent
// exists. Nothing else can be forked: a child whose parent was deleted is a
// child of nothing.
func (d *Driver) pending(choose func(int) int) (VMSpec, bool) {
	var ready []VMSpec
	for _, spec := range d.world.Topology().VMs {
		if !spec.IsFork() {
			continue
		}
		if d.world.Exists(spec.ID) || d.world.HostOf(spec.Parent) < 0 {
			continue
		}
		ready = append(ready, spec)
	}
	if len(ready) == 0 {
		return VMSpec{}, false
	}
	return ready[choose(len(ready))], true
}

// describe is the one line a run prints to say which fault covers which steps.
func describe(windows []window) string {
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		parts = append(parts, fmt.Sprintf("%s[%d,%d)", w.fault.Name(), w.begin, w.end))
	}
	slices.Sort(parts)
	return strings.Join(parts, " ")
}
