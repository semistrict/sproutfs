package simtest_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
)

// TestForksThatOnlyReadPublishNothingAndGoOnSharing is the fan-out the pager
// exists for, on a host whose faults all claim to be writes: two children of
// one pause read every page of their memory and their disk, store not one byte,
// and are checkpointed. A write fault is not a store — KVM finishes a cold read
// from a worker that always asks for the page writable — so each of those reads
// costs a copy of the page; the settle behind each checkpoint's pause is what
// says the copies were never dirty.
//
// What each child must then publish is nothing at all, and what it must hold of
// its own is nothing at all: every page it touched is the parent's page again.
func TestForksThatOnlyReadPublishNothingAndGoOnSharing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 11,
			Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
				ConnectLatency: time.Microsecond},
			ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
				PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
				BytesPerSecond: 1 << 40}})
		prefix, err := platform.NewObjectPrefix("sproutfs/")
		if err != nil {
			t.Fatal(err)
		}
		const memoryPages, diskPages = 4, 2
		volumes := []volume.VolumeSpec{
			{Name: simtest.MemoryVolume, Size: memoryPages * simtest.PageSize, PageSize: simtest.PageSize},
			{Name: "disk", Size: diskPages * simtest.PageSize, PageSize: simtest.PageSize}}
		topology := simtest.Topology{Hosts: []string{"host-0"},
			VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: volumes}}}
		k := knobs.Defaults()
		k.ResidentPages, k.DirtyPages, k.LogicalPages = 64, 64, 256
		k.ReadAheadPages, k.WriteAheadPages = 1, 1
		k.SettleWorkers = 4
		if err := k.Validate(); err != nil {
			t.Fatal(err)
		}
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: k, Prefix: prefix, Log: t.Logf})

		// The parent writes every page and publishes them, so the fork point
		// holds nothing unpublished and every page a child inherits is a page
		// the store has under a name of its own.
		if err := world.StoreAll("vm-1", 0x5c); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		children := []simtest.VMSpec{
			{ID: "vm-1-a", Parent: "vm-1", Host: 0, Volumes: volumes},
			{ID: "vm-1-b", Parent: "vm-1", Host: 0, Volumes: volumes},
		}
		if err := world.FanOut(ctx, "vm-1", children); err != nil {
			t.Fatal(err)
		}

		const touched = memoryPages + diskPages
		private := uint64(touched) * simtest.PageSize
		for _, child := range children {
			if err := world.TakeWritable(ctx, child.ID); err != nil {
				t.Fatalf("%s: taking every page writable: %v", child.ID, err)
			}
			held, err := world.Host(0).PrivateBytes(ctx, child.ID)
			if err != nil {
				t.Fatal(err)
			}
			if held != private {
				t.Fatalf("%s holds %d private bytes after reading everything, want %d",
					child.ID, held, private)
			}
		}
		for _, child := range children {
			if err := world.Checkpoint(ctx, child.ID); err != nil {
				t.Fatalf("%s: checkpoint: %v", child.ID, err)
			}
			sealed, unchanged := world.Sealed(child.ID)
			if sealed != 0 || unchanged != touched {
				t.Fatalf("%s published %d pages and settled %d, want none published and %d settled",
					child.ID, sealed, unchanged, touched)
			}
			held, err := world.Host(0).PrivateBytes(ctx, child.ID)
			if err != nil {
				t.Fatal(err)
			}
			if held != 0 {
				t.Fatalf("%s still holds %d private bytes after its checkpoint, want none", child.ID, held)
			}
		}
		// The parent's own pages are untouched by any of it, and every guest
		// still reads exactly what it last wrote.
		if held, err := world.Host(0).PrivateBytes(ctx, "vm-1"); err != nil || held != 0 {
			t.Fatalf("the parent holds %d private bytes: %v", held, err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Error(err)
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Error(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Error(err)
		}
	})
}
