package host_test

import (
	"testing"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/volume"
)

// nestedVM is a nested VM running on the first host of a two-host harness,
// with its guest's RAM and disk each holding one store.
func nestedVM(t *testing.T) (*hostHarness, *volume.VM, *machine) {
	t.Helper()
	h := newSizedHostHarness(t, 2)
	pagers := newMixedPagers(t, h.configs[0].Resources, nil)
	h.configs[0].Pagers = pagers.pagers
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", mixedVolumes)
	if err != nil {
		t.Fatal(err)
	}
	nested := true
	if err := h.hosts[0].Reshape(t.Context(), vm, host.ColdShape{Memory: "ram0", Nested: &nested}); err != nil {
		t.Fatal(err)
	}
	if !vm.Nested() {
		t.Fatal("the VM is not nested after a cold boot that made it so")
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 41)
	guest.store("disk", 0, 42)
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	return h, vm, guest
}

// A nested VM is captured and suspended like any other VM: the Firecracker
// fork never offers its guest the VMX controls that make KVM write its RAM
// behind the page tables (see vmmachine's nested.go), so nothing about its RAM
// is out of the pager's reach. The VM a suspend leaves is still nested
// wherever it is opened.
func TestANestedVMIsCapturedAndSuspendedLikeAnyOther(t *testing.T) {
	h, vm, guest := nestedVM(t)
	if _, err := host.Capture(t.Context(), vm, guest, nil, volume.Terms{}); err != nil {
		t.Fatalf("a capture of a nested VM: %v", err)
	}
	if _, err := h.hosts[0].Stop(t.Context(), "vm-1", hostapi.StopRequest{Suspend: true}); err != nil {
		t.Fatalf("a suspend of a nested VM: %v", err)
	}
	reopened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(t.Context())
	if !reopened.Nested() {
		t.Fatal("the suspended VM opened on another host is not nested")
	}
}

// A nested VM is forked like any other VM, and seals the point its children
// start from.
func TestANestedVMIsForkedLikeAnyOther(t *testing.T) {
	h, vm, _ := nestedVM(t)
	if _, err := h.hosts[0].Fork(t.Context(), "vm-1", []string{"vm-2"}, h.pages[1]); err != nil {
		t.Fatalf("a fork of a nested VM: %v", err)
	}
	if !vm.Status().Sealed {
		t.Fatal("the fork left the nested parent unsealed")
	}
}
