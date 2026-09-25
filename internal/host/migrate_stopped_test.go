package host_test

import (
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// twoMemoryRegionVolumes is a VM with more than one memory region, which is what makes a
// migration something that can fail part way through giving them up.
var twoMemoryRegionVolumes = []volume.VolumeSpec{
	{Name: "ram0", Size: 4 * migrationPageSize, PageSize: migrationPageSize},
	{Name: "root", Size: 4 * migrationPageSize, PageSize: migrationPageSize},
}

// TestAMigrationStoppedPartWayDiscardsTheVM. A migration gives each memory region's
// volume up in turn, and a failure after the first has succeeded cannot put the
// guest back: it is stopped, and the memory regions that went are serving pages for a
// volume this host no longer owns. `vmmigrate` says so with ErrStopped. Running
// the checkpoint loop over it again leaves a zombie — a stopped guest whose
// interval tries to seal memory regions that no longer own what they map, whose VMM
// process is still alive, and whose handle nothing ever closes.
func TestAMigrationStoppedPartWayDiscardsTheVM(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	closed := make(chan string, 1)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", twoMemoryRegionVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 0, 1)
	if err := h.hosts[0].AddMachine("vm-1", source); err != nil {
		t.Fatal(err)
	}
	// The memory regions give their volumes up in name order, so a seal held on the
	// second is a handoff that fails after the first has already happened.
	if err := source.memoryRegions["root"].Seal(t.Context()); err != nil {
		t.Fatal(err)
	}

	_, err = h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if !errors.Is(err, vmmigrate.ErrStopped) {
		t.Fatalf("a migration that gave up one memory region and failed on the next = %v", err)
	}
	if machines := h.hosts[0].Machines(); len(machines) != 0 {
		t.Fatalf("the host still runs a VM it stopped and could not hand over: %v", machines)
	}
	if !source.closed.Load() {
		t.Fatal("the VMM process of a VM nothing can run again is still alive")
	}
	select {
	case id := <-closed:
		if id != "vm-1" {
			t.Fatalf("the supervisor was told about %q", id)
		}
	default:
		t.Fatal("the supervisor was never told the VM was closed")
	}
}
