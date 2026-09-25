package simtest_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// swizzleCampaignName is what this campaign's per-seed records are filed under
// in a sweep's summary.
const swizzleCampaignName = "swizzle"

// swizzleWindow is how long the campaign keeps blocking and healing links. It
// is virtual time: the bubble runs it in microseconds of real time.
const swizzleWindow = 20 * time.Second

const (
	// swizzleVMID is the VM two writers hold across a takeover, and
	// swizzleGuestID the one whose guest holds pages no checkpoint has, so that
	// its handoff has to pull them over the page-server link the swizzle is
	// taking away.
	swizzleVMID    = "vm-1"
	swizzleGuestID = "vm-2"
)

// TestTwoWritersOfOneVMNeverMixAcrossASwizzle: two hosts hold one VM across a
// takeover while every link among them and the object store is separated at its
// own seeded moment and healed in a different order, and the page-server links
// drop, duplicate, delay and slow what they carry. That is exactly the shape of
// the failure the whole design exists to refuse: the writer that lost the epoch
// cannot tell it has, and the one that took it cannot always reach the store.
//
// Whatever the swizzle does, the requirements are the same: the handle the
// takeover fenced stays fenced and never publishes, the VM's state is built
// only out of checkpoints a writer holding the epoch published, every page the
// source held reaches the destination, and the bytes a fresh reader sees are
// the surviving writer's own.
func TestTwoWritersOfOneVMNeverMixAcrossASwizzle(t *testing.T) {
	seeds := uint64(16)
	if value := os.Getenv("SPROUTFS_SWIZZLE_SEEDS"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil || parsed == 0 {
			t.Fatal("SPROUTFS_SWIZZLE_SEEDS must be positive")
		}
		seeds = parsed
	}
	for seed := uint64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed-%02d", seed), func(t *testing.T) {
			testsoak.Measure(t, swizzleCampaignName, seed, func(t *testing.T) *sim.Runtime {
				return runSwizzleCampaign(t, seed)
			})
		})
	}
}

// newSwizzleRuntime is the world this campaign swizzles: a network whose
// latency is a millisecond, so a link that is blocked and healed at seeded
// moments inside a twenty-second window carries something in between, over a
// store that answers in microseconds.
func newSwizzleRuntime(seed uint64) *sim.Runtime {
	return sim.New(sim.Config{Seed: seed,
		Network: sim.NetworkConfig{Latency: time.Millisecond, Jitter: 100 * time.Microsecond,
			ConnectLatency: time.Millisecond},
		ObjectStore: sim.ObjectStoreConfig{GetLatency: time.Microsecond, PutLatency: time.Microsecond,
			ListLatency: time.Microsecond, BytesPerSecond: 1 << 40}})
}

