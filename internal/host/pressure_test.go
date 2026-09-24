package host_test

import (
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// A guest that dirties faster than its interval is not a guest to kill: the
// pager asks its host for a checkpoint of the largest dirty region, the host
// takes it out of the interval's turn, and the stores that were waiting for a
// reservation land. Without the wiring the guest's fifth store fails, the
// session closes and the VMM dies.
func TestHostCheckpointsOutOfTurnWhenTheDirtyBudgetFills(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	// Far longer than this test: every checkpoint it sees is one the pager's
	// pressure asked for.
	h.configs[0].CheckpointInterval = time.Hour
	pagers := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 4, ReadAheadPages: 1})
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
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
	before := vm.Status().Checkpoint.Sequence
	// Eight pages against a four-page budget: the stores past it are admitted
	// only by a checkpoint nothing else in this test asks for.
	for page := range uint64(8) {
		guest.store("disk", page, byte(page+1))
	}
	if after := vm.Status().Checkpoint.Sequence; after == before {
		t.Fatalf("the guest outran its dirty budget at checkpoint %d with no checkpoint of its own", after)
	}
	for page := range uint64(8) {
		if got := guest.load("disk", page)[0]; got != byte(page+1) {
			t.Fatalf("page %d holds %d after the out-of-turn checkpoint, want %d", page, got, byte(page+1))
		}
	}
}

// A guest whose budget no checkpoint can relieve — here because its host runs
// no checkpoint loop for it — is stopped deliberately: the store fails as a
// stall, the host publishes what it can still capture, and the supervisor is
// told which VM went, in place of a VMM killed by a failed fault.
func TestHostStopsAVMNoCheckpointCanAdmitStoresFor(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	closed := make(chan string, 1)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	// No loop: nothing this host can do will take a checkpoint of this VM.
	h.configs[0].CheckpointInterval = -1
	pagers := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 2, ReadAheadPages: 1})
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
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 1)
	guest.store("ram0", 1, 2)
	if err := guest.Regions()["ram0"].Fault(t.Context(), 2, true); !errors.Is(err, vmmemory.ErrDirtyStalled) {
		t.Fatalf("the store past the budget failed with %v, want a dirty-budget stall", err)
	}
	select {
	case stopped := <-closed:
		if stopped != "vm-1" {
			t.Fatalf("the host stopped %q, want the stalled VM", stopped)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the host left the stalled VM running")
	}
	if running := h.hosts[0].Machines(); len(running) != 0 {
		t.Fatalf("the host still runs %v after stopping the stalled VM", running)
	}
	if got := vm.Status().Checkpoint.Sequence; got == 0 {
		t.Fatal("the deliberate stop published nothing of what it could still capture")
	}
}
