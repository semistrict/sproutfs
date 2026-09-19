package host_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/volume"
)

// fenceVolumes is the VM the fencing invariant runs on: four pages, so each
// round writes one page and every page of the state names the writer that wrote
// it.
var fenceVolumes = []volume.VolumeSpec{{Name: "root", Size: 4 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB}}

// fenceMarkerSize is the marker each round writes at the head of one page. A
// page is published whole, so the marker is what the whole page is identified
// by.
const fenceMarkerSize = 64

func marker(writer string, round int) []byte {
	text := make([]byte, fenceMarkerSize)
	copy(text, fmt.Sprintf("%s-round-%d", writer, round))
	return text
}

// writeMarker writes one round's marker to the page that round owns. A handle a
// later writer has fenced refuses the write, which is the only failure this
// accepts: the fenced writer goes on trying exactly as a live but superseded
// host does.
func writeMarker(t *testing.T, ctx context.Context, vm *volume.VM, writer string, round int) {
	t.Helper()
	page := uint64(round % 4)
	err := vm.Volume("root").Write(ctx, page*checkpoint.PageSize2MiB, marker(writer, round))
	if err != nil && !errors.Is(err, volume.ErrNeedsRecovery) {
		t.Fatalf("%s writing round %d: %v", writer, round, err)
	}
}

// markerAt reports the marker one page of a VM reads, which says which writer's
// round that page came from.
func markerAt(t *testing.T, ctx context.Context, vm *volume.VM, page uint64) string {
	t.Helper()
	buffer := make([]byte, fenceMarkerSize)
	if err := vm.Volume("root").Read(ctx, page*checkpoint.PageSize2MiB, buffer); err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimRight(buffer, "\x00"))
}

