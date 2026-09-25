package host_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/volume"
)

// TestStoppingAVMPublishesWhatItHeldAndGivesThePagesBack: a stop is the
// deliberate end of a running VM that leaves the VM behind. Everything the
// guest wrote since its last checkpoint is published — that is what makes a
// stop different from losing the host — and then the VMM process is closed, the
// pages go back to the pager and the handle is released, so another host can
// open the VM at exactly the bytes the stop published.
func TestStoppingAVMPublishesWhatItHeldAndGivesThePagesBack(t *testing.T) {
	h := newHostHarness(t)
	pagers := newPager(t, h.configs[0].Resources)
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	// Written after the last checkpoint, so only this host's pages hold it:
	// a stop that published nothing would lose it exactly as a host loss does.
	guest.store("ram0", 1, 9)
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}

	stopped, err := h.hosts[0].Stop(t.Context(), "vm-1", true)
	if err != nil {
		t.Fatalf("stopping a running VM: %v", err)
	}
	// The checkpoint it reports is the pause the VM comes back at, and
	// nothing else records it: the handle that knew is released by now.
	if stopped.VM != "vm-1" || stopped.Sequence == 0 {
		t.Fatalf("the stop published %s, want a checkpoint of the VM it stopped", stopped)
	}
	if running := h.hosts[0].Machines(); len(running) != 0 {
		t.Fatalf("the host still runs %v after stopping it", running)
	}
	if !guest.closed.Load() {
		t.Fatal("the VMM process of a stopped VM is still alive")
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("a stopped VM is still served: %v", serving)
	}

	// The VM is only its control record and its objects now, so another host
	// opens it at the checkpoint the stop published.
	reopened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatalf("opening a stopped VM elsewhere: %v", err)
	}
	page := make([]byte, migrationPageSize)
	if err := reopened.Volume("ram0").Read(t.Context(), migrationPageSize, page); err != nil {
		t.Fatal(err)
	}
	if want := bytes.Repeat([]byte{9}, migrationPageSize); !bytes.Equal(page, want) {
		t.Fatalf("the stopped VM came back holding %d..., want the byte its guest wrote before the stop", page[0])
	}
	if err := reopened.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestAPlainStopKeepsTheDisksAndBootsCold: a stop that does not suspend
// publishes the VM's disks and nothing of its memory, so the VM comes back cold:
// its disk as the guest left it, its memory zeroes, and no VMM state to restore
// — not even the state of the checkpoint before it.
func TestAPlainStopKeepsTheDisksAndBootsCold(t *testing.T) {
	h := newSizedHostHarness(t, 2)
	pagers := newMixedPagers(t, h.configs[0].Resources, nil)
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", mixedVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 5)
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 1, 9)
	guest.store("disk", 0, 42)
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Stop(t.Context(), "vm-1", false); err != nil {
		t.Fatalf("stopping a running VM: %v", err)
	}

	reopened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(t.Context())
	state, err := h.hosts[1].Starting(t.Context(), reopened, "ram0")
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("a plainly stopped VM starts from %d bytes of VMM state, want a cold boot", len(state))
	}
	disk := make([]byte, checkpoint.PageSize2MiB)
	if err := reopened.Volume("disk").Read(t.Context(), 0, disk); err != nil {
		t.Fatal(err)
	}
	if want := bytes.Repeat([]byte{42}, len(disk)); !bytes.Equal(disk, want) {
		t.Fatalf("the disk came back holding %d..., want the byte its guest wrote before the stop", disk[0])
	}
	memory := make([]byte, 2*checkpoint.PageSize4KiB)
	if err := reopened.Volume("ram0").Read(t.Context(), 0, memory); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(memory, make([]byte, len(memory))) {
		t.Fatalf("a cold boot kept memory: %d, %d...", memory[0], memory[checkpoint.PageSize4KiB])
	}
}

// TestStoppingAVMAForkPointHoldsIsRefused: a child reads the pages no
// checkpoint holds out of the memory the parent's seal froze, and those pages
// are the parent's VMM process's. Closing that process while a child is still
// pulling them takes the point out from under it, so a stop of a sealed VM is
// refused exactly as a delete of one is — and the parent goes on running.
func TestStoppingAVMAForkPointHoldsIsRefused(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	for i := range h.configs {
		h.configs[i].Migration = host.MigrationConfig{Address: h.pages[i], PageSize: migrationPageSize}
	}
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 3)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1]); err != nil {
		t.Fatal(err)
	}

	if _, err := h.hosts[0].Stop(t.Context(), "parent", true); !errors.Is(err, volume.ErrSealed) {
		t.Fatalf("stopping a VM a fork point holds = %v, want ErrSealed", err)
	}
	if running := h.hosts[0].Machines(); len(running) != 1 || running[0] != "parent" {
		t.Fatalf("the refused stop left the host running %v", running)
	}
	if guest.closed.Load() {
		t.Fatal("the refused stop closed the parent's VMM process anyway")
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 || serving[0] != "child" {
		t.Fatalf("the refused stop gave up the point the child reads: %v", serving)
	}
	if err := h.hosts[0].Abandon("child"); err != nil {
		t.Fatal(err)
	}
}

// TestStoppingAVMThisHostDoesNotRunIsNotFound. A stop closes a guest, and only
// the host running it has one; the deployment asks the host its table names and
// has to be told when that is the wrong one.
func TestStoppingAVMThisHostDoesNotRunIsNotFound(t *testing.T) {
	h := newHostHarness(t)
	h.start(t)
	if _, err := h.hosts[0].Stop(t.Context(), "vm-nobody-runs", true); !errors.Is(err, host.ErrNotRunning) {
		t.Fatalf("stopping a VM this host does not run = %v, want ErrNotRunning", err)
	}
}
