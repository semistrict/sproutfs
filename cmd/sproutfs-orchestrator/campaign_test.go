package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
	"github.com/semistrict/sproutfs/internal/handover"
	"github.com/semistrict/sproutfs/platform/sim"
)

// The orchestrator calls three things across a boundary: each host's API, the
// Kubernetes API and the bucket. The fakes of all three fail at random in a
// campaign (orchestrator_test.go), and these are their sites: every method of
// each fails before the far side acted, and every request that changes
// something may lose its answer after it did.
var (
	// hostReads are the host API's requests that change nothing, where an
	// answer lost on the way back is a request that never arrived.
	hostReads = []string{"Status", "Console", "Handed", "Kept"}
	// hostWrites are the rest.
	hostWrites = []string{"Create", "ImportTemplate", "Open", "Fork", "Capture", "WriteConsole", "Exec",
		"Migrate", "Receive", "Released", "Abandoned", "Stop", "Delete", "Release"}
	recordReads = []string{"List", "Pending", "Epoch", "Journals"}
)

// boundarySites is every fault site of the three fakes.
func boundarySites() []string {
	sites := []string{"orchestrator/pods-unavailable/List", "orchestrator/pods-unavailable/Delete",
		"orchestrator/pods-reply-lost/Delete"}
	for _, method := range slices.Concat(hostReads, hostWrites) {
		sites = append(sites, "orchestrator/host-unreachable/"+method, "orchestrator/host-refuses/"+method)
	}
	for _, method := range hostWrites {
		sites = append(sites, "orchestrator/host-reply-lost/"+method)
	}
	for _, method := range recordReads {
		sites = append(sites, "orchestrator/records-unavailable/"+method)
	}
	return sites
}

const (
	// campaignSeeds runs enough seeds that each site is activated in several,
	// and campaignSteps enough operations that an activated site fires.
	// SPROUTFS_ORCHESTRATOR_SEEDS selects another count.
	campaignSeeds = 128
	campaignSteps = 300
	// replaySeeds is how many of the campaign's seeds the replay test runs
	// twice, unless SPROUTFS_ORCHESTRATOR_SEEDS selects another count.
	replaySeeds = 16
	// campaignHold is what a host holds a handover for, as a deployment's
	// four checkpoint intervals are.
	campaignHold = 4 * time.Minute
	// campaignHosts is how many hosts the deployment keeps listed: a host
	// killed or drained is replaced.
	campaignHosts = 3
)

// TestTheOrchestratorUnderBoundaryFaults drives the orchestrator through
// creates, forks, migrations, drains, stops, starts, recoveries, host kills,
// orchestrator restarts and deletes while each host, the Kubernetes API and the
// bucket fail at random: requests that never arrive, refusals, and answers lost
// after the far side acted, and hosts cut off the pod network for minutes. Then
// the faults stop and the deployment settles. Every VM with a control record
// must then run on exactly one host or be stopped, and a stopped one must
// start; every VM the orchestrator said it made and nothing deleted must still
// have its record; and no host may run a VM that has none, or still hold a
// handover. Across the seeds, every fault site fires.
//
// A seed chooses the steps, which sites it activates, and the order in which
// everything that goes on at once goes on: every request the orchestrator
// makes of the fakes, and every timer that ends, is released by the seed's
// scheduler. A failing seed fails the same way every time it runs
// (TestTheOrchestratorCampaignReplaysItsSeeds), and prints what the deployment
// did and what the orchestrator logged.
func TestTheOrchestratorUnderBoundaryFaults(t *testing.T) {
	seeds := seedCount(t, campaignSeeds)
	fired := map[string]bool{}
	// What the orchestrator logs is a seed's own, and shown only when the
	// seed fails: a passing campaign would otherwise print thousands of lines.
	defer slog.SetDefault(slog.Default())
	for seed := uint64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			var logged bytes.Buffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			synctest.Test(t, func(t *testing.T) {
				run := orchestratorCampaign(t, seed)
				for site := range run.fired {
					fired[site] = true
				}
				if t.Failed() {
					t.Logf("the orchestrator logged:\n%s", logged.String())
				}
			})
		})
	}
	if t.Failed() {
		return
	}
	for _, site := range boundarySites() {
		if !fired[site] {
			t.Errorf("no seed fired the fault site %s", site)
		}
	}
}

