package simtest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The scheduled scenario is the one workload recording, replay and byte-exact
// cross-process reproduction run over. Everything it drives is a real
// deployment — the volume managers, the checkpoint store, the control records,
// the pagers, the page servers and the migration coordinator, on the simulated
// network, object store, disks and clocks of internal/platform/sim — and the
// shared controller chooses every completion order, so two runs of one seed
// must produce byte-identical execution and adapter traces however the Go
// scheduler ran them.
//
// It has three halves, because that is what one deployment does: volumes that
// are written, checkpointed, forked, fenced and taken over; a guest that is
// handed from host to host through every way a handover can fail; and hosts
// that drain, are lost and come back.

const (
	// scheduledVMID is the VM a guest runs and every handover moves, and
	// scheduledVolumeID the one a writer drives through its volumes alone —
	// written, discarded, forked over and taken over — with no guest mapping it.
	scheduledVMID     = "vm-a"
	scheduledVolumeID = "scheduled-volume"
	scheduledForkID   = "scheduled-fork"
)

// scheduledHosts is how many hosts the scenario runs: one the VMs start on, one
// they are handed to, and one bystander that takes a fenced VM over.
const scheduledHosts = 3

// runScheduledWorld runs one seed of the scenario in both halves of the
// deployment and returns its recording and the runtime whose trace carries the
// fingerprint of everything the simulated dependencies did.
func runScheduledWorld(t *testing.T, seed uint64, reverse bool) (sim.Recording, *sim.Runtime) {
	t.Helper()
	var scheduler *sim.Scheduler
	var runtime *sim.Runtime
	// One line per seed comparing the simulated time this scenario explored
	// against the wall time it took. A nightly block is budgeted against that
	// ratio, and a collapse in it is a real wait inside a virtual-time test.
	bubble := testsoak.Start(scheduledCampaignName, seed)
	synctest.Test(t, func(t *testing.T) {
		defer bubble.Simulated(time.Now())
		scheduler = sim.NewScheduler(seed)
		done := make(chan struct{})
		result := make(chan error, 1)
		go func() { result <- scheduler.Run(done) }()
		func() {
			defer close(done)
			runtime = sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait,
				Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
					ConnectLatency: time.Microsecond},
				ObjectStore: sim.ObjectStoreConfig{GetLatency: time.Microsecond,
					PutLatency: time.Microsecond, BytesPerSecond: 1 << 40}})
			driveScheduledWorld(t, runtime, scheduler, seed, reverse)
		}()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	})
	recording, err := scheduler.Recording(runtime.Trace())
	if err != nil {
		t.Fatal(err)
	}
	bubble.Report(t, runtime)
	return recording, runtime
}

const scheduledCampaignName = "scheduled-world"

