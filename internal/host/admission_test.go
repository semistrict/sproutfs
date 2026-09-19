package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// fillerVolumes takes all but one page of a test pager's 64-page logical cap,
// which is how a receive is put in front of a cap it cannot fit under.
var fillerVolumes = []volume.VolumeSpec{{Name: "ram0", Size: 63 * migrationPageSize, PageSize: migrationPageSize}}

// TestReceiveRefusesAVMThePagerCannotMap. The pager's logical-page cap is
// checked one region at a time, at attachment, which is long after the VMM
// process exists: a VM whose regions do not all fit has some of them admitted,
// its process started and then killed, and the guest dies with nothing but a
// socket that closed to say why. A host knows every region's size before it
// starts anything, so a VM that cannot fit is refused there and nothing is
// started to be killed.
func TestReceiveRefusesAVMThePagerCannotMap(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	var received *machine
	started := false
	h.configs[1].Migration.StartVM = func(ctx context.Context, vm *volume.VM,
		backings map[string]vmmemory.Backing, state []byte) (host.Machine, error) {
		started = true
		return starter(t, pagers[1], arenas[1], &received)(ctx, vm, backings, state)
	}
	h.configs[1].Pager = pagers[1]
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], arenas[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 0, 7)
	if err := h.hosts[0].AddMachine("vm-1", source); err != nil {
		t.Fatal(err)
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	// Fill the destination's logical cap with a region of its own, so the
	// received VM's eight pages are exactly what does not fit. Its pages are
	// untouched: this is the per-region metadata cap, not the arena.
	filler, err := h.hosts[1].Volumes().Create(t.Context(), "vm-filler", fillerVolumes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newMachine(t, pagers[1], arenas[1], filler, nil); err != nil {
		t.Fatal(err)
	}
	if free := pagers[1].LogicalHeadroom(); free >= 8 {
		t.Fatalf("the destination's pager still has room for the VM: %d pages left", free)
	}
	if free := h.hosts[1].Status().LogicalPagesFree; free != 1 {
		t.Fatalf("host status reports %d logical pages left, want 1", free)
	}

	if _, err := h.hosts[1].Receive(t.Context(), handoff); !errors.Is(err, vmmemory.ErrCapacity) {
		t.Fatalf("receiving a VM the pager cannot map = %v, want the capacity refusal", err)
	}
	if started {
		t.Fatal("the destination started a VMM for a VM its pager could never map")
	}
}