// seedCount is how many seeds a campaign test runs: standard unless
// SPROUTFS_ORCHESTRATOR_SEEDS selects another count.
func seedCount(t *testing.T, standard uint64) uint64 {
	value := os.Getenv("SPROUTFS_ORCHESTRATOR_SEEDS")
	if value == "" {
		return standard
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		t.Fatal("SPROUTFS_ORCHESTRATOR_SEEDS must be positive")
	}
	return parsed
}

// TestTheOrchestratorCampaignReplaysItsSeeds runs seeds of the campaign twice
// each and requires the two runs to release every request and every timer in
// the same order, fire the same faults and leave the same log: what a seed
// does is the seed's, not the Go scheduler's. Before 2026-10-08 a seed did
// not replay: the orchestrator surveyed its hosts at once, reconciled on its
// own timer, watched a migration's source and wrote its flights' rows again
// beside the requests it served, and each of them drew its faults and changed
// the deployment in whatever order the Go scheduler ran it.
func TestTheOrchestratorCampaignReplaysItsSeeds(t *testing.T) {
	seeds := seedCount(t, replaySeeds)
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(slog.DiscardHandler))
	for seed := uint64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			var runs [2]campaignRun
			for run := range runs {
				synctest.Test(t, func(t *testing.T) { runs[run] = orchestratorCampaign(t, seed) })
			}
			requireReplay(t, seed, runs)
		})
	}
}

// requireReplay fails unless two runs of a seed released everything in the
// same order, fired the same faults and logged the same deployment. A failure
// prints the first line the runs differ at and the ones before it, which is
// where a search for what the Go scheduler chose begins.
func requireReplay(t *testing.T, seed uint64, runs [2]campaignRun) {
	t.Helper()
	var texts [2][]string
	for run, r := range runs {
		var text strings.Builder
		if err := r.recording.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		text.WriteString("Deployment:\n")
		text.WriteString(strings.Join(r.log, "\n"))
		texts[run] = strings.Split(text.String(), "\n")
	}
	a, b := texts[0], texts[1]
	if slices.Equal(a, b) {
		return
	}
	at := 0
	for at < min(len(a), len(b)) && a[at] == b[at] {
		at++
	}
	from := max(0, at-12)
	t.Fatalf("seed %d went another way on its second run, from line %d:\nfirst run:\n%s\nsecond run:\n%s",
		seed, at, strings.Join(a[from:min(len(a), at+6)], "\n"), strings.Join(b[from:min(len(b), at+6)], "\n"))
}

// campaignRun is what one run of a campaign's seed reports: the sites it
// fired, the order its scheduler released everything in with the faults the
// runtime drew, and what the deployment did.
type campaignRun struct {
	fired     map[string]uint64
	recording sim.Recording
	log       []string
}

// world is one seed's deployment and what the campaign knows of it.
type world struct {
	*deployment
	t *testing.T
	// base carries the runtime, and ctx is the task the campaign is on: a
	// step, or the settling at the end.
	base, ctx context.Context
	runtime   *sim.Runtime
	random    sim.Random
	// made is every VM the orchestrator said it made and nothing has been
	// asked to delete since: each must keep its control record.
	made map[string]bool
	// hostsMade numbers the host pods, which are never named twice, and
	// losses the answers the hosts lost.
	hostsMade int
	losses    atomic.Uint64
	// stopReconciling ends the orchestrator's own reconcile timer, which
	// every orchestrator process runs beside the requests it serves, and
	// processes counts those processes.
	stopReconciling func()
	processes       int
}

