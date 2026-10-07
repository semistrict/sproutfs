package simtest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// journalInterval is the checkpoint loop of a world with durable flush on. A
// flush is journaled only once a checkpoint names the journal, which the loop
// takes out of turn when a flush asks; its own turns are an hour apart, which
// no clock here reaches.
const journalInterval = time.Hour

// journalWorld is hosts with durable flush on, each writing the journal disk
// the controller keeps for its machine, and one VM on the first with its
// memory and a disk of four pages.
func journalWorld(t *testing.T, ctx context.Context, runtime *sim.Runtime, prefix string, hosts int) *simtest.World {
	t.Helper()
	topology := simtest.Topology{VMs: []simtest.VMSpec{{ID: "vm-0", Host: 0, Volumes: []volume.VolumeSpec{
		{Name: simtest.MemoryVolume, Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
		{Name: simtest.DiskVolume, Size: 4 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}}}}
	for at := range hosts {
		topology.Hosts = append(topology.Hosts, fmt.Sprintf("host-%d", at))
	}
	k := campaignKnobs(t, runtime, topology)
	// The loss window stops a guest's stores; nothing here waits for one.
	k.LossWindow = 0
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology, Knobs: k,
		Prefix: newPrefix(t, prefix), Log: t.Logf, Journals: true, CheckpointInterval: journalInterval})
	for index := range hosts {
		if !world.JournalServed(index) {
			t.Fatalf("host-%d serves no journal once the world has started", index)
		}
	}
	return world
}

// readsPage fails unless page of the VM's disk reads value, every byte of it.
func readsPage(t *testing.T, ctx context.Context, world *simtest.World, page uint64, value byte) {
	t.Helper()
	got, err := world.ReadPage(ctx, "vm-0", simtest.DiskVolume, page)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Repeat([]byte{value}, len(got)); !bytes.Equal(got, want) {
		t.Fatalf("disk page %d reads %d, want the %d the guest stored before its flush", page, got[0], value)
	}
}

