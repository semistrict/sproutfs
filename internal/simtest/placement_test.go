package simtest_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// rangePages is the RAM pages one 2 MiB-aligned range holds, which is the
// offsets its extent owns.
const rangePages = (2 << 20) / simtest.RAMPage

// A private page lives at the offset it has within its range, so what a range
// costs its VMM in mappings is how often it alternates between shared and
// private and not how many of its pages are private. This is that through the
// deployment's own stack — a real host, a real pager, the simulated VMM's own
// page table — with the byte model checked after every pattern.
func TestScatteredStoresCostMappingsPerRunAndNotPerPage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stores   []uint64
		mappings int
		extents  int
	}{
		{"one page", []uint64{300}, 1, 1},
		{"two adjacent pages forwards", []uint64{300, 301}, 1, 1},
		{"two adjacent pages backwards", []uint64{301, 300}, 1, 1},
		{"a run of eight", []uint64{304, 300, 302, 301, 306, 303, 305, 307}, 1, 1},
		{"eight pages far enough apart to stay apart",
			[]uint64{20, 52, 84, 116, 148, 180, 212, 244}, 8, 1},
		{"one page in each of three ranges",
			[]uint64{10, rangePages + 10, 2*rangePages + 10}, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, world := placedWorld(t)
				if err := world.StorePages("vm-1", simtest.MemoryVolume, tc.stores, 0x7e); err != nil {
					t.Fatal(err)
				}
				if got := world.Mappings("vm-1", simtest.MemoryVolume); got != tc.mappings {
					t.Errorf("%d stores are %d mappings, want %d", len(tc.stores), got, tc.mappings)
				}
				if got := world.PrivateExtents(0); got != tc.extents {
					t.Errorf("%d stores own %d extents, want %d", len(tc.stores), got, tc.extents)
				}
				if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
					t.Error(err)
				}
				if err := world.Checkpoint(ctx, "vm-1"); err != nil {
					t.Fatal(err)
				}
				if err := world.VerifyDurable(ctx, "vm-1"); err != nil {
					t.Error(err)
				}
				if err := world.Close(ctx); err != nil {
					t.Error(err)
				}
			})
		})
	}
}

// placedWorld is one host running one VM whose RAM is four 2 MiB ranges, with
// an extent for every range the pager may write into and no read-ahead or
// write-ahead, so what a test stores into is the whole of what it makes
// private. The VM is cold-started before it is handed back: a cold start
// discards the memory the create wrote, so every page is a hole again, no range
// owns an extent and the guest's stores are the only thing that makes one.
func placedWorld(t *testing.T) (context.Context, *simtest.World) {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 41,
		Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
			ConnectLatency: time.Microsecond},
		ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
			PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
			BytesPerSecond: 1 << 40}})
	prefix, err := platform.NewObjectPrefix("sproutfs/")
	if err != nil {
		t.Fatal(err)
	}
	const memoryPages = 4 * rangePages
	volumes := []volume.VolumeSpec{
		{Name: simtest.MemoryVolume, Size: memoryPages * simtest.RAMPage, PageSize: simtest.RAMPage}}
	topology := simtest.Topology{Hosts: []string{"host-0"},
		VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: volumes}}}
	k := knobs.Defaults()
	k.ResidentPages, k.DirtyPages, k.LogicalPages = memoryPages, memoryPages, memoryPages
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	c := sim.WithRuntime(t.Context(), runtime)
	world := simtest.MustStart(t, c, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: k, Prefix: prefix, Log: t.Logf})
	if err := world.Suspend(c, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if err := world.StartCold(c, "vm-1", 0); err != nil {
		t.Fatal(err)
	}
	if got := world.PrivateExtents(0); got != 0 {
		t.Fatalf("a cold-started VM's ranges own %d extents, want none", got)
	}
	if got := world.Mappings("vm-1", simtest.MemoryVolume); got != 0 {
		t.Fatalf("a cold-started VM's RAM is %d mappings, want none", got)
	}
	return c, world
}