// orchestratorCampaign runs one seed of the campaign, everything it does
// released by a scheduler of that seed.
func orchestratorCampaign(t *testing.T, seed uint64) campaignRun {
	scheduler := sim.NewScheduler(seed)
	done := make(chan struct{})
	// Once the campaign is over nothing waits for a turn: a hold that ends
	// after it, on a host's own timer, ends at once.
	wait := func(ctx context.Context, id string, minimum, maximum time.Duration) error {
		select {
		case <-done:
			return nil
		default:
		}
		return scheduler.Wait(ctx, id, minimum, maximum)
	}
	runtime := sim.New(sim.Config{Seed: seed, Wait: wait})
	base := sim.WithRuntime(t.Context(), runtime)
	w := &world{deployment: newDeployment(t, map[string][]string{}), t: t, base: base, ctx: base,
		runtime: runtime, random: runtime.Random("orchestrator-campaign"), made: map[string]bool{}}
	// The deployment's own timings: the simulated clock makes them free.
	w.orchestrator.handover = handover.Default
	w.orchestrator.sourceWatch = 0
	// A journal is read only through a membership.
	w.orchestrator.members = newMembershipStore(t, runtime, "orchestrator")
	go func() {
		defer close(done)
		w.run()
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	recording, err := scheduler.Recording(runtime.Trace())
	if err != nil {
		t.Fatal(err)
	}
	return campaignRun{fired: runtime.FiredSites(), recording: recording, log: w.log}
}

// run is the campaign: a deployment of campaignHosts hosts takes
// campaignSteps steps with the faults on, settles with them off, and is
// checked.
func (w *world) run() {
	for range campaignHosts {
		w.replenish()
	}
	w.reconciling()
	w.runtime.SetBuggify(true)
	for step := range campaignSteps {
		w.step(step)
	}
	w.runtime.SetBuggify(false)
	w.ctx = sim.WithTask(w.base, "settle")
	w.settle()
	w.stopReconciling()
	w.check()
}

// sleep lets d pass, and goes on when the run chooses: whatever else ends at
// the same instant, a hold or a reconcile's timer, goes on in the seed's
// order.
func (w *world) sleep(d time.Duration) {
	time.Sleep(d)
	if err := sim.Admit(w.ctx, "orchestrator-campaign/wake"); err != nil {
		w.t.Errorf("waking: %v", err)
	}
}

// reconciling runs a new orchestrator process's reconcile timer, a task of
// its own, until stopReconciling is called, which returns once nothing it
// began is still running.
func (w *world) reconciling() {
	w.processes++
	ctx, cancel := context.WithCancel(sim.WithTask(w.base, "orchestrator "+strconv.Itoa(w.processes)))
	o := w.orchestrator
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.Reconciling(ctx, ReconcileInterval)
	}()
	w.stopReconciling = func() {
		cancel()
		<-done
		o.resumes.Wait()
	}
}

// steps is what a deployment does, each with how often it does it.
var steps = []struct {
	weight int
	do     func(w *world, key string)
}{
	{4, func(w *world, _ string) { w.create() }},
	{2, (*world).fork},
	{3, (*world).migrate},
	// A handover taken up from its source is left only by a restart or a lost
	// answer, and restarts are this common so that many seeds meet one.
	{4, (*world).restart},
	{1, (*world).drain},
	{1, (*world).stop},
	{1, (*world).start},
	{1, (*world).recover},
	{1, (*world).kill},
	{1, (*world).delete},
	{1, (*world).capture},
	{3, (*world).touch},
	{1, (*world).partition},
	{1, func(w *world, _ string) { w.reconcile() }},
	// Time passes, a minute at most at a time: rows age out of flight and
	// holds end, with the deployment surveyed in between.
	{3, func(w *world, key string) { w.sleep(w.random.Duration("wait/"+key, time.Minute)) }},
}

// step does one thing a deployment does, chosen by the seed, as a task of its
// own.
func (w *world) step(step int) {
	key := strconv.Itoa(step)
	w.ctx = sim.WithTask(w.base, "step "+key)
	total := 0
	for _, s := range steps {
		total += s.weight
	}
	choice := w.random.Intn("step/"+key, total)
	for _, s := range steps {
		if choice < s.weight {
			s.do(w, key)
			break
		}
		choice -= s.weight
	}
	w.orchestrator.resumes.Wait()
}

// replenish lists a new host pod.
func (w *world) replenish() {
	w.hostsMade++
	h := w.addHost("host-"+strconv.Itoa(w.hostsMade), nil)
	w.mu.Lock()
	h.hold = host.Seconds(campaignHold.Seconds())
	// Half the answers a host loses, it loses because it fell off the pod
	// network, for up to two holds: the moment its last request was carried
	// out is the moment it stops being reachable.
	h.onReplyLost = func(h *fakeHostClient) {
		key := strconv.FormatUint(w.losses.Add(1), 10)
		if w.random.Chance("cut-off/"+key, 0.5) {
			h.partitioned = time.Now().Add(w.random.Duration("cut-off-for/"+key, 2*campaignHold))
		}
	}
	w.mu.Unlock()
}

