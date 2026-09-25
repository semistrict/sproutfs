package host_test

import (
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/vmmemory"
)

// TestStoppingAStalledVMGivesUpTheForkPointsTakenOnIt: a VM the dirty budget can
// no longer admit stores for is stopped deliberately, and a deliberate stop is
// the host giving that VM up like any other. It skipped the give-up order
// entirely: the fork points taken on it were left registered, so the page
// server went on offering a child pages out of a VMM process this stop had
// closed, and nothing claimed the close, so the watcher that found the same
// process dead closed it a second time and told the supervisor twice.
func TestStoppingAStalledVMGivesUpTheForkPointsTakenOnIt(t *testing.T) {
	h := newSizedHostHarness(t, 2)
	closed := make(chan string, 4)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	// No loop: nothing this host can do will take a checkpoint of this VM.
	h.configs[0].CheckpointInterval = -1
	pagers := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 2, ReadAheadPages: 1})
	h.configs[0].Pagers = pagers.pagers
	for i := range h.configs {
		h.configs[i].Migration = host.MigrationConfig{Address: h.pages[i], PageSize: migrationPageSize}
	}
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 1)
	guest.store("ram0", 1, 2)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1]); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 || serving[0] != "child" {
		t.Fatalf("the fork point is served for %v, want the child", serving)
	}

	// The seal holds the whole dirty budget and a sealed VM is not checkpointed,
	// so the next store is a stall no checkpoint can relieve.
	if err := guest.MemoryRegions()["ram0"].Fault(t.Context(), 2, true); !errors.Is(err, vmmemory.ErrDirtyStalled) {
		t.Fatalf("the store past the budget failed with %v, want a dirty-budget stall", err)
	}
	select {
	case stopped := <-closed:
		if stopped != "parent" {
			t.Fatalf("the host stopped %q, want the stalled VM", stopped)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the host left the stalled VM running")
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("a stopped parent still offers %v a point whose pages are gone", serving)
	}
	if running := h.hosts[0].Machines(); len(running) != 0 {
		t.Fatalf("the host still runs %v after stopping the stalled VM", running)
	}
	// The VMM process is gone, which the watcher finds too. One stall and one
	// death close the VM once between them.
	guest.die(errors.New("the VMM process ended"))
	select {
	case again := <-closed:
		t.Fatalf("the host gave %s up a second time", again)
	case <-time.After(250 * time.Millisecond):
	}
}