// A guest stores, flushes and is answered, and its host dies at once: the
// store view, the guests and the machine go, and the journal disk keeps only
// what it synced. A survivor is given the dead host's journal disk to read,
// and the VM opened there replays it: every page the guest stored before the
// flush reads what it stored.
func TestAFlushAnsweredJustBeforeItsHostDiesIsThereWhereTheVMOpensNext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(31, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "flush-then-die/", 2)
		if err := world.StorePages("vm-0", simtest.DiskVolume, []uint64{0, 1}, 7); err != nil {
			t.Fatal(err)
		}
		if err := <-world.Flush("vm-0", simtest.DiskVolume); err != nil {
			t.Fatalf("the flush failed: %v", err)
		}
		if err := world.Kill(ctx, 0, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if got := world.HostOf("vm-0"); got != 1 {
			t.Fatalf("the VM runs on host-%d after its host died, want host-1", got)
		}
		if got := world.Takeovers(); got != 1 {
			t.Fatalf("the VM was taken over %d times, want once", got)
		}
		readsPage(t, ctx, world, 0, 7)
		readsPage(t, ctx, world, 1, 7)
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// The host dies after the journal synced a flush's batch and before the
// guest took the answer. The flush promised nothing, but its blocks are on
// the journal disk, so the VM opened elsewhere reads them.
func TestAFlushSyncedAndNotYetAnsweredWhenItsHostDiesIsReplayed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(32, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "synced-then-die/", 2)
		if err := world.StorePages("vm-0", simtest.DiskVolume, []uint64{0, 1}, 7); err != nil {
			t.Fatal(err)
		}
		held := world.HoldFlush("vm-0", simtest.DiskVolume)
		if err := <-held.Answered(); err != nil {
			t.Fatalf("the host answered the flush with %v", err)
		}
		if err := world.Kill(ctx, 0, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if held.Deliver() {
			t.Fatal("the guest of a dead host took its flush's answer")
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if got := world.HostOf("vm-0"); got != 1 {
			t.Fatalf("the VM runs on host-%d after its host died, want host-1", got)
		}
		if answered, failed := world.Flushes("vm-0"); answered != 0 || failed != 0 {
			t.Fatalf("the guest took %d answers and %d failures, want none", answered, failed)
		}
		readsPage(t, ctx, world, 0, 7)
		readsPage(t, ctx, world, 1, 7)
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A checkpoint is uploading when the guest stores into a page it sealed and
// flushes, and the host dies before the upload lands. The flush took the
// guest's copy of the page and the sealed copy of the page it had not stored
// into again, so the VM opened elsewhere reads both, whatever became of the
// checkpoint.
func TestAFlushDuringASealsUploadSurvivesItsHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(33, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "flush-during-upload/", 2)
		if err := world.StorePages("vm-0", simtest.DiskVolume, []uint64{0, 1}, 5); err != nil {
			t.Fatal(err)
		}
		// The checkpoint's first write to the store gets no reply, so the
		// guest runs on behind a seal whose upload is in flight.
		runtime.ObjectStore().HangNext(sim.ObjectPut, 1)
		checkpointed := make(chan error, 1)
		go func() { checkpointed <- world.Checkpoint(ctx, "vm-0") }()
		synctest.Wait()
		if err := world.StorePages("vm-0", simtest.DiskVolume, []uint64{0}, 6); err != nil {
			t.Fatal(err)
		}
		if err := <-world.Flush("vm-0", simtest.DiskVolume); err != nil {
			t.Fatalf("the flush during the upload failed: %v", err)
		}
		select {
		case err := <-checkpointed:
			t.Fatalf("the checkpoint ended before its host died: %v", err)
		default:
		}
		if err := world.Kill(ctx, 0, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if got := world.HostOf("vm-0"); got != 1 {
			t.Fatalf("the VM runs on host-%d after its host died, want host-1", got)
		}
		readsPage(t, ctx, world, 0, 6)
		readsPage(t, ctx, world, 1, 5)
		if err := <-checkpointed; err == nil {
			t.Fatal("the checkpoint of a host that died landed")
		}
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A migration's source dies while its destination is still fetching the
// pages no checkpoint holds. The migration ends, and the VM opens on a
// survivor once the source's journal disk is served there: what the guest
// flushed on the source reads back.
func TestAFlushSurvivesItsSourceDyingDuringThePostCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(34, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "source-dies/", 3)
		if err := world.StorePages("vm-0", simtest.DiskVolume, []uint64{0}, 5); err != nil {
			t.Fatal(err)
		}
		if err := <-world.Flush("vm-0", simtest.DiskVolume); err != nil {
			t.Fatal(err)
		}
		// A store after the flush, which the post-copy has to fetch.
		if err := world.StorePages("vm-0", simtest.DiskVolume, []uint64{1}, 6); err != nil {
			t.Fatal(err)
		}
		stall := simtest.StalledStream(1)
		if err := stall.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		migrated := make(chan error, 1)
		go func() { migrated <- world.Migrate(ctx, "vm-0", 1) }()
		waitFor(t, func() bool {
			return slices.Contains(world.Host(0).Status().Serving, "vm-0")
		}, "the migration never reached its post-copy")
		if err := world.Kill(ctx, 0, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if err := stall.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := <-migrated; err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if got := world.HostOf("vm-0"); got != 1 {
			t.Fatalf("the VM runs on host-%d after its source died, want host-1, which the migration was for", got)
		}
		readsPage(t, ctx, world, 0, 5)
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A migration's destination answers no flush before its post-copy ends: the
// pages its guest stored into on the source since their last flush are in no
// journal until they arrive. Here they never do, since the source dies. The
// destination's flush goes unanswered, and the VM opens elsewhere at what the
// source flushed.
func TestADestinationAnswersNoFlushBeforeItsPostCopyEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(36, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "answer-before-post-copy/", 3)
		flushed(t, world, []uint64{0}, 4)
		// Only the source's memory holds this store.
		if err := world.StorePages("vm-0", simtest.DiskVolume, []uint64{0}, 5); err != nil {
			t.Fatal(err)
		}
		stall := simtest.StalledStream(1)
		if err := stall.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		guests := world.ReceivedGuests("vm-0")
		moved := make(chan error, 1)
		go func() { moved <- world.Migrate(ctx, "vm-0", 1) }()
		waitFor(t, func() bool { return world.ReceivedGuests("vm-0") > guests },
			"the destination never started the VM it was taking in")
		answer := world.FlushOn(1, "vm-0", simtest.DiskVolume)
		synctest.Wait()
		select {
		case err := <-answer:
			t.Fatalf("the destination answered a flush during its post-copy: %v", err)
		default:
		}
		if err := world.Kill(ctx, 0, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if err := stall.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := <-moved; err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-answer:
			t.Fatalf("the destination answered a flush of a VM it gave up: %v", err)
		default:
		}
		if got := world.HostOf("vm-0"); got != 1 {
			t.Fatalf("the VM runs on host-%d after its source died, want host-1", got)
		}
		readsPage(t, ctx, world, 0, 4)
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A VMM sends the flushes it held when it stopped again as soon as it runs on
// its next host, which may be before that host has registered it. Such a flush
// is a destination's like any other: it waits for the post-copy, and here goes
// unanswered, since the source dies and the destination gives the VM up.
func TestAFlushSentAsTheDestinationStartsWaitsForThePostCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(39, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "flush-at-start/", 3)
		flushed(t, world, []uint64{0}, 4)
		// Only the source's memory holds this store.
		if err := world.StorePages("vm-0", simtest.DiskVolume, []uint64{0}, 5); err != nil {
			t.Fatal(err)
		}
		answer := world.FlushAtStart(1, "vm-0", simtest.DiskVolume)
		stall := simtest.StalledStream(1)
		if err := stall.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		guests := world.ReceivedGuests("vm-0")
		moved := make(chan error, 1)
		go func() { moved <- world.Migrate(ctx, "vm-0", 1) }()
		waitFor(t, func() bool { return world.ReceivedGuests("vm-0") > guests },
			"the destination never started the VM it was taking in")
		synctest.Wait()
		select {
		case err := <-answer:
			t.Fatalf("the destination answered a flush sent as its guest started, during its post-copy: %v", err)
		default:
		}
		if err := world.Kill(ctx, 0, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if err := stall.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := <-moved; err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-answer:
			t.Fatalf("the destination answered a flush of a VM it gave up: %v", err)
		default:
		}
		readsPage(t, ctx, world, 0, 4)
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A host is cut off from the others with the VM running on it. Another host
// cannot read its journal, so the takeover waits. An operator gives the cut
// off host up: its member is drained and its journal disk detached from its
// machine while its process runs on. The disk is read on the other host, the
// VM opens there with what was flushed, and the guest the cut off host still
// runs can flush nothing more.
func TestAFlushSurvivesItsHostCutOffAndGivenUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(37, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "cut-off/", 2)
		flushed(t, world, []uint64{0, 1}, 4)
		isolated := simtest.IsolatedHost(0)
		if err := isolated.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := world.Takeover(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if got := world.Takeovers(); got != 0 {
			t.Fatalf("the VM was taken over %d times while its journal's holder was cut off, want none", got)
		}
		world.GiveUpOn(0)
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if got := world.Takeovers(); got != 1 {
			t.Fatalf("the VM was taken over %d times once its host was given up, want once", got)
		}
		if got := world.HostOf("vm-0"); got != 1 {
			t.Fatalf("the VM runs on host-%d, want host-1", got)
		}
		readsPage(t, ctx, world, 0, 4)
		readsPage(t, ctx, world, 1, 4)
		stale := world.FlushOn(0, "vm-0", simtest.DiskVolume)
		synctest.Wait()
		if err := <-stale; !errors.Is(err, host.ErrJournalUnavailable) {
			t.Fatalf("the cut off host answered a flush after its journal disk was detached with %v, "+
				"want ErrJournalUnavailable", err)
		}
		if err := isolated.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// membershipOf is the membership the world's store holds.
func membershipOf(t *testing.T, ctx context.Context, world *simtest.World) membership.Membership {
	t.Helper()
	store, err := membership.NewStore(membership.Config{ObjectStore: world.Runtime().ObjectStore(),
		ObjectPrefix: world.Prefix()})
	if err != nil {
		t.Fatal(err)
	}
	// The store's read-fails site may be on: read again.
	var errs []error
	for range 16 {
		m, err := store.Read(ctx)
		if err == nil {
			return m
		}
		errs = append(errs, err)
	}
	t.Fatalf("the membership could not be read: %v", errors.Join(errs...))
	return membership.Membership{}
}

// journalOf is the journal disk the membership keeps for machine.
func journalOf(t *testing.T, ctx context.Context, world *simtest.World, machine string) membership.Disk {
	t.Helper()
	for _, disk := range membershipOf(t, ctx, world).Disks() {
		if disk.Kind == membership.Journal && disk.Machine == machine {
			return disk
		}
	}
	t.Fatalf("no journal disk is kept for %s", machine)
	return membership.Disk{}
}

// A host scales down: it moves its VM away and keeps its journal disk until
// no record names it, which it learns on its trim's turn. It dies during
// that wait, and a survivor reads its disk. The VM's next host dies too,
// and the VM opens on the last host with everything either flushed. The
// survivor lets the disk go once the records it reads name it no more.
func TestAFlushSurvivesItsHostDyingDuringAScaleDownsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(38, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "scale-down/", 3)
		flushed(t, world, []uint64{0}, 4)
		draining := journalOf(t, ctx, world, "host-0")
		world.Unlist(ctx, 0)
		migrated(t, ctx, world, 1)
		if disk, _ := membershipOf(t, ctx, world).Disk(draining.ID); disk.State != membership.Releasing ||
			disk.Empty || !world.JournalServed(0) {
			t.Fatalf("the draining host's journal disk is %+v, served %t, want releasing and held open with its entry",
				disk, world.JournalServed(0))
		}
		if err := world.Kill(ctx, 0, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		flushed(t, world, []uint64{1}, 6)
		if err := world.Kill(ctx, 1, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if got := world.HostOf("vm-0"); got != 2 {
			t.Fatalf("the VM runs on host-%d, want host-2, the last one up", got)
		}
		readsPage(t, ctx, world, 0, 4)
		readsPage(t, ctx, world, 1, 6)
		// The survivor reads the records of what it holds on its trim's turn,
		// and the controller lets the disks go once they hold nothing.
		world.Advance(2 * host.DefaultJournalTrimInterval)
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if disk, _ := membershipOf(t, ctx, world).Disk(draining.ID); disk.State != membership.Released ||
			!disk.Empty || disk.Machine != "" {
			t.Fatalf("the dead draining host's journal disk is %+v once nothing names it, want released, empty and free",
				disk)
		}
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// flushed has the VM's guest store value into each of pages of its disk and
// flush the disk, and fails unless the flush succeeds.
func flushed(t *testing.T, world *simtest.World, pages []uint64, value byte) {
	t.Helper()
	if err := world.StorePages("vm-0", simtest.DiskVolume, pages, value); err != nil {
		t.Fatal(err)
	}
	if err := <-world.Flush("vm-0", simtest.DiskVolume); err != nil {
		t.Fatalf("flushing pages %v at %d: %v", pages, value, err)
	}
}

// migrated moves the VM to host to and takes a checkpoint there, which names
// that host's journal alone: its post-copy is over.
func migrated(t *testing.T, ctx context.Context, world *simtest.World, to int) {
	t.Helper()
	if err := world.Migrate(ctx, "vm-0", to); err != nil {
		t.Fatal(err)
	}
	if got := world.HostOf("vm-0"); got != to {
		t.Fatalf("the VM runs on host-%d after its migration, want host-%d", got, to)
	}
	if err := world.Checkpoint(ctx, "vm-0"); err != nil {
		t.Fatal(err)
	}
}

// A VM leaves a host with a flush in its journal and comes back to it. The
// destination dies during that post-copy, so the record names the journal
// of the host before and the destination's at the epoch it opened at. The
// VM opens on a survivor, which reads both: what the guest flushed on the
// host before reads back, and the entry the destination's journal still
// holds of the VM's earlier stay there, which a later checkpoint covers, is
// not replayed over it.
func TestAFlushSurvivesItsDestinationDyingDuringThePostCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(35, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := journalWorld(t, ctx, runtime, "destination-dies/", 3)
		migrated(t, ctx, world, 1)
		// An entry of host-1's journal at the epoch of this stay, which no
		// record names once the VM has left.
		flushed(t, world, []uint64{0}, 4)
		migrated(t, ctx, world, 0)
		flushed(t, world, []uint64{0}, 5)
		stall := simtest.StalledStream(1)
		if err := stall.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		guests := world.ReceivedGuests("vm-0")
		moved := make(chan error, 1)
		go func() { moved <- world.Migrate(ctx, "vm-0", 1) }()
		waitFor(t, func() bool { return world.ReceivedGuests("vm-0") > guests },
			"the destination never started the VM it was taking in")
		if err := world.Kill(ctx, 1, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}
		if err := stall.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := <-moved; err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if got := world.HostOf("vm-0"); got != 0 {
			t.Fatalf("the VM runs on host-%d after its destination died, want host-0, its source", got)
		}
		readsPage(t, ctx, world, 0, 5)
		if err := world.VerifyFlushes(); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