func driveScheduledWorld(t *testing.T, runtime *sim.Runtime, scheduler *sim.Scheduler, seed uint64, reverse bool) {
	t.Helper()
	prefix := newPrefix(t, "scheduled/")
	// Deciding to ask a source for pages is the destination's own decision, not
	// an adapter operation: a stream closed between two requests may either send
	// the next one and have it refused on the wire or abandon it, and a pooled
	// connection leaves nothing admitted in between. Naming the memory region makes the
	// decision one the scheduler orders against the cancellation that stops it.
	// The probes and buggified sites inside the real host, volume, checkpoint,
	// control, pager and migration code read the runtime out of the context;
	// without it they are no-ops.
	ctx := vmmigrate.WithAdmission(sim.WithRuntime(t.Context(), runtime),
		func(ctx context.Context, memoryRegion string) error {
			return runtime.Admit(sim.WithTask(ctx, "post-copy/"+memoryRegion), "peer/request")
		})
	specs := []volume.VolumeSpec{{Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage},
		{Name: "ram0", Size: 3 * simtest.RAMPage, PageSize: simtest.RAMPage}}
	topology := simtest.Topology{Hosts: []string{"host-0", "host-1", "host-2"},
		VMs: []simtest.VMSpec{{ID: scheduledVMID, Host: 0, Volumes: specs}}}
	k := knobs.Defaults()
	k.ResidentPages, k.DirtyPages, k.LogicalPages = 32, 32, 128
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: k, Prefix: prefix, Log: t.Logf, ReverseMemoryRegions: reverse,
		Admit: func(ctx context.Context, id string) error { return scheduler.Wait(ctx, id, 0, 0) }})
	closed := false
	defer func() {
		if !closed {
			_ = world.Close(ctx)
		}
	}()

	scheduledVolumeWorkload(t, ctx, world, scheduler, seed, reverse)
	scheduledHandoverWorkload(t, ctx, world, runtime, scheduler)
	scheduledHostWorkload(t, ctx, world, runtime, scheduler)

	if err := world.CheckSelected(ctx); err != nil {
		t.Error(err)
	}
	closed = true
	if err := world.Close(ctx); err != nil {
		t.Error(err)
	}
	scheduler.Record("world/end", "checked", nil)
	// Every handle is closed, so what the store holds is the whole of this
	// deployment's durable state and it must still be a deployment. The
	// allowances are a collector's work: the checkpoints each takeover left
	// behind, the parts of a publication that never reached its index, a
	// checkpoint no later sweep came back for, and the objects of a VM whose
	// record a delete already took.
	if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
		volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex,
		volume.AllowUnreferencedCheckpoint, volume.AllowUnrecordedVM); err != nil {
		t.Error(err)
	}
}

// model is the bytes an independent reader expects of every volume of one VM.
type model map[string][]byte

func newModel(specs []volume.VolumeSpec) model {
	m := model{}
	for _, spec := range specs {
		m[spec.Name] = make([]byte, spec.Size)
	}
	return m
}

func (m model) clone() model {
	other := model{}
	for name, data := range m {
		other[name] = bytes.Clone(data)
	}
	return other
}