// TestNoTwoWritersOfOneVMEverMix is the durability requirement itself: two hosts
// open one VM, the second takes the epoch, and both go on writing and
// checkpointing through injected store failures. Nothing the fenced writer
// produces after the takeover may become part of the VM's state — not a page of
// it, and not an object any selected index names.
func TestNoTwoWritersOfOneVMEverMix(t *testing.T) {
	h := newSizedHostHarness(t, 2)
	h.start(t)
	ctx := t.Context()
	first, err := h.hosts[0].Volumes().Create(ctx, "vm-1", fenceVolumes)
	if err != nil {
		t.Fatal(err)
	}
	// published is every sequence that reached the control record, which is the
	// whole of what the VM's state may be built from.
	published := map[uint64]bool{}
	for round := range 4 {
		writeMarker(t, ctx, first, "first", round)
		if err := first.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		published[first.Status().Checkpoint.Sequence] = true
	}

	second, err := h.hosts[1].Volumes().Open(ctx, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	store := h.runtime.ObjectStore()
	random := rand.New(rand.NewPCG(7, 11))
	for round := 4; round < 24; round++ {
		if random.IntN(4) == 0 {
			store.FailNext(sim.ObjectPut, 1+random.IntN(2))
		}
		if random.IntN(4) == 0 {
			store.FailNext(sim.ObjectGet, 1)
		}
		writeMarker(t, ctx, first, "fenced", round)
		if err := first.Checkpoint(ctx); err == nil {
			t.Fatalf("round %d: the fenced writer published %s", round, first.Status().Checkpoint)
		}
		writeMarker(t, ctx, second, "second", round)
		if err := second.Checkpoint(ctx); err != nil {
			// An injected store failure. The writes stay in the overlay and the
			// next round's checkpoint publishes them.
			continue
		}
		published[second.Status().Checkpoint.Sequence] = true
	}
	// The last checkpoint is retried until it lands, so the state under test is
	// the surviving writer's own and nothing it wrote is left unpublished.
	writeMarker(t, ctx, second, "second", 24)
	landed := false
	for range 8 {
		if err := second.Checkpoint(ctx); err == nil {
			landed = true
			break
		}
	}
	if !landed {
		t.Fatalf("the surviving writer never published its last checkpoint: %+v", second.Status())
	}
	published[second.Status().Checkpoint.Sequence] = true
	if status := first.Status(); !errors.Is(status.Err, volume.ErrNeedsRecovery) {
		t.Fatalf("the fenced writer's handle is %+v, want one that must be reopened", status)
	}

	record, err := h.hosts[0].Control().Read(ctx, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Epoch != second.Epoch() || control.EpochOf(record.Selected) != second.Epoch() {
		t.Fatalf("the control record is epoch %d selecting %d, want the surviving writer's epoch %d",
			record.Epoch, record.Selected, second.Epoch())
	}
	index, err := h.hosts[0].Checkpoints().Open(ctx, control.Ref{VM: "vm-1", Sequence: record.Selected})
	if err != nil {
		t.Fatal(err)
	}
	for _, named := range index.Checkpoints() {
		if !published[named.Sequence] {
			t.Fatalf("the VM's state reads %s, which no selected checkpoint ever published", named)
		}
	}
	// Reopening reads the state as any host would after both writers are gone.
	third, err := h.hosts[0].Volumes().Open(ctx, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(4) {
		if got := markerAt(t, ctx, third, page); !strings.HasPrefix(got, "second-round-") {
			t.Fatalf("page %d of the VM's state is %q, want a page of the writer that holds the epoch",
				page, got)
		}
	}
	if err := third.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(ctx); err != nil && !errors.Is(err, volume.ErrNeedsRecovery) {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestAFencedHostLearnsItsEpochMovedWithoutACheckpoint: a host finds out it has
// been taken over within seconds of the takeover, whatever its checkpoint
// interval is. The checkpoint is not the only place a host may learn this: a
// recovery that fences a host whose guest is still running leaves that guest
// writing into pages nothing can ever publish until the host notices.
func TestAFencedHostLearnsItsEpochMovedWithoutACheckpoint(t *testing.T) {
	h := newHostHarness(t)
	// No interval checkpoint at all: the loop is what a host learns through
	// today, and this is the VM that never reaches one. What it learns through
	// instead is the host's own epoch timer, at its production default.
	h.configs[0].CheckpointInterval = -1
	closed := make(chan string, 1)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	h.start(t)
	pager, arena := newPager(t, h.configs[0].Resources)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pager, arena, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 3)
	// The VM is openable elsewhere only once a checkpoint of it is published.
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	fenced := &closingMachine{machine: guest}
	if err := h.hosts[0].AddMachine("vm-1", fenced); err != nil {
		t.Fatal(err)
	}

	taken, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close(t.Context())

	select {
	case vmID := <-closed:
		if vmID != "vm-1" {
			t.Fatalf("the host reported closing %q", vmID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fenced host never learned its epoch had moved")
	}
	if !fenced.closed() {
		t.Fatal("the fenced host reported the VM closed without stopping its VMM")
	}
	if machines := h.hosts[0].Machines(); len(machines) != 0 {
		t.Fatalf("the fenced host still runs %v", machines)
	}
	for _, open := range h.hosts[0].Volumes().VMs() {
		if open.ID() == "vm-1" {
			t.Fatal("the fenced host still holds the VM's volumes")
		}
	}
}

// TestAFencedHostSealedByAForkPointStopsServing: a VM whose pages a fork point
// holds is never checkpointed, so the checkpoint loop is no place for it to
// learn anything. It must still learn, and everything it serves of that VM must
// stop: the child's pages it holds, another fork of it, and its handoff.
func TestAFencedHostSealedByAForkPointStopsServing(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	h.configs[0].CheckpointInterval = -1
	closed := make(chan string, 1)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], arenas[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 5)
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	// The parent is sealed from here: a child of this point runs on another
	// host and pulls the parent's unpublished pages out of its page server.
	guest.store("ram0", 1, 6)
	if _, err := h.hosts[0].Fork(t.Context(), "vm-1", []string{"child-1"}, h.pages[1]); err != nil {
		t.Fatal(err)
	}
	if !vm.Status().Sealed {
		t.Fatal("the fork left the parent unsealed")
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 || serving[0] != "child-1" {
		t.Fatalf("the parent's host serves %v, want the child it forked out", serving)
	}

	taken, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close(t.Context())

	select {
	case vmID := <-closed:
		if vmID != "vm-1" {
			t.Fatalf("the host reported closing %q", vmID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fenced host never closed the VM a fork point had sealed")
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("the fenced host still serves %v, want a host that serves nothing of a VM it lost", serving)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "vm-1", []string{"child-2"}, h.pages[1]); err == nil {
		t.Fatal("the fenced host forked a VM it had lost")
	}
	if _, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1]); err == nil {
		t.Fatal("the fenced host handed a VM it had lost over")
	}
}

// TestAHandoffConfirmsTheControlRecordFirst: a handoff is the one thing a host
// does that hands another host pages no checkpoint holds, and so the one thing
// that must never run on evidence this stale. The epoch timer is where a
// running VM learns it has been taken over, and it is a timer: a host between
// ticks, or one whose reads of the store fail while its pod network is fine,
// still holds a handle that looks healthy. Handing that VM over gives a third
// host one VM's memory made of two writers' pages.
//
// So the handoff re-reads the record itself. The source below is never told —
// both its loops are off — and every way it can hand a VM over must refuse.
func TestAHandoffConfirmsTheControlRecordFirst(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	h.configs[0].CheckpointInterval = -1
	h.configs[0].EpochInterval = -1
	var received *machine
	h.configs[2].Migration.StartVM = starter(t, pagers[2], arenas[2], &received)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], arenas[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 3)
	// The VM is openable elsewhere only once a checkpoint of it is published.
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	// The guest goes on writing pages no checkpoint holds, which are exactly
	// the pages a handoff would serve.
	guest.store("ram0", 1, 4)

	taken, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close(t.Context())

	if _, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[2]); !errors.Is(err, volume.ErrNeedsRecovery) {
		t.Fatalf("the superseded host handed the VM over: %v, want a handle that must be reopened", err)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "vm-1", []string{"child-1"}, h.pages[1]); !errors.Is(err, volume.ErrNeedsRecovery) {
		t.Fatalf("the superseded host forked the VM onto another host: %v, want a handle that must be reopened", err)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "vm-1", []string{"child-2"}, ""); !errors.Is(err, volume.ErrNeedsRecovery) {
		t.Fatalf("the superseded host forked the VM onto itself: %v, want a handle that must be reopened", err)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("the superseded host serves %v, want a host that handed nothing over", serving)
	}
	if machines := h.hosts[2].Machines(); len(machines) != 0 {
		t.Fatalf("a third host runs %v, want one that was never handed the VM", machines)
	}
}

// TestAHandoffRefusesWhenTheControlRecordCannotBeRead: a record that cannot be
// read is not evidence of a takeover, and everywhere else that is a reason to
// change nothing. The handoff is the exception: it gives another host pages
// nothing afterwards can check, so it proceeds on a confirmed record or not at
// all. The VM goes on running here, which is what the next attempt finds.
func TestAHandoffRefusesWhenTheControlRecordCannotBeRead(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	h.configs[0].CheckpointInterval = -1
	h.configs[0].EpochInterval = -1
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], arenas[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 3)
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}

	h.runtime.ObjectStore().FailNext(sim.ObjectGet, 1)
	if _, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1]); !errors.Is(err, platform.ErrInjectedFault) {
		t.Fatalf("a handoff that could not read the record reported %v, want the read's own failure", err)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("the host serves %v after a refused handoff", serving)
	}
	if machines := h.hosts[0].Machines(); len(machines) != 1 || machines[0] != "vm-1" {
		t.Fatalf("the host runs %v after a refused handoff, want the VM still running here", machines)
	}
	// Nothing is wrong with the VM: the next handoff, over a store that answers,
	// is the one that moves it.
	handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if err != nil {
		t.Fatalf("migrating after the store answered again: %v", err)
	}
	if handoff.VMID != "vm-1" {
		t.Fatalf("handoff %+v", handoff)
	}
}