func runSwizzleCampaign(t *testing.T, seed uint64) *sim.Runtime {
	t.Helper()
	runtime := newSwizzleRuntime(seed)
	prefix := newPrefix(t, "swizzle/")
	ctx := sim.WithRuntime(t.Context(), runtime)
	topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{
			{ID: swizzleVMID, Host: 0, Volumes: []volume.VolumeSpec{{Name: "ram0", Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage}}},
			{ID: swizzleGuestID, Host: 0, Volumes: []volume.VolumeSpec{{Name: "ram0", Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage}}}}}
	world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: campaignKnobs(t, runtime, topology), Prefix: prefix, Log: t.Logf})
	pages := func(limit int) int { return limit - 1 }
	// Four rounds of the healthy writer, each published, so the VM has a
	// history the surviving writer's state has to be built out of.
	for round := range 4 {
		if err := world.Store(ctx, swizzleVMID, 2, pages); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, swizzleVMID); err != nil {
			t.Fatalf("the healthy writer could not publish round %d: %v", round, err)
		}
	}
	// The second VM's guest writes pages that are durable nowhere: only the
	// pages this host holds have them, so only a handoff that pulled every one
	// of them preserves the guest.
	if err := world.Checkpoint(ctx, swizzleGuestID); err != nil {
		t.Fatal(err)
	}
	if err := world.Store(ctx, swizzleGuestID, 4, pages); err != nil {
		t.Fatal(err)
	}
	// The handle the takeover is about to fence, kept so that what it does
	// afterwards can be required of it.
	fenced := world.VM(swizzleVMID)
	if fenced == nil {
		t.Fatal("the first writer holds no handle on the VM")
	}
	firstEpoch := fenced.Epoch()

	// Everything now goes dark and comes back in a different order: each of the
	// links among the two hosts, their page servers and the store is blocked at
	// its own moment inside the first half of the window and healed at its own
	// moment inside the second. The page-server links get the rest of the kit
	// as well, so a handoff that does run during the window runs over a link
	// that drops, duplicates, delays and slows what it carries.
	addrs := []platform.Address{world.Address(0), world.Address(1), simtest.StoreAddress}
	runtime.Network().Swizzle(addrs, swizzleWindow, runtime.Random("simtest/swizzle"))
	dropped := simtest.DroppedPageServerFrames(1)
	if err := dropped.Begin(ctx, world); err != nil {
		t.Fatal(err)
	}

	// The handoff runs inside the window, over links that are separated,
	// healed, dropping, duplicating and delaying. The receive is retried for
	// twice the window — the source keeps the pages the destination has not
	// pulled until it is told the destination has them all, so every retry is
	// of the same handoff — and it has to succeed before those retries run
	// out, because a link the swizzle separated is a link that heals inside
	// the window. The retrying is this campaign's: a deployment's drain tries
	// the receive once, which TASK-14 in backlog/tasks records.
	took := world.Takeovers()
	if err := world.MigrateWith(ctx, swizzleGuestID, 1,
		simtest.Handover{Attempts: 64, Pause: swizzleWindow / 32}); err != nil {
		t.Fatalf("the handoff reported %v", err)
	}
	if at := world.HostOf(swizzleGuestID); at != 1 {
		t.Fatalf("the guest is on host %d after the handoff, not host-1", at)
	}
	// A handoff that gave up and let the VM be opened again lost the pages no
	// checkpoint held, which is not what a retried handoff owes its guest.
	if world.Takeovers() != took {
		t.Fatal("the guest was taken over rather than handed over")
	}
	if err := dropped.End(ctx, world); err != nil {
		t.Fatal(err)
	}

	// The takeover itself happens inside the window, so the host that takes the
	// epoch may have to wait for its own link to the store to come back.
	deadline := time.Now().Add(swizzleWindow)
	for world.HostOf(swizzleVMID) != 1 && time.Now().Before(deadline) {
		if err := world.Takeover(ctx, swizzleVMID, 1); err != nil {
			t.Fatal(err)
		}
		if world.HostOf(swizzleVMID) != 1 {
			time.Sleep(swizzleWindow / 32)
		}
	}
	if world.HostOf(swizzleVMID) != 1 {
		t.Fatal("the takeover never reached the control record")
	}

	// Both writers now go on writing and checkpointing for the rest of the
	// window. The fenced one behaves exactly as a live but superseded host
	// does: it does not know, so it keeps trying. What it must never do is
	// publish.
	deadline = time.Now().Add(swizzleWindow)
	for round := 4; time.Now().Before(deadline); round++ {
		_ = fenced.Volume("ram0").Write(ctx, 0, []byte{byte(round)})
		if err := fenced.Checkpoint(ctx); err == nil {
			t.Fatalf("round %d: the fenced writer published %s", round, fenced.Status().Checkpoint)
		}
		if err := world.Store(ctx, swizzleVMID, 1, pages); err != nil {
			t.Fatal(err)
		}
		_ = world.Checkpoint(ctx, swizzleVMID)
		// A round costs virtual time whatever the links did, so the window ends
		// whether or not anything succeeded.
		time.Sleep(swizzleWindow / 16)
	}

	// Every link is carrying traffic again. The surviving writer rewrites every
	// page and retries until its checkpoint lands, so the state under test is
	// its own with nothing of it left unpublished.
	landed := false
	for range 16 {
		if err := world.Store(ctx, swizzleVMID, 4, pages); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, swizzleVMID); err == nil {
			landed = true
			break
		}
	}
	if !landed {
		t.Fatal("the surviving writer never published its last checkpoint")
	}

	// The fenced handle stays fenced: it cannot publish, and it says so.
	if err := fenced.Checkpoint(ctx); err == nil {
		t.Fatalf("the fenced writer published %s after every link healed", fenced.Status().Checkpoint)
	}
	if status := fenced.Status(); status.Err == nil {
		t.Fatalf("the fenced writer's handle reports no error: %+v", status)
	}
	survivor := world.VM(swizzleVMID)
	if survivor == nil || survivor.Epoch() <= firstEpoch {
		t.Fatalf("the takeover did not advance the epoch past %d", firstEpoch)
	}
	record, err := world.Host(1).Control().Read(ctx, swizzleVMID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Epoch != survivor.Epoch() || control.EpochOf(record.Selected) != survivor.Epoch() {
		t.Fatalf("the control record is epoch %d selecting %d, want the surviving writer's epoch %d",
			record.Epoch, record.Selected, survivor.Epoch())
	}
	// The VM's state is built only out of checkpoints a writer holding the
	// epoch published, and nothing past that epoch.
	published := world.Published(swizzleVMID)
	index, err := world.Host(1).Checkpoints().Open(ctx, control.Ref{VM: swizzleVMID, Sequence: record.Selected})
	if err != nil {
		t.Fatal(err)
	}
	for _, named := range index.Checkpoints() {
		if !published[named.Sequence] {
			t.Fatalf("the VM's state reads %s, which no checkpoint of a writer holding the epoch published", named)
		}
		if control.EpochOf(named.Sequence) > survivor.Epoch() {
			t.Fatalf("the VM's state reads %s, which is past the epoch that wrote it", named)
		}
	}

	// A fresh reader sees the surviving writer's own bytes, through a guest's
	// mappings and through the volume both, for the VM that was taken over and
	// for the guest that was handed across.
	if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
		t.Error(err)
	}
	for _, id := range []string{swizzleVMID, swizzleGuestID} {
		if err := world.Checkpoint(ctx, id); err != nil {
			t.Fatalf("%s could not be published once every link healed: %v", id, err)
		}
		if err := world.VerifyDurable(ctx, id); err != nil {
			t.Error(err)
		}
		if err := world.VerifyLossWindow(ctx, id); err != nil {
			t.Error(err)
		}
	}
	if err := world.CheckSelected(ctx); err != nil {
		t.Error(err)
	}
	if err := world.Close(ctx); err != nil {
		t.Error(err)
	}
	// The allowances are a collector's: the checkpoints the takeover left
	// behind, and the parts of a publication that never reached its index.
	if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
		volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex,
		volume.AllowUnreferencedCheckpoint); err != nil {
		t.Errorf("seed=%d: %v", seed, err)
	}
	if t.Failed() {
		reportTrace(t, runtime)
	}
	return runtime
}
