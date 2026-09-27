package host_test

import (
	"errors"
	"testing"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/volume"
)

// A nested VM runs, and takes the checkpoints of its disks, but nothing that
// captures or moves its RAM: a capture, a suspend, a fork, a capture into a
// new VM and a live migration are all refused before the VM is touched, and it
// goes on running. A plain stop is a checkpoint of its disks, and the VM it
// leaves is still nested wherever it is opened.
func TestANestedVMRefusesEverythingThatCapturesItsRAM(t *testing.T) {
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
	guest.store("disk", 0, 42)
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	refused := map[string]error{}
	_, refused["a capture"] = host.Capture(t.Context(), vm, guest, nil, volume.Terms{})
	_, refused["a suspend"] = h.hosts[0].Stop(t.Context(), "vm-1", hostapi.StopRequest{Suspend: true})
	_, refused["a fork"] = h.hosts[0].Fork(t.Context(), "vm-1", []string{"vm-2"}, "")
	_, refused["a capture into a new VM"] = h.hosts[0].CaptureInto(t.Context(), "vm-1", "vm-3")
	_, refused["a live migration"] = h.hosts[0].Migrate(t.Context(), "vm-1", "elsewhere")
	for what, err := range refused {
		if !errors.Is(err, host.ErrNested) {
			t.Errorf("%s of a nested VM = %v, want ErrNested", what, err)
		}
	}
	if s := vm.Status(); s.Sealed {
		t.Fatal("a refused operation left the nested VM sealed")
	}
	if _, err := host.CaptureDisks(t.Context(), vm, guest, nil, volume.Terms{}); err != nil {
		t.Fatalf("a checkpoint of a nested VM's disks: %v", err)
	}
	if _, err := h.hosts[0].Stop(t.Context(), "vm-1", hostapi.StopRequest{}); err != nil {
		t.Fatalf("a plain stop of a nested VM: %v", err)
	}
	reopened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(t.Context())
	if !reopened.Nested() {
		t.Fatal("the stopped VM opened on another host is not nested")
	}
}
