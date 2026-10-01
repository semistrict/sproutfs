package host_test

import (
	"testing"
)

// A source keeps the handoff it handed out for as long as it holds the pages,
// and hands it out again with what is left of its hold. A control plane that
// restarted mid-handover tries the receive again with it. A release ends it.
func TestASourceKeepsItsHandoffWhileItHoldsThePages(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	handoff, _ := migrateFourPages(t, h, pagers)
	handed, left, held := h.hosts[0].Handed("vm-1")
	if !held {
		t.Fatal("the source holds no handoff of the VM it migrated")
	}
	if handed.VMID != handoff.VMID || handed.Checkpoint != handoff.Checkpoint || handed.Source != handoff.Source ||
		len(handed.MemoryRegions) != len(handoff.MemoryRegions) {
		t.Fatalf("the source hands out %+v, want the handoff it returned, %+v", handed, handoff)
	}
	if left <= 0 || left > h.hosts[0].HoldTimeout() {
		t.Fatalf("the source has %v of its hold left, want some of %v", left, h.hosts[0].HoldTimeout())
	}
	taken, err := h.hosts[1].Receive(t.Context(), handed)
	if err != nil {
		t.Fatalf("receiving the handoff the source handed out again: %v", err)
	}
	defer taken.Close()
	if err := h.hosts[0].ReleaseMigrated("vm-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, held := h.hosts[0].Handed("vm-1"); held {
		t.Fatal("a released source still hands out the handoff")
	}
}
