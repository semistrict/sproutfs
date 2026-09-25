//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/testnet"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// A deployment runs untrusted code in its guests. A guest must not hurt its
// neighbours beyond its own share of the host. When it exhausts a bound, the
// host stops it deliberately and logs why. These suites run two guests on one
// host and one pair of pagers: one hostile and one well-behaved. The host is the
// real one, with its checkpoint loop, its loss window and its answers to the
// pagers' pressure and to a guest's flush. So what the hostile guest does to its
// neighbour here is what it would do on a deployment's host.

const (
	// neighbourRAM is each guest's RAM.
	neighbourRAM = 128 << 20
	// neighbourAnswer bounds how long the well-behaved guest may take to answer
	// one request while its neighbour does its worst. Lima's timings are noisy,
	// so it is generous. A guest that is starved takes minutes or never answers.
	neighbourAnswer = 30 * time.Second
	// hogStart bounds how long a hostile load may take to go over the whole of
	// what it was given once.
	hogStart = 2 * time.Minute
)

// neighbourhood is one host running guests on one pair of pagers.
type neighbourhood struct {
	host   *host.Host
	pagers *hostPagers
	logs   *records
	closed chan string
}

// neighbourhoodConfig is what a suite sets of the host: its pagers' budgets and
// the loop's three bounds, each with host.Config's meaning.
type neighbourhoodConfig struct {
	pagers                           hostPagersConfig
	interval, lossWindow, flushBound time.Duration
}

// neighbour is one guest of a neighbourhood. rounds counts the rounds of work
// it answered, and slowest is the longest of them, which a test reports.
type neighbour struct {
	id      string
	vm      *volume.VM
	process *vmmachine.Process

	rounds  int
	slowest time.Duration
	which   string
}

