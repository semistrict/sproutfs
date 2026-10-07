package simtest_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// journalProbes is every probe the journal campaign must reach across its
// seeds: durable flush's on the hosts, the journal disks' on the hosts, and
// the journal's own, less unreachedJournalProbes. A commit refused because a
// read fenced its VM is not among them: whether a commit is still waiting for
// its batch when a takeover's read arrives, or fails its capture as the holder
// gives the VM up, is a race of the run, not a choice of the seed.
// TestAReadFencesTheVMAndWaitsForTheBatchInFlight in journal reaches it.
var journalProbes = append([]string{host.ProbeJournalHalfRing, host.ProbeJournalThreeQuarters,
	host.ProbeJournalNoRoom, host.ProbeJournalReadGaveUpVM, host.ProbeJournalCaptureAfterPause,
	host.ProbeJournalDiskOpened, host.ProbeJournalDiskClosedEmpty, host.ProbeJournalDiskLost},
	slices.DeleteFunc(journal.Probes(), func(probe string) bool { return probe == journal.ProbeFenced })...)

// unreachedJournalProbes is the probes of journalProbes no seed reaches here,
// and why; the list is asserted in both directions. A lease is refused, or
// found taken, only by a process that holds a journal disk's device under an
// older assignment. The cloud here attaches a disk to one machine at a time
// and takes it from every process of a machine it detaches it from, as
// Compute Engine does, and a host opens a disk only once the membership, read
// again, still assigns it there. TestANewerLeaseRefusesTheDisk in journal
// keeps two handles of one device and reaches both.
var unreachedJournalProbes = []string{journal.ProbeLeaseRefused, journal.ProbeLeaseLost}

// journalSites is every site the journal campaign must fire across its
// seeds: the journal's writes, a capture held before a pause, and the
// cloud's as the controller moves, makes and deletes journal disks. A world
// with no shards describes no disk. A seed makes and deletes a handful of
// disks, too few to fail one; TestTheCloudsCreateAndDeleteSitesFire in
// platform/sim fails both.
var journalSites = append(journal.Sites(), host.BuggifyJournalCaptureSlow, sim.BuggifyAttachSlow,
	sim.BuggifyAttachFails, sim.BuggifyAttachReplyLost, sim.BuggifyDetachSlow, sim.BuggifyDetachFails,
	sim.BuggifyCreateSlow)

// campaignJournalBytes is each journal disk of the campaign: ten of its VMs'
// whole pages, so half a ring is five of them, and rings fill.
const campaignJournalBytes = 20 << 20

// campaignJournalExpiry is how long the campaign's controller keeps a journal
// disk free and empty before it deletes it: two steps, so disks are deleted
// and made again as hosts leave and join.
const campaignJournalExpiry = 2 * time.Minute

// journalCampaignSeeds is how many seeds the campaign runs.
const journalCampaignSeeds = 16

// journalCampaign is one seed of the campaign: three hosts with durable
// flush on, two VMs, and a seeded schedule of what happens to flushes and
// the hosts that answer them, with the sites on.
type journalCampaign struct {
	t       *testing.T
	ctx     context.Context
	runtime *sim.Runtime
	world   *simtest.World
	choose  sim.Random
	step    int
	// down is the hosts the schedule has stopped, which a join starts again.
	down []int
	// value is the last value the guests stored: every store is a new one,
	// so a block read back says which store it was.
	value byte
}

// journalCampaignVMs is the campaign's VMs, one on each of the first two
// hosts.
var journalCampaignVMs = []string{"vm-0", "vm-1"}

// newJournalCampaign starts one seed's world.
func newJournalCampaign(t *testing.T, ctx context.Context, runtime *sim.Runtime) *journalCampaign {
	t.Helper()
	topology := simtest.Topology{Hosts: []string{"host-0", "host-1", "host-2"}}
	for index, id := range journalCampaignVMs {
		topology.VMs = append(topology.VMs, simtest.VMSpec{ID: id, Host: index, Volumes: []volume.VolumeSpec{
			{Name: simtest.MemoryVolume, Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
			{Name: simtest.DiskVolume, Size: 4 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}})
	}
	k := campaignKnobs(t, runtime, topology)
	k.LossWindow = 0
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology, Knobs: k,
		Prefix: newPrefix(t, "journals/"), Log: t.Logf, Journals: true, JournalBytes: campaignJournalBytes,
		JournalExpiry:      campaignJournalExpiry,
		CheckpointInterval: journalInterval})
	return &journalCampaign{t: t, ctx: ctx, runtime: runtime, world: world,
		choose: runtime.Random("simtest/journals")}
}