// scheduledVolumeWorkload exercises parallel checkpoint publication, a fork
// over a checkpoint, a discard, failed and lost-reply publications, and the
// takeover that fences a handle — all through one VM's volumes, with no guest
// mapping them. The model derives only from acknowledged operations; checkpoint
// bytes never define the expected data.
func scheduledVolumeWorkload(t *testing.T, ctx context.Context, world *simtest.World,
	scheduler *sim.Scheduler, seed uint64, reverse bool) {
	t.Helper()
	specs := []volume.VolumeSpec{{Name: "disk", Size: 2 * checkpoint.PageSize2MiB, PageSize: simtest.PMEMPage},
		{Name: "ram", Size: 2 * checkpoint.PageSize2MiB, PageSize: simtest.PMEMPage}}
	manager := world.Host(0).Volumes()
	vm, err := manager.Create(ctx, scheduledVolumeID, specs)
	if err != nil {
		t.Fatal(err)
	}
	want := newModel(specs)
	check := func(label string, handle *volume.VM, expected model) {
		t.Helper()
		for _, spec := range specs {
			got := make([]byte, spec.Size)
			if err := handle.Volume(spec.Name).Read(ctx, 0, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, expected[spec.Name]) {
				t.Fatalf("%s: %s differs from acknowledged bytes", label, spec.Name)
			}
			digest := sha256.Sum256(got)
			scheduler.Record(label+"/"+spec.Name, "bytes-match/sha256", digest[:])
		}
	}
	write := func(handle *volume.VM, expected model, name string, offset uint64, value byte) {
		t.Helper()
		data := bytes.Repeat([]byte{value}, checkpoint.SectorSize)
		if err := handle.Volume(name).Write(ctx, offset, data); err != nil {
			t.Fatal(err)
		}
		copy(expected[name][offset:], data)
	}
	// Different callers contend for one VM's overlay, then diverge into
	// different volume and page objects during publication.
	order := []int{0, 1, 2, 3}
	if reverse {
		slices.Reverse(order)
	}
	var wg sync.WaitGroup
	for _, i := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := scheduler.Wait(ctx, fmt.Sprintf("volume/write/%d", i), 0, 0); err != nil {
				t.Error(err)
				return
			}
			write(vm, want, specs[i/2].Name, uint64(i%2)*checkpoint.PageSize2MiB, byte(seed%200)+byte(i)+1)
			if err := scheduler.Wait(ctx, fmt.Sprintf("volume/ack/%d", i), 0, 0); err != nil {
				t.Error(err)
				return
			}
			scheduler.Record(fmt.Sprintf("volume/ack/%d", i), "acknowledged", nil)
		}()
	}
	wg.Wait()
	check("initial", vm, want)
	checkpointModel := want.clone()
	state := []byte("captured VMM registers")
	ckpt, err := vm.Snapshot(ctx, volume.Prepared(state, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	point, err := vm.ForkPoint(ctx, volume.Prepared(state, nil))
	if err != nil {
		t.Fatal(err)
	}
	fork, err := manager.Fork(ctx, scheduledForkID, point)
	if err != nil {
		t.Fatal(err)
	}
	forkModel := checkpointModel.clone()
	write(vm, want, "disk", 0, 211)
	write(fork, forkModel, "ram", checkpoint.PageSize2MiB, 212)
	if err := fork.Volume("disk").Discard(ctx, 0, checkpoint.SectorSize); err != nil {
		t.Fatal(err)
	}
	clear(forkModel["disk"][:checkpoint.SectorSize])
	// The immutable checkpoint must retain the earlier bytes while both
	// descendants change, even before publication completes.
	for _, spec := range specs {
		got := make([]byte, spec.Size)
		if err := ckpt.Read(ctx, spec.Name, 0, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, checkpointModel[spec.Name]) {
			t.Fatal("checkpoint changed after capture")
		}
		digest := sha256.Sum256(got)
		scheduler.Record("checkpoint/"+spec.Name, "immutable/sha256", digest[:])
	}
	if !bytes.Equal(ckpt.State(), state) {
		t.Fatal("captured VMM state changed")
	}
	if err := fork.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := point.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	check("parent-diverged", vm, want)
	check("fork-diverged", fork, forkModel)

	// A failed publication must preserve the old selected checkpoint and all
	// acknowledged bytes. A retry must publish them without a rollback.
	before := vm.Status().Checkpoint
	world.Runtime().ObjectStore().FailNext(sim.ObjectPut, 1)
	if err := vm.Checkpoint(ctx); err == nil {
		t.Fatal("injected publication failure was ignored")
	}
	if vm.Status().Checkpoint != before {
		t.Fatal("failed publication selected a checkpoint")
	}
	check("failed-publication", vm, want)
	if err := vm.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.Record("publication/retry", "selected", nil)
	write(vm, want, "ram", checkpoint.SectorSize, 213)
	// Lost upload replies are reconciled by the checkpoint store; data remains
	// correct whether this attempt completes or leaves work for a retry.
	world.Runtime().ObjectStore().FailNextAfterApply(sim.ObjectPut, 1)
	_ = vm.Checkpoint(ctx)
	if err := vm.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	check("lost-publication-reply", vm, want)

	// A takeover fences the handle that held the VM. What the fenced handle had
	// published is what the new one opens; what it merely held is gone, which
	// is what losing its host would have cost.
	write(vm, want, "disk", checkpoint.SectorSize, 214)
	if err := vm.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	selected := vm.Status().Checkpoint
	elsewhere := world.Host(2).Volumes()
	reopened, err := elsewhere.Open(ctx, vm.ID())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Status().Checkpoint != selected || reopened.Epoch() != vm.Epoch()+1 {
		t.Fatalf("takeover opened %+v at epoch %d", reopened.Status(), reopened.Epoch())
	}
	check("reopened-parent", reopened, want)
	if err := vm.Volume("disk").Write(ctx, 0, []byte("stale")); err != nil {
		t.Fatalf("the fenced handle refused a local write: %v", err)
	}
	if err := vm.Checkpoint(ctx); !errors.Is(err, control.ErrFenced) {
		t.Fatalf("old writer: %v", err)
	}
	scheduler.Record("old-writer", "fenced", nil)
	// Every sequence the takeover allocates belongs to its own epoch, so it can
	// never collide with one the fenced handle was uploading.
	if got := control.EpochOf(reopened.Status().Checkpoint.Sequence); got != vm.Epoch() {
		t.Fatalf("the selected checkpoint belongs to epoch %d, want %d", got, vm.Epoch())
	}
	write(reopened, want, "ram", 2*checkpoint.SectorSize, 219)
	if err := reopened.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if got := control.EpochOf(reopened.Status().Checkpoint.Sequence); got != reopened.Epoch() {
		t.Fatalf("the takeover published under epoch %d, want its own %d", got, reopened.Epoch())
	}
	scheduler.Record("takeover/sequence", "epoch-major", nil)
	reopenedFork, err := elsewhere.Open(ctx, fork.ID())
	if err != nil {
		t.Fatal(err)
	}
	check("reopened-fork", reopenedFork, forkModel)

	// Cancellation is observed by a scheduled operation and must not make
	// already published data inaccessible to the next writer.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := reopened.Volume("disk").Write(canceled, 0, []byte("canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write: %v", err)
	}
	check("after-cancellation", reopened, want)
	scheduler.Record("volume/end", "checked", nil)
}