func newNeighbourhood(t *testing.T, ctx context.Context, cfg neighbourhoodConfig) *neighbourhood {
	t.Helper()
	logs := capturing(t)
	resources := testresource.New()
	cfg.pagers.Resources = resources
	n := &neighbourhood{pagers: newConfiguredHostPagers(t, ctx, cfg.pagers), logs: logs,
		closed: make(chan string, 8)}
	runtime := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Nanosecond,
		GetLatency: time.Nanosecond, PutLatency: time.Nanosecond, ListLatency: time.Nanosecond,
		DeleteLatency: time.Nanosecond, BytesPerSecond: 1 << 50}})
	h, err := host.StartHost(ctx, host.Config{Network: testnet.New(), Resources: resources,
		ObjectStore: runtime.ObjectStore(), CacheBytes: 64 << 20, Pagers: n.pagers.pagers,
		CheckpointInterval: cfg.interval, LossWindow: cfg.lossWindow, FlushBound: cfg.flushBound,
		MachineClosed: func(id string) {
			select {
			case n.closed <- id:
			default:
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	n.host = h
	t.Cleanup(func() {
		if err := h.Close(context.Background()); err != nil {
			t.Errorf("closing the host: %v", err)
		}
		// What the host said is the account of a failure: which VM it stopped
		// and why, and what its checkpoints did.
		if t.Failed() {
			logs.mu.Lock()
			defer logs.mu.Unlock()
			for _, entry := range logs.logs {
				t.Logf("host log: %s %s %v", entry.level, entry.message, entry.attrs)
			}
		}
	})
	return n
}

// boot creates one VM of the qualification image on this host, boots it, and
// registers it once its agent is serving, so the host checkpoints it on its
// interval and answers for it under pressure from then on.
func (n *neighbourhood) boot(t *testing.T, ctx context.Context, binaryPath, name string) *neighbour {
	t.Helper()
	vm, err := n.host.Volumes().Create(ctx, name, []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: neighbourRAM, PageSize: ramPageBytes(t)},
		{Name: "root", Size: guestRootBytes, PageSize: checkpoint.PageSize2MiB},
	})
	if err != nil {
		t.Fatal(err)
	}
	loadRootImage(t, ctx, vm.Volume("root"))
	if err := vm.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	config := migrationConfig(t, binaryPath, n.pagers, vm)
	config.Starter.(*vmmachine.Firecracker).VsockCID = guestVsockCID
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	// The process closes before the scratch it runs in, and its loop stops
	// before that, so no checkpoint pauses a process that is gone.
	t.Cleanup(func() {
		n.host.RemoveMachine(name)
		_ = p.Close()
	})
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	waitLine(t, ctx, p, fmt.Sprintf("sproutfs-guest-agent: serving on vsock port %d", guest.Port), 0)
	if err := n.host.AddMachine(name, p); err != nil {
		t.Fatal(err)
	}
	return &neighbour{id: name, vm: vm, process: p}
}

// hog starts one hostile load in a guest and returns once the load has been
// over the whole of what it was given once.
func (g *neighbour) hog(t *testing.T, ctx context.Context, kind string, mib int) {
	t.Helper()
	reader := newConsole(g.process)
	if err := reader.skipExisting(); err != nil {
		t.Fatal(err)
	}
	if err := g.process.WriteConsole(ctx, fmt.Appendf(nil, "hog %s %d\n", kind, mib)); err != nil {
		t.Fatal(err)
	}
	started, cancel := context.WithTimeout(ctx, hogStart)
	defer cancel()
	for _, want := range []string{"SPROUTFS_HOG kind=" + kind, "SPROUTFS_HOG_PASS kind=" + kind} {
		if _, err := reader.wait(started, want); err != nil {
			t.Fatalf("%s's %s load: %v\n%s", g.id, kind, err, consoleText(g.process))
		}
	}
}

// answers is one round of a well-behaved guest's work, each request of it
// bounded by neighbourAnswer: a RAM store, a disk store the guest syncs, which
// is a flush the host answers, both read back, and a command run by its agent
// over the vsock. It keeps the slowest round for report.
func (g *neighbour) answers(t *testing.T, ctx context.Context, round uint64) {
	t.Helper()
	start := time.Now()
	var took []string
	for _, exchange := range []struct{ line, want string }{
		{fmt.Sprintf("ram %d\n", round), fmt.Sprintf("SPROUTFS_RAM ram=%d", round)},
		{fmt.Sprintf("write %d\n", round), fmt.Sprintf("SPROUTFS_FLUSH disk=%d", round)},
		{"read\n", fmt.Sprintf("SPROUTFS_VALUE ram=%d disk=%d", round, round)},
	} {
		bounded, cancel := context.WithTimeout(ctx, neighbourAnswer)
		began := time.Now()
		err := guestCommand(bounded, g.process, exchange.line, exchange.want)
		cancel()
		if err != nil {
			t.Fatalf("%s did not answer %q within %s while its neighbour ran: %v\n%s",
				g.id, exchange.line, neighbourAnswer, err, consoleText(g.process))
		}
		took = append(took, fmt.Sprintf("%s %s", strings.Fields(exchange.line)[0], time.Since(began).Round(time.Millisecond)))
	}
	bounded, cancel := context.WithTimeout(ctx, neighbourAnswer)
	defer cancel()
	began := time.Now()
	result, err := guestExec(bounded, g.process, guest.ExecRequest{Cmd: fmt.Sprintf("echo %d", round)})
	if err != nil {
		t.Fatalf("%s's agent did not answer within %s while its neighbour ran: %v\n%s",
			g.id, neighbourAnswer, err, consoleText(g.process))
	}
	if result.Exit != 0 || result.Stdout != fmt.Sprintf("%d\n", round) {
		t.Fatalf("%s's agent answered %+v, want %d", g.id, result, round)
	}
	took = append(took, fmt.Sprintf("exec %s", time.Since(began).Round(time.Millisecond)))
	g.rounds++
	if all := time.Since(start); all > g.slowest {
		g.slowest, g.which = all, fmt.Sprintf("round %d: %s", round, strings.Join(took, ", "))
	}
}

// report logs how the rounds of work went.
func (g *neighbour) report(t *testing.T) {
	t.Helper()
	t.Logf("%s answered %d rounds; the slowest was %s", g.id, g.rounds, g.which)
}

// withinBudget checks what the host holds against what it was given: no pager
// ever held more dirty pages than its budget or more resident pages than its
// arena, and the arenas hold no more memory than they were sized for.
func (n *neighbourhood) withinBudget(t *testing.T, ctx context.Context) {
	t.Helper()
	for kind, pager := range map[vmmemory.MemoryRegionKind]*vmmemory.Host{
		vmmemory.Ram: n.pagers.pagers.Ram, vmmemory.Pmem: n.pagers.pagers.Pmem} {
		stats, err := pager.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		config := n.pagers.configs[kind]
		if stats.PeakDirtyPages > config.DirtyPages {
			t.Errorf("the %s pager held %d dirty pages, past its budget of %d", kind, stats.PeakDirtyPages, config.DirtyPages)
		}
		if stats.PeakResidentPages > config.ResidentPages {
			t.Errorf("the %s pager held %d resident pages, past its arena of %d", kind, stats.PeakResidentPages, config.ResidentPages)
		}
	}
	allocated, err := n.pagers.AllocatedBytes()
	if err != nil {
		t.Fatal(err)
	}
	if allocated > n.pagers.capacity {
		t.Errorf("the arenas hold %d bytes, past the %d they were sized for", allocated, n.pagers.capacity)
	}
}

// noneClosed checks that the host gave up no VM on its own.
func (n *neighbourhood) noneClosed(t *testing.T) {
	t.Helper()
	select {
	case id := <-n.closed:
		t.Fatalf("the host stopped %s", id)
	default:
	}
}

// statsOf is one pager's counters, for a test to report or check.
func statsOf(t *testing.T, ctx context.Context, pager *vmmemory.Host) vmmemory.Stats {
	t.Helper()
	stats, err := pager.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

// TestAGuestTouchingAllItsRAMLeavesItsNeighbourItsWorkingSet: a guest that
// stores into all of its RAM over and over faults on nearly every store. The
// arena here is three eighths of the RAM the two guests map, a share of 24
// pages each at 2 MiB. That holds the neighbour's working set and not the
// hog's, so every pass of the hog evicts and spills. The neighbour's working
// set, agent and all, must go on answering within a bound, and the arena must
// never hold more than it has. At a quarter the share is below the working set
// of a booted guest, and the neighbour thrashes on its own. The dirty budget
// holds both guests' RAM, so this is residency and not the dirty budget.
func TestAGuestTouchingAllItsRAMLeavesItsNeighbourItsWorkingSet(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	n := newNeighbourhood(t, ctx, neighbourhoodConfig{
		pagers: hostPagersConfig{
			RAM:  hostPagerBudgets{Arena: 3 * neighbourRAM / 4, Logical: 4 * neighbourRAM, Dirty: 2 * neighbourRAM},
			PMEM: hostPagerBudgets{Arena: 2 * guestRootBytes, Logical: 4 * guestRootBytes, Dirty: 2 * guestRootBytes},
		},
		interval: 2 * time.Second,
	})
	calm := n.boot(t, ctx, binaryPath, "calm")
	t.Logf("after the calm guest booted: RAM %s", brief(statsOf(t, ctx, n.pagers.pagers.Ram)))
	hostile := n.boot(t, ctx, binaryPath, "hostile")
	t.Logf("after the hostile guest booted: RAM %s", brief(statsOf(t, ctx, n.pagers.pagers.Ram)))

	const workingSet = 16
	command(t, ctx, calm.process, fmt.Sprintf("pressure %d\n", workingSet),
		fmt.Sprintf("SPROUTFS_PRESSURE bytes=%d", workingSet<<20))
	before := statsOf(t, ctx, n.pagers.pagers.Ram)
	hostile.hog(t, ctx, "ram", 80)
	t.Logf("after the hog's first pass: RAM %s", brief(statsOf(t, ctx, n.pagers.pagers.Ram)))

	for round := uint64(1); round <= 4; round++ {
		began := time.Now()
		bounded, cancel := context.WithTimeout(ctx, neighbourAnswer)
		err := guestCommand(bounded, calm.process, "checkpressure\n",
			fmt.Sprintf("SPROUTFS_PRESSURE_OK bytes=%d", workingSet<<20))
		cancel()
		if err != nil {
			t.Fatalf("round %d: the calm guest's working set did not fault back in within %s: %v\n%s",
				round, neighbourAnswer, err, consoleText(calm.process))
		}
		t.Logf("round %d: the working set faulted back in in %s", round, time.Since(began).Round(time.Millisecond))
		calm.answers(t, ctx, round)
	}

	after := statsOf(t, ctx, n.pagers.pagers.Ram)
	t.Logf("RAM while the hog ran: %d evictions, %d spills, %d refaults, %d dirty stalls",
		after.Evictions-before.Evictions, after.Spills-before.Spills,
		after.SpillRefaults-before.SpillRefaults, after.DirtyStalls)
	if after.Evictions == before.Evictions || after.Spills == before.Spills || after.SpillRefaults == before.SpillRefaults {
		t.Fatalf("the hog evicted %d, spilled %d and refaulted %d RAM pages: the arena was never pressed",
			after.Evictions-before.Evictions, after.Spills-before.Spills, after.SpillRefaults-before.SpillRefaults)
	}
	// The hog only used its own RAM, which the dirty budget holds, so nothing
	// was stalled and nothing stopped.
	if after.DirtyStalls != 0 {
		t.Fatalf("%d RAM stores were stalled under a budget that holds both guests", after.DirtyStalls)
	}
	n.noneClosed(t)
	command(t, ctx, hostile.process, "read\n", "SPROUTFS_VALUE ram=7")
	calm.report(t)
	n.withinBudget(t, ctx)
}

// brief is the part of a pager's counters these suites read.
func brief(s vmmemory.Stats) string {
	return fmt.Sprintf("resident %d, dirty %d (peak %d), evictions %d, spills %d, refaults %d, "+
		"checkpoint requests %d, dirty waits %d, dirty stalls %d, window waits %d, window stalls %d, flushes %d",
		s.ResidentPages, s.DirtyPages, s.PeakDirtyPages, s.Evictions, s.Spills, s.SpillRefaults,
		s.CheckpointRequests, s.DirtyWaits, s.DirtyStalls, s.WindowWaits, s.WindowStalls, s.Flushes)
}

// count is how many records whose message contains want name vm.
func (r *records) count(want, vm string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, entry := range r.logs {
		if strings.Contains(entry.message, want) && entry.attrs["vm"] == vm {
			n++
		}
	}
	return n
}

// TestAGuestDirtyingPastItsBudgetsIsCheckpointedThenStopped: a guest that
// dirties its DAX disk and its RAM as fast as it can runs past the PMEM dirty
// budget and the RAM dirty budget. Its interval is far away, so only the host's
// answers to the pagers can keep it going. The disk is made durable out of
// turn, which relieves the PMEM budget. Nothing relieves RAM, so when the RAM
// budget is full the host stops the guest that holds the most of it,
// deliberately and with a logged reason. The neighbour shares both budgets. It
// must answer throughout, keep its bytes, and never be the one stopped.
func TestAGuestDirtyingPastItsBudgetsIsCheckpointedThenStopped(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	n := newNeighbourhood(t, ctx, neighbourhoodConfig{
		pagers: hostPagersConfig{
			// Room for both guests to boot and for the neighbour's work, and not
			// for the hog: two booted guests hold about 42 of these 64 pages.
			RAM: hostPagerBudgets{Arena: neighbourRAM, Logical: 4 * neighbourRAM, Dirty: neighbourRAM},
			// Both roots stay resident, and the dirty budget is eight 2 MiB pages,
			// a third of the file the hog writes over and over.
			PMEM: hostPagerBudgets{Arena: 2 * guestRootBytes, Logical: 4 * guestRootBytes, Dirty: 16 << 20},
		},
		interval: time.Hour,
	})
	calm := n.boot(t, ctx, binaryPath, "calm")
	hostile := n.boot(t, ctx, binaryPath, "hostile")
	const workingSet = 8
	command(t, ctx, calm.process, fmt.Sprintf("pressure %d\n", workingSet),
		fmt.Sprintf("SPROUTFS_PRESSURE bytes=%d", workingSet<<20))
	calm.answers(t, ctx, 1)
	published := hostile.vm.Status().Checkpoint

	// The disk first. The hog's first pass alone is three dirty budgets, so it
	// completes only if the host checkpoints it out of turn.
	hostile.hog(t, ctx, "disk", 24)
	for round := uint64(2); round <= 4; round++ {
		calm.answers(t, ctx, round)
	}
	budget := statsOf(t, ctx, n.pagers.pagers.Pmem)
	t.Logf("PMEM once the budget held the hog: %s", brief(budget))
	if budget.DirtyWaits == 0 || budget.CheckpointRequests == 0 {
		t.Fatalf("the hog wrote three dirty budgets with %d waits for the budget and %d checkpoints asked for",
			budget.DirtyWaits, budget.CheckpointRequests)
	}
	if hostile.vm.Status().Checkpoint == published {
		t.Fatal("the hog wrote three dirty budgets with no checkpoint of its own landing")
	}
	n.noneClosed(t)

	// Then RAM, which no checkpoint the host takes relieves. The hog is stopped;
	// the neighbour goes on answering while it is, and after.
	if err := hostile.process.WriteConsole(ctx, []byte("hog ram 80\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for round, stopped := uint64(5), false; !stopped; round++ {
		if time.Now().After(deadline) {
			t.Fatalf("the host had not stopped the RAM hog %s after it started\nRAM: %s",
				3*time.Minute, brief(statsOf(t, ctx, n.pagers.pagers.Ram)))
		}
		calm.answers(t, ctx, round)
		select {
		case id := <-n.closed:
			if id != hostile.id {
				t.Fatalf("the host stopped %s, want the hog", id)
			}
			stopped = true
		default:
		}
	}
	attrs := n.logs.await(t, "host: stopped a VM whose stores the dirty budget could not admit", slog.LevelWarn)
	if attrs["vm"] != hostile.id || !strings.Contains(attrs["cause"], "managed-memory dirty budget stalled") {
		t.Fatalf("the stop was logged as %v, want the hog and the dirty budget", attrs)
	}
	t.Logf("RAM when the hog was stopped: %s", brief(statsOf(t, ctx, n.pagers.pagers.Ram)))
	for round := uint64(100); round <= 102; round++ {
		calm.answers(t, ctx, round)
	}
	command(t, ctx, calm.process, "checkpressure\n", fmt.Sprintf("SPROUTFS_PRESSURE_OK bytes=%d", workingSet<<20))
	if running := n.host.Machines(); len(running) != 1 || running[0] != calm.id {
		t.Fatalf("the host runs %v, want only the neighbour", running)
	}
	n.noneClosed(t)
	calm.report(t)
	n.withinBudget(t, ctx)
}

// TestAGuestStormingFlushesLeavesItsNeighboursFlushes: a guest that writes one
// block and fsyncs it in a loop sends a virtio-pmem flush for every fsync. The
// host answers each at once while the guest's disk is fresh, and holds it for
// one checkpoint taken out of turn once the guest's oldest unpublished write is
// older than the flush bound. So the storm costs its host at most one
// checkpoint of the storming guest per flush bound, beside the interval's own.
// The neighbour's flushes must still complete within a bound, and its bytes
// must be intact.
func TestAGuestStormingFlushesLeavesItsNeighboursFlushes(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	const interval, flushBound, storm = 4 * time.Second, time.Second, 20 * time.Second
	n := newNeighbourhood(t, ctx, neighbourhoodConfig{
		pagers: hostPagersConfig{
			RAM:  hostPagerBudgets{Arena: 2 * neighbourRAM, Logical: 4 * neighbourRAM, Dirty: 2 * neighbourRAM},
			PMEM: hostPagerBudgets{Arena: 2 * guestRootBytes, Logical: 4 * guestRootBytes, Dirty: 2 * guestRootBytes},
		},
		interval:   interval,
		flushBound: flushBound,
	})
	calm := n.boot(t, ctx, binaryPath, "calm")
	hostile := n.boot(t, ctx, binaryPath, "hostile")
	calm.answers(t, ctx, 1)

	before := statsOf(t, ctx, n.pagers.pagers.Pmem)
	checkpoints := n.logs.count("host: checkpoint", hostile.id)
	began := time.Now()
	hostile.hog(t, ctx, "sync", 1)
	for round := uint64(2); time.Since(began) < storm; round++ {
		calm.answers(t, ctx, round)
	}
	took := time.Since(began)
	after := statsOf(t, ctx, n.pagers.pagers.Pmem)
	taken := n.logs.count("host: checkpoint", hostile.id) - checkpoints
	t.Logf("in %s of the storm: %d flushes, %d checkpoints of the storming guest; PMEM %s",
		took.Round(time.Millisecond), after.Flushes-before.Flushes, taken, brief(after))
	// The hog's first pass is 256 fsyncs, each at least one flush.
	if after.Flushes-before.Flushes < 256 {
		t.Fatalf("the storm sent %d flushes, fewer than the 256 fsyncs of its first pass", after.Flushes-before.Flushes)
	}
	if bound := int(took/flushBound) + int(took/interval) + 2; taken > bound {
		t.Fatalf("the storm cost %d checkpoints of the storming guest in %s, past the %d that one per flush bound and one per interval allow",
			taken, took.Round(time.Millisecond), bound)
	}
	command(t, ctx, hostile.process, "read\n", "SPROUTFS_VALUE ram=7")
	n.noneClosed(t)
	calm.report(t)
	n.withinBudget(t, ctx)
}

// TestAGuestFloodingItsVsockLeavesTheHostsExec: a guest that connects to the
// host over its vsock as fast as it can reaches nothing, because a host serves
// nothing a guest can connect to. Each attempt is its own VMM's work. The
// host's exec still reaches the flooding guest's agent within its bound, and
// the neighbour goes on answering.
func TestAGuestFloodingItsVsockLeavesTheHostsExec(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	n := newNeighbourhood(t, ctx, neighbourhoodConfig{
		pagers: hostPagersConfig{
			RAM:  hostPagerBudgets{Arena: 2 * neighbourRAM, Logical: 4 * neighbourRAM, Dirty: 2 * neighbourRAM},
			PMEM: hostPagerBudgets{Arena: 2 * guestRootBytes, Logical: 4 * guestRootBytes, Dirty: 2 * guestRootBytes},
		},
		interval: 4 * time.Second,
	})
	calm := n.boot(t, ctx, binaryPath, "calm")
	hostile := n.boot(t, ctx, binaryPath, "hostile")
	hostile.hog(t, ctx, "vsock", 2000)
	for round := uint64(1); round <= 3; round++ {
		calm.answers(t, ctx, round)
		bounded, cancel := context.WithTimeout(ctx, neighbourAnswer)
		result, err := guestExec(bounded, hostile.process, guest.ExecRequest{Cmd: fmt.Sprintf("echo %d", round)})
		cancel()
		if err != nil {
			t.Fatalf("the flooding guest's agent did not answer within %s: %v\n%s",
				neighbourAnswer, err, consoleText(hostile.process))
		}
		if result.Exit != 0 || result.Stdout != fmt.Sprintf("%d\n", round) {
			t.Fatalf("the flooding guest's agent answered %+v, want %d", result, round)
		}
	}
	n.noneClosed(t)
	calm.report(t)
	n.withinBudget(t, ctx)
}

// TestAGuestPastItsLossWindowIsCheckpointedOutOfTurn: a guest that writes two
// pages of its disk over and over stays inside the dirty budget, so only the
// loss window holds it back. With the interval an hour away, every store past
// the window waits for a checkpoint taken out of turn. The neighbour's own
// writes age past the same window. Both must go on answering.
//
// It fails on the aarch64 Lima instance, and the defect is not fixed: see
// TASK-1 in backlog/tasks. A store the window holds keeps its vCPU inside a
// userfault. The checkpoint that would end the wait has to pause that vCPU
// first, and the pause's signal does not bring it out. Firecracker gives up on
// the pause after 30 seconds, and the loop then backs off without answering the
// pager's requests, so the guest stops for an eighth of the interval.
func TestAGuestPastItsLossWindowIsCheckpointedOutOfTurn(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	const window = 2 * time.Second
	n := newNeighbourhood(t, ctx, neighbourhoodConfig{
		pagers: hostPagersConfig{
			RAM:        hostPagerBudgets{Arena: 2 * neighbourRAM, Logical: 4 * neighbourRAM, Dirty: 2 * neighbourRAM},
			PMEM:       hostPagerBudgets{Arena: 2 * guestRootBytes, Logical: 4 * guestRootBytes, Dirty: 2 * guestRootBytes},
			LossWindow: window,
		},
		interval:   time.Hour,
		lossWindow: window,
	})
	calm := n.boot(t, ctx, binaryPath, "calm")
	hostile := n.boot(t, ctx, binaryPath, "hostile")
	calm.answers(t, ctx, 1)
	hostile.hog(t, ctx, "disk", 4)
	checkpoints := n.logs.count("host: checkpoint", hostile.id)
	began := time.Now()
	for round := uint64(2); time.Since(began) < 10*window; round++ {
		calm.answers(t, ctx, round)
	}
	pmem := statsOf(t, ctx, n.pagers.pagers.Pmem)
	taken := n.logs.count("host: checkpoint", hostile.id) - checkpoints
	t.Logf("in %s past the window: %d checkpoints of the hog; PMEM %s",
		time.Since(began).Round(time.Millisecond), taken, brief(pmem))
	// Every window the hog outlives ends in a checkpoint of it out of turn.
	if pmem.WindowWaits == 0 || taken < 4 {
		t.Fatalf("the window held %d stores and %d checkpoints of the hog landed in %s, want waits and at least 4",
			pmem.WindowWaits, taken, 10*window)
	}
	command(t, ctx, hostile.process, "read\n", "SPROUTFS_VALUE ram=7")
	n.noneClosed(t)
	calm.report(t)
	n.withinBudget(t, ctx)
}
