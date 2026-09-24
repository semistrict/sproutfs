package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// A host runs one pager per kind of region, each with its own arena, its own
// spill file and its own page. The pager knows nothing about VMs, so everything
// a VM is — its checkpoint, its loss window, what it holds privately — is the
// host's to put back together across the two. These are the places that would
// otherwise read one pager and report the whole host.

// What a VM holds privately spans both pagers, and it is bytes because it has
// to be: eight 4 KiB RAM pages and one 2 MiB disk page are nine pages of
// nothing in particular and 2,129,920 bytes exactly.
func TestPrivateBytesAddsTheTwoPagersUpInBytes(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = -1
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
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	private := func() uint64 {
		t.Helper()
		bytes, err := h.hosts[0].PrivateBytes(t.Context(), "vm-1")
		if err != nil {
			t.Fatal(err)
		}
		return bytes
	}
	if got := private(); got != 0 {
		t.Fatalf("a VM that has stored nothing holds %d private bytes", got)
	}
	for page := range uint64(8) {
		guest.store("ram0", page, byte(page+1))
	}
	if got, want := private(), uint64(8*checkpoint.PageSize4KiB); got != want {
		t.Fatalf("eight RAM stores hold %d private bytes, want %d", got, want)
	}
	guest.store("disk", 0, 42)
	want := uint64(8*checkpoint.PageSize4KiB + checkpoint.PageSize2MiB)
	if got := private(); got != want {
		t.Fatalf("eight RAM pages and one disk page hold %d private bytes, want %d", got, want)
	}

	// The status report keeps the two apart, because their page counts cannot
	// be added, and adds their arenas up in bytes.
	status := h.hosts[0].Status()
	if status.LogicalPagesFree.RAM != 64-8 || status.LogicalPagesFree.PMEM != 64-8 {
		t.Fatalf("the host reports %d RAM and %d PMEM logical pages free, want 56 and 56",
			status.LogicalPagesFree.RAM, status.LogicalPagesFree.PMEM)
	}
	ram, err := pagers.ram().Sharing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pmem, err := pagers.pmem().Sharing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if ram.Ram.UniqueBytes != 8*checkpoint.PageSize4KiB || pmem.Pmem.UniqueBytes != checkpoint.PageSize2MiB {
		t.Fatalf("the gauges read %d RAM bytes and %d PMEM bytes, want %d and %d",
			ram.Ram.UniqueBytes, pmem.Pmem.UniqueBytes, 8*checkpoint.PageSize4KiB, checkpoint.PageSize2MiB)
	}
	// Each pager holds only its own kind: a host that read one of them would
	// report a host with no disk at all, or none with any memory.
	if ram.Pmem.UniqueBytes != 0 || pmem.Ram.UniqueBytes != 0 {
		t.Fatalf("a pager holds pages of the other kind: %+v %+v", ram, pmem)
	}
}

// Pressure from the disk pager is one checkpoint of the VM's disks, which is
// what relieves it: the disk's stores that were waiting for a reservation land,
// and the guest's RAM goes on being its own, unpublished.
func TestPressureFromTheDiskPagerIsOneCheckpointOfTheDisks(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	// Far longer than this test: every checkpoint it sees is one the pressure
	// asked for.
	h.configs[0].CheckpointInterval = time.Hour
	kind := vmmemory.Ram
	pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) {
		if kind == vmmemory.Pmem {
			cfg.DirtyPages = 4
		}
		kind = vmmemory.Pmem
	})
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
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 77)
	before := vm.Status().Checkpoint.Sequence
	for page := range uint64(8) {
		guest.store("disk", page, byte(page+1))
	}
	if after := vm.Status().Checkpoint.Sequence; after == before {
		t.Fatal("the disk outran its dirty budget with no checkpoint of its own")
	}
	for page := range uint64(8) {
		if got := guest.load("disk", page)[0]; got != byte(page+1) {
			t.Fatalf("disk page %d reads %d, want %d", page, got, byte(page+1))
		}
	}
	if got := guest.load("ram0", 0)[0]; got != 77 {
		t.Fatalf("RAM reads %d after the disks' checkpoint, want 77", got)
	}
	if guest.regions["ram0"].OldestUnpublished().IsZero() {
		t.Fatal("the disks' checkpoint published the guest's RAM")
	}
}