func (c *journalCampaign) up() []int {
	var up []int
	for index := range c.world.Hosts() {
		if !slices.Contains(c.down, index) {
			up = append(up, index)
		}
	}
	return up
}

func (c *journalCampaign) pick(name string, from []int) int {
	return from[c.choose.Intn(fmt.Sprintf("%d/%s", c.step, name), len(from))]
}

// running is the VMs a host runs now.
func (c *journalCampaign) running() []string {
	var running []string
	for _, id := range journalCampaignVMs {
		if c.world.VM(id) != nil {
			running = append(running, id)
		}
	}
	return running
}

// vm is one of the VMs a host runs, the seed's choice, or "" for none.
func (c *journalCampaign) vm(name string) string {
	running := c.running()
	if len(running) == 0 {
		return ""
	}
	return running[c.choose.Intn(fmt.Sprintf("%d/%s", c.step, name), len(running))]
}

// store has the guest of id store a new value into one to four pages of its
// disk, which the seed chooses.
func (c *journalCampaign) store(id string) {
	pages := []uint64{uint64(c.choose.Intn(fmt.Sprintf("%d/%s/%d/first", c.step, id, c.value), 4))}
	for page := range uint64(4) {
		if page != pages[0] && c.choose.Chance(fmt.Sprintf("%d/%s/%d/page-%d", c.step, id, c.value, page), 0.3) {
			pages = append(pages, page)
		}
	}
	c.storeInto(id, pages)
}

// storeInto has the guest of id store a new value into pages of its disk.
func (c *journalCampaign) storeInto(id string, pages []uint64) {
	c.t.Helper()
	if c.value == 255 {
		c.t.Fatal("the campaign ran out of values to store")
	}
	c.value++
	if err := c.world.StorePages(id, simtest.DiskVolume, pages, c.value); err != nil {
		c.t.Fatalf("step %d: %s storing into %v: %v", c.step, id, pages, err)
	}
}

// maximumFlushWait is the most a flush may wait for its answer while its
// host runs: a full ring holds it until the trim's turn, which the world's
// clocks reach as the wait goes on.
const maximumFlushWait = 10 * time.Minute

// answered waits for a flush's answer, moving the hosts' clocks on while it
// has none: a flush that waits for room on its ring may wait for its host to
// read the records of what the ring holds. It reports whether the flush was
// answered within maximumFlushWait. Either answer keeps its promise: a flush
// answered with success is checked at every recovery, and one answered with
// an error promised nothing.
func (c *journalCampaign) answered(id string, answer <-chan error) bool {
	for waited := time.Duration(0); waited < maximumFlushWait; waited += time.Minute {
		// The bubble's time is the simulation's: a minute of it is the
		// disks', the network's and the store's, and then the hosts'.
		minute := time.NewTimer(time.Minute)
		select {
		case err := <-answer:
			minute.Stop()
			if err != nil {
				c.t.Logf("step %d: a flush of %s failed: %v", c.step, id, err)
			}
			return true
		case <-minute.C:
		}
		c.world.Advance(time.Minute)
	}
	return false
}

// await is answered for a flush of a VM its host runs on: one left
// unanswered is a flush the host lost.
func (c *journalCampaign) await(id string, answer <-chan error) {
	c.t.Helper()
	if !c.answered(id, answer) {
		c.t.Fatalf("step %d: a flush of %s on host-%d went unanswered for %s", c.step, id, c.world.HostOf(id),
			maximumFlushWait)
	}
}

// flush stores and flushes one VM.
func (c *journalCampaign) flush() {
	id := c.vm("flushed")
	if id == "" {
		return
	}
	c.store(id)
	c.await(id, c.world.Flush(id, simtest.DiskVolume))
}

