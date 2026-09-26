package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// checkpointVolume is what a checkpoint does to a memory region attached to a real
// volume: it seals, publishes the sealed pages through VM.Snapshot, and lets
// that publication retire the checkpoint.
func checkpointVolume(t *testing.T, vm *volume.VM, name string, r *vmmemory.MemoryRegion) error {
	t.Helper()
	if err := r.Seal(t.Context()); err != nil {
		return err
	}
	checkpoint, err := vm.Snapshot(t.Context(), volume.Prepared(nil, map[string]volume.DirtySource{name: r.Checkpoint()}), volume.Terms{})
	if err != nil {
		return errors.Join(err, r.Unseal(t.Context()))
	}
	return checkpoint.Wait(t.Context())
}

// A checkpoint publishes a mapped memory region's dirty pages into its volume's
// checkpoint, which is the moment they survive the loss of this host. A
// replacement owner fences the stale mapping, whose next checkpoint can no
// longer land.
func TestMappedVolumeCheckpointAndWriterReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPagerCluster(t)
		vm := c.create(t, "vm", 4)
		f := newFixture(t, 2, 8, 8)
		r, m := f.attach(vm.Volume("ram0"))
		access(t, r, m, 0, true)[0] = 61
		access(t, r, m, 1, true)[0] = 62
		access(t, r, m, 2, false) // force dirty spill before the checkpoint
		var data [1]byte
		if err := vm.Volume("ram0").Read(t.Context(), 0, data[:]); err != nil || data[0] != 0 {
			t.Fatalf("spill altered the volume: %v %d", err, data[0])
		}
		if err := checkpointVolume(t, vm, "ram0", r); err != nil {
			t.Fatal(err)
		}
		if err := vm.Volume("ram0").Read(t.Context(), 0, data[:]); err != nil || data[0] != 61 {
			t.Fatalf("the checkpoint did not reach the volume: %v %d", err, data[0])
		}
		replacement, err := c.manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer replacement.Close(t.Context())
		if err := replacement.Volume("ram0").Read(t.Context(), 0, data[:]); err != nil || data[0] != 61 {
			t.Fatalf("the takeover lost the published checkpoint: %v %d", err, data[0])
		}
		// The old mapping can still store into its own doomed pages, but those
		// bytes have nowhere to go: the checkpoint that would publish them is
		// fenced, and the memory region stops being eligible to run.
		access(t, r, m, 0, true)[0] = 99
		if err := checkpointVolume(t, vm, "ram0", r); !errors.Is(err, volume.ErrNeedsRecovery) {
			t.Fatalf("the stale owner published a checkpoint: %v", err)
		}
		if err := r.Verify(t.Context()); err == nil {
			t.Fatal("fenced memory region remained eligible to run")
		}
		if err := replacement.Volume("ram0").Read(t.Context(), 0, data[:]); err != nil || data[0] != 61 {
			t.Fatalf("the stale owner changed the published checkpoint: %v %d", err, data[0])
		}
	})
}

// An object-store outage cannot fail a guest store, which contacts nothing.
// What it fails is the checkpoint, and the memory region stays eligible to run: the
// bytes are still there, they are simply not durable yet.
func TestMappedVolumeOutageStallsTheCheckpointNotTheGuest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPagerCluster(t)
		vm := c.create(t, "outage", 4)
		f := newFixture(t, 4, 8, 8)
		r, m := f.attach(vm.Volume("ram0"))
		access(t, r, m, 0, true)[0] = 61
		if err := checkpointVolume(t, vm, "ram0", r); err != nil {
			t.Fatal(err)
		}
		access(t, r, m, 0, true)[0] = 99
		c.runtime.ObjectStore().Fail()
		if err := checkpointVolume(t, vm, "ram0", r); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("a checkpoint during the outage = %v, want ErrUnavailable", err)
		}
		c.runtime.ObjectStore().Recover()
		if err := r.Verify(t.Context()); err != nil {
			t.Fatalf("a failed publication left the memory region ineligible to run: %v", err)
		}
		// The abandoned checkpoint handed its pages back, so the retry carries them.
		if err := checkpointVolume(t, vm, "ram0", r); err != nil {
			t.Fatalf("the checkpoint after the outage: %v", err)
		}
		var data [1]byte
		if err := vm.Volume("ram0").Read(t.Context(), 0, data[:]); err != nil || data[0] != 99 {
			t.Fatalf("the published volume = %d, want 99: %v", data[0], err)
		}
	})
}
