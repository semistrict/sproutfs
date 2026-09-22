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

// A store into fresh zeros makes a whole run of them private in one mapping
// command, which is what a guest writing memory forwards after a cold start is
// served by. The pages of a run the guest never stores into are shared with
// nobody, so making them private gives no sharing back — and they cost nothing
// past the next checkpoint either: they read back as zeroes, the publication
// gives them no object, and the retire hands each one back to the hole it was.
//
// This is that through the deployment's own stack. A cold start is where a
// guest's memory is holes, so the VM is stopped and started cold, and then the
// guest stores into one page of every run: the worst pattern for write-ahead,
// which makes the whole region private for one page in eight stored into. What
// the host holds privately afterwards, what the volume holds and what every
// page reads back as must all be exactly the stores.
func TestZeroWriteAheadPublishesOnlyWhatTheGuestStored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const memoryPages, run = 64, 8
		runtime := sim.New(sim.Config{Seed: 23,
			Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
				ConnectLatency: time.Microsecond},
			ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
				PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
				BytesPerSecond: 1 << 40}})
		prefix, err := platform.NewObjectPrefix("sproutfs/")
		if err != nil {
			t.Fatal(err)
		}
		volumes := []volume.VolumeSpec{
			{Name: simtest.MemoryVolume, Size: memoryPages * simtest.RAMPage, PageSize: simtest.RAMPage}}
		topology := simtest.Topology{Hosts: []string{"host-0"},
			VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: volumes}}}
		k := knobs.Defaults()
		k.ResidentPages, k.DirtyPages, k.LogicalPages = 128, 128, 512
		k.ReadAheadPages, k.WriteAheadPages = run, run
		if err := k.Validate(); err != nil {
			t.Fatal(err)
		}
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: k, Prefix: prefix, Log: t.Logf})

		// A cold start discards the memory the create wrote, so every page of
		// the region is a hole again and none of it is this host's.
		if err := world.Stop(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.StartCold(ctx, "vm-1", 0); err != nil {
			t.Fatal(err)
		}
		held, err := world.Host(0).PrivateBytes(ctx, "vm-1")
		if err != nil {
			t.Fatal(err)
		}
		if held != 0 {
			t.Fatalf("a cold-started VM holds %d private bytes, want none", held)
		}

		var stored []uint64
		for page := uint64(0); page < memoryPages; page += run {
			stored = append(stored, page)
		}
		if err := world.StorePages("vm-1", simtest.MemoryVolume, stored, 0x7e); err != nil {
			t.Fatal(err)
		}
		if held, err = world.Host(0).PrivateBytes(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if want := uint64(memoryPages) * simtest.RAMPage; held != want {
			t.Fatalf("%d stores made %d private bytes, want the whole region's %d", len(stored), held, want)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		// Every page of the region was sealed and the settle gave none of them
		// back: a page made from zeros has no origin to be compared with. What
		// takes them back is the retire, once the publication has said which of
		// them the volume holds no object for.
		if sealed, unchanged := world.Sealed("vm-1"); sealed != memoryPages || unchanged != 0 {
			t.Fatalf("the checkpoint sealed %d pages and settled %d, want %d and 0",
				sealed, unchanged, memoryPages)
		}
		if held, err = world.Host(0).PrivateBytes(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if held != 0 {
			t.Fatalf("the VM holds %d private bytes after its checkpoint, want none", held)
		}
		// The byte model, through the guest's own mappings and through the
		// volume the checkpoint published: the stores where the guest made them
		// and a hole everywhere else.
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Error(err)
		}
		if err := world.VerifyDurable(ctx, "vm-1"); err != nil {
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