// scheduledHandoverWorkload moves one guest from host to host through every way
// a handover can fail: a healthy one, one taken over a fresh checkpoint, a
// destination that cannot read the control record, a destination that cannot
// start the guest, and a source whose frames are held until whatever asked for
// them gives up. A layout that would truncate a memory region or map beyond its volume
// is refused before anything starts a guest.
//
// What the destination reads back is checked by the world itself at every hop:
// nothing was published at the handoff, so its first read is the source's last
// checkpoint plus the pages it serves, and the VMM state it restored carries
// the guest's own counters.
func scheduledHandoverWorkload(t *testing.T, ctx context.Context, world *simtest.World,
	runtime *sim.Runtime, scheduler *sim.Scheduler) {
	t.Helper()
	pages := func(limit int) int { return limit - 1 }
	faults := []string{"healthy", "layout", "interval-checkpoint", "destination-unavailable",
		"start-failed", "stalled-stream"}
	// The seed controls the order of the lifecycle failures, independently of
	// Go goroutine creation order. Every seed covers every fault.
	for i := len(faults) - 1; i > 0; i-- {
		j := runtime.Random("handover-fault-order").Intn(fmt.Sprint(i), i+1)
		faults[i], faults[j] = faults[j], faults[i]
	}
	for hop, fault := range faults {
		to := (world.HostOf(scheduledVMID) + 1) % scheduledHosts
		if err := world.Store(ctx, scheduledVMID, 2, pages); err != nil {
			t.Fatal(err)
		}
		terms := simtest.Handover{}
		var staged simtest.Fault
		switch fault {
		case "interval-checkpoint":
			// A checkpoint of the running guest right before the migration:
			// what it published is the destination's to read from storage, and
			// the stores that follow are the source's pages to serve.
			if err := world.Checkpoint(ctx, scheduledVMID); err != nil {
				t.Fatalf("the interval checkpoint: %v", err)
			}
			if err := world.Store(ctx, scheduledVMID, 1, pages); err != nil {
				t.Fatal(err)
			}
		case "layout":
			terms.Inspect = func(handoff vmmigrate.Handoff) error {
				return refuseLayouts(t, ctx, world, scheduler, hop, to, handoff)
			}
		case "destination-unavailable":
			staged = simtest.HostLosesStore(to)
		case "start-failed":
			staged = simtest.RefusedStart(to)
		case "stalled-stream":
			staged = simtest.StalledStream(to)
		}
		if staged != nil {
			if err := staged.Begin(ctx, world); err != nil {
				t.Fatal(err)
			}
		}
		if err := world.MigrateWith(ctx, scheduledVMID, to, terms); err != nil {
			t.Fatalf("%s: %v", fault, err)
		}
		if staged != nil {
			if err := staged.End(ctx, world); err != nil {
				t.Fatal(err)
			}
			if err := world.Settle(ctx); err != nil {
				t.Fatal(err)
			}
			if err := staged.Holds(ctx, world); err != nil {
				t.Errorf("%s did not leave the world as it found it: %v", fault, err)
			}
		}
		scheduler.Record(fmt.Sprintf("hop/%d/%s", hop, fault), "handled", nil)
		// Late post-copy pages must not undo stores the resumed guest made.
		for step := range 4 {
			if err := scheduler.Wait(ctx, fmt.Sprintf("hop/%d/resumed-store/%d", hop, step), 0, 5*time.Microsecond); err != nil {
				t.Fatal(err)
			}
			if err := world.Store(ctx, scheduledVMID, 1, pages); err != nil {
				t.Fatal(err)
			}
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("%s: %v", fault, err)
		}
		if err := world.Checkpoint(ctx, scheduledVMID); err != nil {
			t.Fatalf("%s: publishing what the destination received: %v", fault, err)
		}
		if err := world.VerifyDurable(ctx, scheduledVMID); err != nil {
			t.Fatalf("%s: %v", fault, err)
		}
		if err := world.VerifyLossWindow(ctx, scheduledVMID); err != nil {
			t.Fatalf("%s: %v", fault, err)
		}
		digest := sha256.Sum256([]byte(fmt.Sprint(world.HostOf(scheduledVMID))))
		scheduler.Record(fmt.Sprintf("hop/%d/settled", hop), "bytes-match/sha256", digest[:])
	}
}