// fill flushes every page of one VM's disk, then two, then one, with no
// checkpoint between: the VM comes to hold more than half its journal's
// ring, and the last flush waits for its own checkpoint.
func (c *journalCampaign) fill() {
	id := c.vm("filled")
	if id == "" {
		return
	}
	for _, pages := range [][]uint64{{0, 1, 2, 3}, {0, 1}, {0}} {
		c.storeInto(id, pages)
		c.await(id, c.world.Flush(id, simtest.DiskVolume))
	}
}

// grouped stores into one page of one VM and flushes, three times without
// waiting, so the second and third flushes wait for the first's batch and
// share the next: each takes a page, and two fit in one batch.
func (c *journalCampaign) grouped() {
	id := c.vm("grouped")
	if id == "" {
		return
	}
	page := []uint64{uint64(c.choose.Intn(fmt.Sprintf("%d/grouped-page", c.step), 4))}
	var answers []<-chan error
	for range 3 {
		c.storeInto(id, page)
		answers = append(answers, c.world.Flush(id, simtest.DiskVolume))
	}
	for _, answer := range answers {
		c.await(id, answer)
	}
}

// race flushes one VM and checkpoints it at once: the checkpoint's pause may
// come between the flush choosing its pages and capturing them.
func (c *journalCampaign) race() {
	id := c.vm("raced")
	if id == "" {
		return
	}
	c.store(id)
	answer := c.world.Flush(id, simtest.DiskVolume)
	if err := c.world.Checkpoint(c.ctx, id); err != nil {
		c.t.Logf("step %d: checkpointing %s beside a flush: %v", c.step, id, err)
	}
	c.await(id, answer)
}

// checkpoint checkpoints one VM, which trims what it covers.
func (c *journalCampaign) checkpoint() {
	id := c.vm("checkpointed")
	if id == "" {
		return
	}
	if err := c.world.Checkpoint(c.ctx, id); err != nil {
		c.t.Logf("step %d: checkpointing %s: %v", c.step, id, err)
	}
}

// migrate moves one VM to another host that is up.
func (c *journalCampaign) migrate() {
	c.t.Helper()
	id := c.vm("migrated")
	if id == "" {
		return
	}
	from := c.world.HostOf(id)
	to := c.pick("destination", slices.DeleteFunc(c.up(), func(index int) bool { return index == from }))
	if err := c.world.Migrate(c.ctx, id, to); err != nil {
		c.t.Fatalf("step %d: migrating %s to host-%d: %v", c.step, id, to, err)
	}
}

// killed takes a host down, in a mode the seed chooses.
func (c *journalCampaign) killed(index int) {
	c.t.Helper()
	mode := sim.CrashProcess
	if c.choose.Chance(fmt.Sprintf("%d/power-loss", c.step), 0.5) {
		mode = sim.PowerLoss
	}
	if err := c.world.Kill(c.ctx, index, mode); err != nil {
		c.t.Fatal(err)
	}
	c.down = append(c.down, index)
}

// kill loses a host that is up.
func (c *journalCampaign) kill() { c.killed(c.pick("killed", c.up())) }

// killAnswered stores and flushes one VM, and kills its host the moment the
// guest has the answer.
func (c *journalCampaign) killAnswered() {
	id := c.vm("answered")
	if id == "" {
		return
	}
	c.store(id)
	c.await(id, c.world.Flush(id, simtest.DiskVolume))
	c.killed(c.world.HostOf(id))
}

// killSynced stores and flushes one VM, and kills its host once the host has
// answered and before the guest has the answer.
func (c *journalCampaign) killSynced() {
	c.t.Helper()
	id := c.vm("synced")
	if id == "" {
		return
	}
	c.store(id)
	at := c.world.HostOf(id)
	held := c.world.HoldFlush(id, simtest.DiskVolume)
	err := <-held.Answered()
	c.killed(at)
	if held.Deliver() {
		c.t.Fatalf("step %d: the guest of %s took an answer from host-%d after it died: %v", c.step, id, at, err)
	}
}