// listed is the host pods the Kubernetes API lists, in name order.
func (w *world) listed() []pod {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.SortedFunc(slices.Values(w.pods.pods), func(a, b pod) int { return strings.Compare(a.Name, b.Name) })
}

// vms is every VM with a control record, in identity order.
func (w *world) vms() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Sorted(slices.Values(w.records.ids))
}

// running is every VM a live host runs, in identity order: what a fork, a
// migration, a stop, a capture or a command is asked of.
func (w *world) running() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var found []string
	for _, h := range w.hosts {
		if !h.killed {
			found = append(found, h.running...)
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// pick chooses one of values by the seed, and reports false when there are
// none.
func pick[T any](w *world, what string, values []T) (T, bool) {
	var none T
	if len(values) == 0 {
		return none, false
	}
	return values[w.random.Intn("pick/"+what, len(values))], true
}

// target chooses a host to name as a destination, or none to let the
// orchestrator place it.
func (w *world) target(key string) string {
	if w.random.Chance("placed/"+key, 0.5) {
		return ""
	}
	chosen, _ := pick(w, "host/"+key, w.listed())
	return chosen.Name
}

// did writes one request the campaign made, and what came of it, into the
// deployment's log between what the hosts were asked.
func (w *world) did(err error, format string, args ...any) {
	outcome := "ok"
	if err != nil {
		outcome = err.Error()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, "campaign "+fmt.Sprintf(format, args...)+": "+outcome)
}

func (w *world) create() {
	result, err := w.orchestrator.Create(w.ctx, orch.CreateRequest{Template: "alpine"})
	w.did(err, "create %s", result.Result.VM.ID)
	if err == nil {
		w.made[result.Result.VM.ID] = true
	}
}

func (w *world) fork(key string) {
	id, ok := pick(w, "fork/"+key, w.running())
	if !ok {
		return
	}
	request := orch.ForkRequest{Count: 1 + w.random.Intn("count/"+key, 3), To: w.target(key)}
	result, err := w.orchestrator.Fork(w.ctx, id, request)
	w.did(err, "fork %s %d to %q: %v", id, request.Count, request.To, result.Children)
	if err == nil {
		for _, child := range result.Children {
			w.made[child] = true
		}
	}
}

func (w *world) migrate(key string) {
	if id, ok := pick(w, "migrate/"+key, w.running()); ok {
		to := w.target(key)
		_, err := w.orchestrator.Migrate(w.ctx, id, to)
		w.did(err, "migrate %s to %q", id, to)
	}
}

// drain is a host pod being deleted with a grace period, as the autoscaler
// removes a node: the pod is listed terminating, the host hands each of its VMs
// over through the orchestrator and reports each one, and the pod goes once it
// is done. Its replacement is listed.
func (w *world) drain(key string) {
	chosen, ok := pick(w, "drain/"+key, w.listed())
	if !ok {
		return
	}
	w.mu.Lock()
	for index := range w.pods.pods {
		if w.pods.pods[index].Name == chosen.Name {
			w.pods.pods[index].Terminating, w.pods.pods[index].Ready = true, false
		}
	}
	h := w.hosts[chosen.Name]
	running := slices.Clone(h.running)
	w.mu.Unlock()
	w.did(nil, "drain %s", chosen.Name)
	w.replenish()
	for _, id := range running {
		report := orch.DrainReport{Host: chosen.Name, VM: id, Phase: orch.DrainStarted}
		if err := w.orchestrator.Drained(w.ctx, report); err != nil {
			w.t.Errorf("a drain report was refused: %v", err)
			return
		}
		report.Phase = orch.DrainFinished
		_, err := w.orchestrator.Migrate(w.ctx, id, "")
		w.did(err, "drain %s off %s", id, chosen.Name)
		if err != nil {
			report.Error = err.Error()
		}
		if err := w.orchestrator.Drained(w.ctx, report); err != nil {
			w.t.Errorf("a drain report was refused: %v", err)
			return
		}
	}
	// The pod ends when its drain does, whatever the drain managed: what it
	// still ran is lost with it, at its last checkpoint.
	w.end(chosen.Name)
}

// end deletes a host pod and ends its process.
func (w *world) end(name string) {
	w.mu.Lock()
	w.pods.pods = slices.DeleteFunc(w.pods.pods, func(p pod) bool { return p.Name == name })
	h := w.hosts[name]
	w.mu.Unlock()
	h.die()
	w.did(nil, "end %s", name)
}

func (w *world) stop(key string) {
	if id, ok := pick(w, "stop/"+key, w.running()); ok {
		_, err := w.orchestrator.Stop(w.ctx, id, orch.StopRequest{Keep: w.random.Chance("keep/"+key, 0.3)})
		w.did(err, "stop %s", id)
	}
}

func (w *world) start(key string) {
	if id, ok := pick(w, "start/"+key, w.vms()); ok {
		to := w.target(key)
		_, err := w.orchestrator.Start(w.ctx, id, orch.StartRequest{To: to})
		w.did(err, "start %s on %q", id, to)
	}
}

func (w *world) recover(key string) {
	if id, ok := pick(w, "recover/"+key, w.vms()); ok {
		_, err := w.orchestrator.Recover(w.ctx, id, false)
		w.did(err, "recover %s", id)
	}
}

// kill deletes a host pod at once, through the orchestrator, as the demo does.
// A pod the Kubernetes API deleted is gone with everything its host ran,
// whatever the orchestrator was told, and its replacement is listed.
func (w *world) kill(key string) {
	chosen, ok := pick(w, "kill/"+key, w.listed())
	if !ok {
		return
	}
	_, err := w.orchestrator.Kill(w.ctx, chosen.Name)
	w.did(err, "kill %s", chosen.Name)
	if slices.ContainsFunc(w.listed(), func(p pod) bool { return p.Name == chosen.Name }) {
		return
	}
	w.end(chosen.Name)
	w.replenish()
}

func (w *world) delete(key string) {
	id, ok := pick(w, "delete/"+key, w.vms())
	if !ok {
		return
	}
	// A delete whose answer was lost may have happened, so the VM is owed
	// nothing from here either way.
	delete(w.made, id)
	w.did(w.orchestrator.Delete(w.ctx, id), "delete %s", id)
}

func (w *world) capture(key string) {
	id, ok := pick(w, "capture/"+key, w.running())
	if !ok {
		return
	}
	request := orch.CaptureRequest{New: w.random.Chance("new/"+key, 0.5)}
	request.Keep = !request.New && w.random.Chance("keep/"+key, 0.5)
	result, err := w.orchestrator.Capture(w.ctx, id, request)
	w.did(err, "capture %s %+v", id, request)
	if err == nil && request.New {
		w.made[result.Result.VM] = true
	}
}

// touch is everything that reads a VM or acts on it in place, each once: its
// console read and written, a command run in it, its kept checkpoints listed
// and one released, a template imported, and the listing.
func (w *world) touch(key string) {
	id, ok := pick(w, "touch/"+key, w.running())
	if !ok {
		return
	}
	_, err := w.orchestrator.Console(w.ctx, id, 0)
	w.did(err, "read the console of %s", id)
	w.did(w.orchestrator.WriteConsole(w.ctx, id, "ls\n"), "write the console of %s", id)
	_, err = w.orchestrator.Exec(w.ctx, id, host.ExecRequest{Cmd: "true"})
	w.did(err, "exec in %s", id)
	_, err = w.orchestrator.Kept(w.ctx, id)
	w.did(err, "list what %s kept", id)
	w.did(w.orchestrator.Release(w.ctx, id, 7), "release %s@7", id)
	_, err = w.orchestrator.ImportTemplate(w.ctx, strings.NewReader("image"), host.ImportTemplateRequest{})
	w.did(err, "import a template")
	_, err = w.orchestrator.VMs(w.ctx)
	w.did(err, "list the VMs")
}

// partition cuts one host off the pod network for a while, up to two holds: it
// runs its guests and keeps its pages, and nothing reaches it.
func (w *world) partition(key string) {
	chosen, ok := pick(w, "partition/"+key, w.listed())
	if !ok {
		return
	}
	lasting := w.random.Duration("partition/"+key, 2*campaignHold)
	w.mu.Lock()
	w.hosts[chosen.Name].partitioned = time.Now().Add(lasting)
	w.mu.Unlock()
	w.did(nil, "partition %s for %s", chosen.Name, lasting)
}

// restart is the orchestrator's process ending in the middle of a migration,
// after the source stopped the guest and before any destination was asked to
// take it, and a new one starting over the same table: the handoff is the
// source's alone, and the new process takes the handover up from it.
func (w *world) restart(key string) {
	id, ok := pick(w, "restart/"+key, w.running())
	if !ok {
		return
	}
	from := w.runners(id)[0]
	others := slices.DeleteFunc(w.listed(), func(p pod) bool { return p.Name == from })
	chosen, ok := pick(w, "restart-to/"+key, others)
	if !ok {
		return
	}
	to := chosen.Name
	w.mu.Lock()
	source, destination := w.hosts[from], w.hosts[to]
	w.mu.Unlock()
	w.orchestrator.note(w.ctx, vmRecord{ID: id, Host: from, State: stateMigrating, From: from, To: to})
	_, err := source.Migrate(w.ctx, id, host.MigrateRequest{Destination: destination.page})
	w.did(err, "migrate %s to %s, and restart the orchestrator", id, to)
	w.stopReconciling()
	old := w.orchestrator
	w.orchestrator = &orchestrator{pods: old.pods, records: old.records, dial: old.dial,
		identify: old.identify, apiPort: old.apiPort, pagePort: old.pagePort, table: old.table,
		code: old.code, sourceWatch: old.sourceWatch, handover: old.handover, members: old.members}
	w.reconciling()
}

func (w *world) reconcile() {
	w.did(w.orchestrator.Reconcile(w.ctx), "reconcile")
}

// settle lets the deployment finish what the faults left: holds end, rows age
// out of flight, and a reconcile takes up or gives up every handover. Then each
// VM no host runs is started, which must work: it is stopped, not lost.
func (w *world) settle() {
	// The faults stopping includes every partition healing.
	w.mu.Lock()
	var healed time.Time
	for _, h := range w.hosts {
		if h.partitioned.After(healed) {
			healed = h.partitioned
		}
	}
	w.mu.Unlock()
	w.sleep(time.Until(healed))
	for round := range 4 {
		w.sleep(campaignHold + inFlightFor)
		if err := w.orchestrator.Reconcile(w.ctx); err != nil {
			w.t.Errorf("reconciling with the faults stopped: %v", err)
			return
		}
		w.orchestrator.resumes.Wait()
		if round > 0 && !w.handingOver() {
			break
		}
	}
	for _, id := range w.vms() {
		if len(w.runners(id)) > 0 {
			continue
		}
		if _, err := w.orchestrator.Start(w.ctx, id, orch.StartRequest{}); err != nil {
			w.t.Errorf("starting %s, which no host runs, with the faults stopped: %v", id, err)
		}
	}
}

// handingOver reports whether any host still serves or receives a VM.
func (w *world) handingOver() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, h := range w.hosts {
		if !h.killed && (len(h.serving) > 0 || len(h.receiving) > 0) {
			return true
		}
	}
	return false
}

// runners is the live hosts running a VM, in name order.
func (w *world) runners(id string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var found []string
	for _, name := range slices.Sorted(maps.Keys(w.hosts)) {
		if h := w.hosts[name]; !h.killed && slices.Contains(h.running, id) {
			found = append(found, name)
		}
	}
	return found
}

// check is what must hold once the deployment has settled.
func (w *world) check() {
	records := w.vms()
	for _, id := range records {
		if runners := w.runners(id); len(runners) != 1 {
			w.t.Errorf("%s runs on %v after the deployment settled, want exactly one host", id, runners)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(w.made)) {
		if !slices.Contains(records, id) {
			w.t.Errorf("%s, which the orchestrator made and nothing deleted, has no control record", id)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(w.hosts)) {
		h := w.hosts[name]
		if h.killed {
			continue
		}
		for _, id := range h.running {
			if !slices.Contains(records, id) {
				w.t.Errorf("%s runs %s, which has no control record", name, id)
			}
		}
		if len(h.serving) > 0 || len(h.receiving) > 0 {
			w.t.Errorf("%s still serves %v and receives %v after the deployment settled", name,
				h.serving, h.receiving)
		}
	}
	if w.t.Failed() {
		w.t.Logf("the deployment did:\n%s", strings.Join(w.log, "\n"))
	}
}