// refuseLayouts requires a control-plane layout that would truncate a memory region or
// map beyond its volume to be refused before anything starts a guest. Neither
// request may start one, and the unchanged handoff must still be receivable
// afterwards.
func refuseLayouts(t *testing.T, ctx context.Context, world *simtest.World,
	scheduler *sim.Scheduler, hop, to int, handoff vmmigrate.Handoff) error {
	t.Helper()
	for _, delta := range []int{-simtest.PMEMPage, simtest.PMEMPage} {
		invalid := handoff
		invalid.MemoryRegions = slices.Clone(handoff.MemoryRegions)
		invalid.MemoryRegions[0].Size = uint64(int64(invalid.MemoryRegions[0].Size) + int64(delta))
		received, err := world.Host(to).Receive(ctx, invalid)
		if received != nil || !errors.Is(err, vmmigrate.ErrInvalid) {
			return fmt.Errorf("receive layout delta %d: received=%v error=%w", delta, received != nil, err)
		}
		scheduler.Record(fmt.Sprintf("hop/%d/layout/%d", hop, delta), "refused-before-start", nil)
	}
	return nil
}

// scheduledHostWorkload is what a deployment does to whole hosts: a drain that
// was cancelled before it began does no work at all, a host that is lost gives
// its VMs back to whoever opens them, and the process that replaces it owns no
// local state.
func scheduledHostWorkload(t *testing.T, ctx context.Context, world *simtest.World,
	runtime *sim.Runtime, scheduler *sim.Scheduler) {
	t.Helper()
	at := world.HostOf(scheduledVMID)
	before := len(runtime.Trace().Events())
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := world.Host(at).Drain(canceled, func(string) platform.Address {
		return world.Pages((at + 1) % scheduledHosts)
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled drain: %v", err)
	}
	if len(runtime.Trace().Events()) != before {
		t.Fatal("an already-canceled drain performed storage or transport work")
	}
	if got := world.HostOf(scheduledVMID); got != at {
		t.Fatalf("a canceled drain moved the VM to host-%d", got)
	}
	scheduler.Record("host/canceled-drain", "no-work", nil)

	// The host running the guest is taken away at a moment and comes back on
	// the disk it left behind. What the VM is worth afterwards is the
	// checkpoint its record selects, which is what the world's own model
	// requires of the takeover.
	if err := world.Kill(ctx, at, sim.PowerLoss); err != nil {
		t.Fatal(err)
	}
	if err := world.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := world.Restart(ctx, at); err != nil {
		t.Fatal(err)
	}
	if got := world.Incarnation(at); got != 2 {
		t.Fatalf("the host came back as incarnation %d, want its second", got)
	}
	if err := world.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
		t.Fatal(err)
	}
	// The VM is migrated back onto the restarted host, which is a fresh process
	// with an empty arena: what it reads is what the deployment's store holds.
	if err := world.Migrate(ctx, scheduledVMID, at); err != nil {
		t.Fatal(err)
	}
	if err := world.Checkpoint(ctx, scheduledVMID); err != nil {
		t.Fatal(err)
	}
	if err := world.VerifyDurable(ctx, scheduledVMID); err != nil {
		t.Fatal(err)
	}
	if err := world.VerifyLossWindow(ctx, scheduledVMID); err != nil {
		t.Fatal(err)
	}
	scheduler.Record("host/end", "checked", nil)
}

