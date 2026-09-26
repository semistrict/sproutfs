package host_test

import (
	"testing"
	"time"
)

// RAM is never checkpointed on the interval, so a copy a write fault made of a
// page the guest never changed would stay private for the VM's life. The
// interval gives it back instead, with the guest running.
func TestTheIntervalGivesBackARAMCopyTheGuestNeverChanged(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = 10 * time.Millisecond
	h.start(t)
	pagers := newPager(t, h.configs[0].Resources)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The page is published, so its resident page holds an identity a copy
	// can be compared with.
	guest.store("ram0", 0, 7)
	capture(t, vm, guest)
	// The whole of what the guest does: it takes the page writable and stores
	// nothing, which is what a cold read is where KVM asks for every page
	// writable.
	if err := guest.memoryRegions["ram0"].Fault(t.Context(), 0, true); err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("vm-1")
	deadline := time.Now().Add(10 * time.Second)
	for {
		stats, err := pagers.ram().Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.GivenBackPages == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the interval never gave the copy back: %+v", stats)
		}
		time.Sleep(time.Millisecond)
	}
	if got := guest.load("ram0", 0)[0]; got != 7 {
		t.Fatalf("the page given back reads %d, want the 7 the guest stored", got)
	}
	region, err := guest.memoryRegions["ram0"].Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if region.PrivatePages != 0 {
		t.Fatalf("the guest still holds %d private RAM pages, want none", region.PrivatePages)
	}
}