// A RAM pager whose dirty budget fills is one no checkpoint relieves, because
// the interval checkpoints disks alone: the store is a stall and the host stops
// the VM, with its checkpoint loop running all the while.
func TestAFullRAMBudgetIsAStallEvenWithTheLoopRunning(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	closed := make(chan string, 1)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	h.configs[0].CheckpointInterval = time.Hour
	kind := vmmemory.Ram
	pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) {
		if kind == vmmemory.Ram {
			cfg.DirtyPages = 2
		}
		kind = vmmemory.Pmem
	})
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
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	var stalled error
	for page := range uint64(4) {
		if stalled = guest.regions["ram0"].Fault(t.Context(), page, true); stalled != nil {
			break
		}
	}
	if !errors.Is(stalled, vmmemory.ErrDirtyStalled) {
		t.Fatalf("the store past the RAM budget reported %v, want a stall", stalled)
	}
	select {
	case id := <-closed:
		if id != "vm-1" {
			t.Fatalf("the host closed %s, want vm-1", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the VM whose RAM no checkpoint could admit was not stopped")
	}
}

// A VM's loss window is the oldest unpublished write across its disks, in
// whichever pager they are. RAM is not in it: no interval checkpoint publishes
// RAM, so a RAM write is not one a window could ever end.
func TestTheLossWindowIsTheDisks(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = -1
	h.configs[0].LossWindow = time.Hour
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
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 6)
	if age, waiting := h.hosts[0].LossWindow("vm-1"); age != 0 || waiting {
		t.Fatalf("a VM that has stored only into RAM reports a window of %s (waiting %t)", age, waiting)
	}
	guest.store("disk", 0, 5)
	if age, waiting := h.hosts[0].LossWindow("vm-1"); age <= 0 || waiting {
		t.Fatalf("a write the disk holds reports %s (waiting %t)", age, waiting)
	}
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if age, _ := h.hosts[0].LossWindow("vm-1"); age != 0 {
		t.Fatalf("a checkpointed VM has held a write for %s", age)
	}
}

// A store no checkpoint can admit stops that VM, whichever pager's budget it
// ran out of, and stops only that VM: the other guest on the host goes on
// storing through both of its own regions.
func TestAStallInEitherPagerStopsOnlyThatVM(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	closed := make(chan string, 2)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	// No loop at all, so nothing can relieve either budget and a store past it
	// is a stall rather than a wait.
	h.configs[0].CheckpointInterval = -1
	pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) {
		cfg.DirtyPages = 2
	})
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	start := func(id string) *machine {
		t.Helper()
		vm, err := h.hosts[0].Volumes().Create(t.Context(), id, mixedVolumes)
		if err != nil {
			t.Fatal(err)
		}
		guest, err := newMachine(t, pagers, vm, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.hosts[0].AddMachine(id, guest); err != nil {
			t.Fatal(err)
		}
		return guest
	}
	stalling, bystander := start("vm-stalled"), start("vm-fine")

	// The PMEM budget is what this one runs out of. Its VM is stopped.
	for page := range uint64(4) {
		if err := stalling.regions["disk"].Fault(t.Context(), page, true); err != nil {
			if !errors.Is(err, vmmemory.ErrDirtyStalled) {
				t.Fatalf("the store past the PMEM budget failed with %v", err)
			}
			break
		}
	}
	select {
	case id := <-closed:
		if id != "vm-stalled" {
			t.Fatalf("the host closed %s, want the VM whose store stalled", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the VM whose store no checkpoint could admit was not stopped")
	}
	// The other VM's RAM pager is untouched by it, and so is its own disk once
	// the stalled VM's reservations have gone.
	bystander.store("ram0", 0, 9)
	if got := bystander.load("ram0", 0)[0]; got != 9 {
		t.Fatalf("the bystander reads %d, want 9", got)
	}
	select {
	case id := <-closed:
		t.Fatalf("the host also closed %s", id)
	default:
	}
}

// Closing a host closes both pagers, in the order a supervisor releases them,
// and a pager that would not close does not take the other with it.
func TestClosingAHostClosesBothPagers(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = -1
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
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 1)
	guest.store("disk", 0, 2)
	if err := h.hosts[0].Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The host's own shutdown does not close the pagers — the supervisor does,
	// after the VMM processes — so this is what that ordering looks like: the
	// regions are detached and then both pagers close, each releasing its own
	// arena and its own spill file.
	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pagers.close(context.Background()); err != nil {
		t.Fatalf("closing both pagers: %v", err)
	}
	for kind, arena := range map[vmmemory.RegionKind]*pageArena{
		vmmemory.Ram: pagers.arenas[vmmemory.Ram], vmmemory.Pmem: pagers.arenas[vmmemory.Pmem]} {
		for slot, held := range arena.slots {
			if held != nil {
				t.Fatalf("the %s arena still holds slot %d after its pager closed", kind, slot)
			}
		}
	}
	// A closed pager admits nothing, which is the other half of closing both.
	for _, kind := range []vmmemory.RegionKind{vmmemory.Ram, vmmemory.Pmem} {
		if _, err := pagers.pagers.For(kind).Attach(t.Context(),
			vmmemory.RegionBacking{Kind: kind, Backing: vm.Volume("ram0")}, newPageMapping(pagers.arenas[kind])); err == nil {
			t.Fatalf("the %s pager attached a region after it closed", kind)
		}
	}
}