// TestScheduledWorldReproduces is the recording comparison: one seed, both
// caller creation orders, identical execution and adapter traces. A seed whose
// interleaving depends on the order goroutines were created in fails here.
func TestScheduledWorldReproduces(t *testing.T) {
	dir := os.Getenv("SPROUTFS_OVERLAP_TRACE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	seeds := uint64(3)
	if value := os.Getenv("SPROUTFS_OVERLAP_TRACE_SEEDS"); value != "" {
		var err error
		seeds, err = strconv.ParseUint(value, 10, 32)
		if err != nil || seeds == 0 {
			t.Fatal("SPROUTFS_OVERLAP_TRACE_SEEDS must be positive")
		}
	}
	for seed := uint64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed-%02d", seed), func(t *testing.T) {
			var recordings [2]sim.Recording
			for order := range 2 {
				recordings[order], _ = runScheduledWorld(t, seed, order == 1)
				if err := recordings[order].WriteFiles(dir, fmt.Sprintf("seed-%02d-order-%d", seed, order)); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(recordings[0].Execution, recordings[1].Execution) {
				t.Errorf("execution differs; inspect %s", dir)
			}
			if !bytes.Equal(recordings[0].Adapters, recordings[1].Adapters) {
				t.Errorf("adapter trace differs; inspect %s", dir)
			}
		})
	}
}

// TestScheduledWorldFingerprintIsStable: this scenario chooses every completion
// order itself, so unlike a campaign that does not it can promise the strict
// fingerprint — the same operations on the same resources, in the same order on
// each of them, at the same simulated moments, with the same adapter
// numbering. The recording comparison proves this across processes for reversed
// creation order; this proves it for two ordinary runs of one seed, which is
// the check a soak can afford to make on every seed it visits.
func TestScheduledWorldFingerprintIsStable(t *testing.T) {
	for seed := uint64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprintf("seed-%02d", seed), func(t *testing.T) {
			_, one := runScheduledWorld(t, seed, false)
			_, two := runScheduledWorld(t, seed, false)
			first, second := one.Fingerprint(), two.Fingerprint()
			t.Logf("seed=%d fingerprint=%#016x probes=%v", seed, first, one.Probes())
			if first != second {
				t.Fatalf("seed %d produced fingerprints %#016x and %#016x", seed, first, second)
			}
		})
	}
}

// TestScheduledWorldSoak runs the scenario over a block of the seed range
// rather than the three seeds the ordinary suite can afford. Every seed still
// runs both caller creation orders and requires identical execution and adapter
// traces, so the sweep is a determinism campaign and not only a longer
// workload.
func TestScheduledWorldSoak(t *testing.T) {
	for seed := range testsoak.Require(t, 50).Seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			var recordings [2]sim.Recording
			for order := range 2 {
				recordings[order], _ = runScheduledWorld(t, seed, order == 1)
			}
			if !bytes.Equal(recordings[0].Execution, recordings[1].Execution) {
				t.Errorf("seed %d: execution differs across creation order", seed)
			}
			if !bytes.Equal(recordings[0].Adapters, recordings[1].Adapters) {
				t.Errorf("seed %d: adapter trace differs across creation order", seed)
			}
		})
	}
}