// takeover opens one VM on another host while its host still runs it and
// has a flush of it in flight, as a deployment that lost track of that host
// does: the read of its journal fences the VM there, refuses what it had not
// placed yet, and gives the VM up. A flush of a VM its host gives up may go
// unanswered.
func (c *journalCampaign) takeover() {
	c.t.Helper()
	id := c.vm("taken")
	if id == "" {
		return
	}
	from := c.world.HostOf(id)
	to := c.pick("taker", slices.DeleteFunc(c.up(), func(index int) bool { return index == from }))
	c.store(id)
	answer := c.world.Flush(id, simtest.DiskVolume)
	if err := c.world.Takeover(c.ctx, id, to); err != nil {
		c.t.Fatalf("step %d: taking %s over on host-%d: %v", c.step, id, to, err)
	}
	if !c.answered(id, answer) {
		c.t.Logf("step %d: the flush of %s in flight as host-%d took it over went unanswered", c.step, id, to)
	}
}

// ownDisk is the journal disk kept for the host at index's machine, if any.
func (c *journalCampaign) ownDisk(index int) (membership.Disk, bool) {
	machine := fmt.Sprintf("host-%d", index)
	for _, disk := range membershipOf(c.t, c.ctx, c.world).Disks() {
		if disk.Kind == membership.Journal && disk.Machine == machine {
			return disk, true
		}
	}
	return membership.Disk{}, false
}

// detachMidBatch fails the sync of the next batch on a host's journal disk,
// flushes a VM there, and detaches the disk by hand under the batch it never
// synced: the controller attaches it again and its host reads it back.
func (c *journalCampaign) detachMidBatch() {
	id := c.vm("detached")
	if id == "" {
		return
	}
	index := c.world.HostOf(id)
	disk, ok := c.ownDisk(index)
	if !ok || c.world.Cloud().Attached(disk.Volume) != fmt.Sprintf("host-%d", index) {
		return
	}
	c.world.Cloud().Disk(disk.Volume).FailNext(sim.DiskSync, 1)
	c.store(id)
	c.await(id, c.world.Flush(id, simtest.DiskVolume))
	if err := c.world.Cloud().Detach(c.ctx, disk.Volume, fmt.Sprintf("host-%d", index)); err != nil {
		c.t.Logf("step %d: detaching %s by hand: %v", c.step, disk.Volume, err)
	}
}

// leave takes a host out as the autoscaler removes its machine: it is
// unlisted, then it stops, and its VMs open elsewhere.
func (c *journalCampaign) leave() {
	index := c.pick("leaving", c.up())
	c.world.Unlist(c.ctx, index)
	if err := c.world.Shutdown(c.ctx, index); err != nil {
		c.t.Fatal(err)
	}
	c.down = append(c.down, index)
}

// join starts a host the schedule stopped.
func (c *journalCampaign) join() {
	index := c.pick("joining", c.down)
	if err := c.world.Restart(c.ctx, index); err != nil {
		c.t.Fatal(err)
	}
	c.down = slices.DeleteFunc(c.down, func(down int) bool { return down == index })
}

// run is the schedule: a seeded choice among what happens to flushes and
// their hosts, each step followed by a settle, which opens elsewhere what a
// lost host ran, and by every guest reading back what it wrote.
func (c *journalCampaign) run() {
	c.t.Helper()
	for c.step = 1; c.step <= 20; c.step++ {
		events := []string{"flush", "fill", "grouped", "race", "checkpoint", "migrate", "takeover",
			"detach-mid-batch"}
		if len(c.up()) > 2 {
			events = append(events, "kill", "kill-answered", "kill-synced", "leave")
		}
		if len(c.down) > 0 {
			events = append(events, "join")
		}
		event := events[c.choose.Intn(fmt.Sprintf("%d/event", c.step), len(events))]
		c.t.Logf("step %d: %s, hosts up %v, VMs running %v", c.step, event, c.up(), c.running())
		switch event {
		case "flush":
			c.flush()
		case "fill":
			c.fill()
		case "grouped":
			c.grouped()
		case "race":
			c.race()
		case "checkpoint":
			c.checkpoint()
		case "migrate":
			c.migrate()
		case "takeover":
			c.takeover()
		case "detach-mid-batch":
			c.detachMidBatch()
		case "kill":
			c.kill()
		case "kill-answered":
			c.killAnswered()
		case "kill-synced":
			c.killSynced()
		case "leave":
			c.leave()
		case "join":
			c.join()
		}
		if err := c.world.Settle(c.ctx); err != nil {
			c.t.Fatalf("step %d: settling: %v", c.step, err)
		}
		if err := c.world.Verify(c.ctx, simtest.ReadsMustSucceed); err != nil {
			c.t.Fatalf("step %d: a guest reads back what it did not write: %v", c.step, err)
		}
		if err := c.world.VerifyFlushes(); err != nil {
			c.t.Fatalf("step %d: %v", c.step, err)
		}
		// A minute on every host's clock: the hosts read the records of what
		// their journals hold, and trim what no record names.
		c.world.Advance(time.Minute)
	}
}

