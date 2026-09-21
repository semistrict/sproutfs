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

// One pause seals every region a VM maps, in both pagers, so pressure from
// either pager is one checkpoint of the whole VM and not two. Here the PMEM
// budget is the one that fills, and what relieves it is a checkpoint that
// publishes the RAM pages too.
func TestPressureFromEitherPagerSealsTheWholeVMOnce(t *testing.T) {
	for _, full := range []struct {
		name      string
		volume    string
		pages     uint64
		configure func(kind vmmemory.RegionKind, cfg *vmmemory.Config)
	}{
		{"the RAM pager's budget fills", "ram0", 8, func(kind vmmemory.RegionKind, cfg *vmmemory.Config) {
			if kind == vmmemory.Ram {
				cfg.DirtyPages = 4
			}
		}},
		{"the PMEM pager's budget fills", "disk", 8, func(kind vmmemory.RegionKind, cfg *vmmemory.Config) {
			if kind == vmmemory.Pmem {
				cfg.DirtyPages = 4
			}
		}},
	} {
		t.Run(full.name, func(t *testing.T) {
			h := newSizedHostHarness(t, 1)
			// Far longer than this test: every checkpoint it sees is one the
			// pressure of one of the two pagers asked for.
			h.configs[0].CheckpointInterval = time.Hour
			kind := vmmemory.Ram
			pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) {
				full.configure(kind, cfg)
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
			// One store into the other region first, so the checkpoint the
			// pressure asks for has both regions to seal.
			other := "disk"
			if full.volume == "disk" {
				other = "ram0"
			}
			guest.store(other, 0, 77)
			before := vm.Status().Checkpoint.Sequence
			for page := range full.pages {
				guest.store(full.volume, page, byte(page+1))
			}
			after := vm.Status().Checkpoint.Sequence
			if after == before {
				t.Fatalf("%s outran its dirty budget with no checkpoint of its own", full.volume)
			}
			// Every page of both regions still reads what the guest wrote,
			// which is what says the one pause took both.
			if got := guest.load(other, 0)[0]; got != 77 {
				t.Fatalf("the other region reads %d after the checkpoint, want 77", got)
			}
			for page := range full.pages {
				if got := guest.load(full.volume, page)[0]; got != byte(page+1) {
					t.Fatalf("%s page %d reads %d, want %d", full.volume, page, got, byte(page+1))
				}
			}
		})
	}
}

// A VM's loss window is the oldest unpublished write across every region it
// maps, in both pagers. A host that measured one of them would report a VM as
// durable while its disk held writes minutes old.
func TestTheLossWindowSpansBothPagers(t *testing.T) {
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
	if age, waiting := h.hosts[0].LossWindow("vm-1"); age != 0 || waiting {
		t.Fatalf("a VM that has stored nothing has held a write for %s", age)
	}
	// A write held by the PMEM pager alone opens the VM's window. A host that
	// read the RAM pager would report this VM as holding nothing.
	guest.store("disk", 0, 5)
	disk, waiting := h.hosts[0].LossWindow("vm-1")
	if disk <= 0 || waiting {
		t.Fatalf("a write only the PMEM pager holds reports %s (waiting %t)", disk, waiting)
	}
	// A later write to RAM does not restart it: the window is the oldest write
	// across both pagers, so it goes on running from the disk's.
	const gap = 20 * time.Millisecond
	time.Sleep(gap)
	guest.store("ram0", 0, 6)
	both, _ := h.hosts[0].LossWindow("vm-1")
	if both < disk+gap {
		t.Fatalf("after a RAM store the window is %s, want at least the disk's %s plus %s",
			both, disk, gap)
	}
	// Publishing the VM ends it for both regions at once: one pause sealed
	// them, so neither pager is left holding a write.
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if age, _ := h.hosts[0].LossWindow("vm-1"); age != 0 {
		t.Fatalf("a checkpointed VM has held a write for %s", age)
	}
	// And the RAM pager alone opens it too, which is the other direction.
	guest.store("ram0", 1, 7)
	if age, _ := h.hosts[0].LossWindow("vm-1"); age <= 0 {
		t.Fatal("a write only the RAM pager holds reports no window")
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
