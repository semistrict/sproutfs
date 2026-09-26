package host_test

import (
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/volume"
)

// TestAForkOrACaptureIntoAnotherTenantIsRefusedBeforeThePause: no page crosses
// between tenants, so a fork or a capture whose new VM belongs to another
// tenant than its source is refused before the source is paused for it. The
// source keeps running, unsealed, and no VM of the other tenant exists.
func TestAForkOrACaptureIntoAnotherTenantIsRefusedBeforeThePause(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = -1
	pagers := newPager(t, h.configs[0].Resources)
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "acme/vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	counting := newCountingMachine(guest)
	if err := h.hosts[0].AddMachine("acme/vm-1", counting); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("acme/vm-1")

	if _, err := h.hosts[0].Fork(t.Context(), "acme/vm-1", []string{"acme/fork-a", "zeta/fork-b"}, ""); !errors.Is(err, volume.ErrOtherTenant) {
		t.Fatalf("a fork with a child of another tenant gave %v, want ErrOtherTenant", err)
	}
	if _, err := h.hosts[0].CaptureInto(t.Context(), "acme/vm-1", "copy"); !errors.Is(err, volume.ErrOtherTenant) {
		t.Fatalf("a capture into a VM of no tenant gave %v, want ErrOtherTenant", err)
	}
	counting.mu.Lock()
	captures := counting.captures
	counting.mu.Unlock()
	if captures != 0 {
		t.Fatalf("the source was paused %d times for refused requests, want never", captures)
	}
	if vm.Status().Sealed {
		t.Fatal("the source is left sealed")
	}
	for _, id := range []string{"acme/fork-a", "zeta/fork-b", "copy"} {
		if _, err := h.hosts[0].Control().Read(t.Context(), id); err == nil {
			t.Fatalf("%s exists after its request was refused", id)
		}
	}
}