// journalRun is what one seed of the campaign did.
type journalRun struct {
	runtime          *sim.Runtime
	answered, failed int
	takeovers        int
	pending          int
}

// runJournalCampaign is one seed of the campaign.
func runJournalCampaign(t *testing.T, seed uint64) journalRun {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: seed, Buggify: true,
		Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
			ConnectLatency: time.Microsecond},
		ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
			PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
			BytesPerSecond: 1 << 40}})
	ctx := sim.WithRuntime(t.Context(), runtime)
	c := newJournalCampaign(t, ctx, runtime)
	c.run()
	if err := c.world.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.world.VerifyFlushes(); err != nil {
		t.Fatal(err)
	}
	if err := c.world.CheckOwnership(); err != nil {
		t.Fatal(err)
	}
	run := journalRun{runtime: runtime, takeovers: c.world.Takeovers(), pending: c.world.JournalPending()}
	for _, id := range journalCampaignVMs {
		answered, failed := c.world.Flushes(id)
		run.answered, run.failed = run.answered+answered, run.failed+failed
	}
	return run
}

// The journal campaign: flushes answered while their hosts die at every
// moment the schedule reaches, are taken over, scale down, and have their
// journal disks detached, while the journal's writes are slow, fail, tear and
// fail to sync, captures wait for a pause, rings fill, and the cloud is slow
// and fails to attach, detach and create disks. Every flush either succeeds
// and survives every recovery after it, or fails, and the guest knows which.
// Across the seeds every site fires, every journal probe is reached, flushes
// fail, VMs are recovered from their journals, and a recovery waits for a
// journal disk still moving to a survivor.
func TestJournalsSurviveTheirFaultsAndReachTheirProbes(t *testing.T) {
	reached, fired := map[string]uint64{}, map[string]uint64{}
	var total journalRun
	for seed := uint64(1); seed <= journalCampaignSeeds; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := runJournalCampaign(t, seed)
				for name, count := range run.runtime.Probes() {
					reached[name] += count
				}
				for site, count := range run.runtime.FiredSites() {
					fired[site] += count
				}
				total.answered += run.answered
				total.failed += run.failed
				total.takeovers += run.takeovers
				total.pending += run.pending
			})
		})
	}
	for _, probe := range journalProbes {
		unreached := slices.Contains(unreachedJournalProbes, probe)
		if reached[probe] == 0 && !unreached {
			t.Errorf("no seed reached %s", probe)
		}
		if reached[probe] > 0 && unreached {
			t.Errorf("%s is reached now; take it off unreachedJournalProbes", probe)
		}
	}
	for _, site := range journalSites {
		if fired[site] == 0 {
			t.Errorf("no seed fired %s", site)
		}
	}
	t.Logf("flushes answered %d, failed %d; takeovers %d; opens that waited for a journal %d",
		total.answered, total.failed, total.takeovers, total.pending)
	if total.answered == 0 || total.failed == 0 || total.takeovers == 0 || total.pending == 0 {
		t.Errorf("flushes answered %d and failed %d, takeovers %d, opens that waited for a journal %d: want some of each",
			total.answered, total.failed, total.takeovers, total.pending)
	}
}
