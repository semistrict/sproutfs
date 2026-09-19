package host_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// What one VM holds privately is the memory its host could not have shared with
// anything, which is the number an operator reads beside what the pager is
// sharing. The pager measures it per region and knows nothing about VMs, so the
// host is where the regions of one VM are added up; nowhere else has both
// halves.
func TestAHostReportsEachVMsPrivateBytes(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = -1
	pager, arena := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
	h.configs[0].Pager = pager
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pager, arena, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	private := func(id string) uint64 {
		t.Helper()
		bytes, err := h.hosts[0].PrivateBytes(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return bytes
	}
	if bytes := private("vm-1"); bytes != 0 {
		t.Fatalf("a VM that has stored nothing holds %d private bytes, want none", bytes)
	}
	guest.store("ram0", 0, 7)
	guest.store("ram0", 3, 9)
	if bytes := private("vm-1"); bytes != 2*vmmemory.PageSize {
		t.Fatalf("a VM that stored into two pages holds %d private bytes, want %d",
			bytes, 2*vmmemory.PageSize)
	}
	// The checkpoint is what ends it: the pages are the store's now, so the VM
	// holds nothing its volumes do not.
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if bytes := private("vm-1"); bytes != 0 {
		t.Fatalf("a checkpointed VM holds %d private bytes, want none", bytes)
	}
	if bytes := private("vm-absent"); bytes != 0 {
		t.Fatalf("a VM this host does not run holds %d private bytes, want none", bytes)
	}
}
