package simtest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes: a
// destination runs its guest from the receive on. Until the orchestrator
// releases the source, and until its own bulk stream ends, every memory region
// of it asks the source for what it faults. Meanwhile the guest stores into
// the pages it received, some of them zeros, and a checkpoint of it publishes
// them and retires them. A fork's parent goes on storing too, but its page
// server serves the pause it was sealed at and it is not checkpointed until
// the point is released; a migration's source stopped before its handoff was
// taken. So neither source's pages change under the destination. What changes
// is the destination's own volume.
//
// So there are two answers about where a page's bytes are, and they must stay
// one. The source holds at best the version of a page before the destination
// published it. The volume names that publication, or the hole it wrote for a
// page of zeros. A page stripped of it for longer tells the retire that a page
// of the guest's has no object, and a load that asks the source hands the
// guest the version it wrote past. The arena here is smaller than one guest,
// so every page the destination published is evicted and read again while the
// source still serves.
func TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes(t *testing.T) {
	for _, handover := range []string{"fork", "migration"} {
		t.Run(handover, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runMeanwhile(t, handover == "fork")
			})
		})
	}
}

func runMeanwhile(t *testing.T, fork bool) {
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
	const memoryPages, diskPages = 16, 2
	volumes := []volume.VolumeSpec{
		{Name: simtest.MemoryVolume, Size: memoryPages * simtest.RAMPage, PageSize: simtest.RAMPage},
		{Name: "disk", Size: diskPages * simtest.PMEMPage, PageSize: simtest.PMEMPage}}
	topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: volumes}}}
	// An arena of fewer pages than one guest's memory, so reading a guest back
	// evicts what it read before: a page the destination published is read
	// again, from wherever its backing sends the read.
	k := knobs.Defaults()
	k.ResidentPages, k.DirtyPages, k.LogicalPages = memoryPages/3, 4*memoryPages, 8*memoryPages
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := sim.WithRuntime(t.Context(), runtime)
	world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: k, Prefix: prefix, Log: t.Logf})

	// A checkpoint, and then a store into every page, so the handoff names every
	// page of both volumes as the source's alone.
	if err := world.StoreAll("vm-1", 1); err != nil {
		t.Fatal(err)
	}
	if err := world.Checkpoint(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if err := world.StoreAll("vm-1", 2); err != nil {
		t.Fatal(err)
	}

	ran := false
	meanwhile := func(ctx context.Context, id string) error {
		ran = true
		if fork {
			// The parent keeps running and storing. None of it reaches the
			// child, whose source is the pause the parent was sealed at.
			if err := world.StoreAll("vm-1", 5); err != nil {
				return err
			}
			// And it is not checkpointed while the point holds its pages,
			// which is what keeps what its page server serves the pause.
			if err := world.Checkpoint(ctx, "vm-1"); !errors.Is(err, volume.ErrSealed) {
				return fmt.Errorf("a checkpoint of the parent while its child runs = %v, want ErrSealed", err)
			}
		}
		// The destination stores into every page it received: half of them a
		// new generation, half of them zeros, which its checkpoint publishes as
		// holes and whose pages the retire hands back at once.
		for name, pages := range map[string]uint64{simtest.MemoryVolume: memoryPages, "disk": diskPages} {
			var ones, zeros []uint64
			for page := range pages {
				if page%2 == 0 {
					ones = append(ones, page)
				} else {
					zeros = append(zeros, page)
				}
			}
			if err := world.StorePages(id, name, ones, 3); err != nil {
				return err
			}
			if err := world.StorePages(id, name, zeros, 0); err != nil {
				return err
			}
		}
		if err := world.Checkpoint(ctx, id); err != nil {
			return err
		}
		// Every page read back, evicting as it goes.
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			return err
		}
		// Every page taken writable again with nothing stored, which for a page
		// the arena gave back reads it in to copy from.
		if err := world.TakeWritable(ctx, id); err != nil {
			return err
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			return err
		}
		if err := world.Checkpoint(ctx, id); err != nil {
			return err
		}
		return world.Verify(ctx, simtest.ReadsMustSucceed)
	}
	id := "vm-1"
	if fork {
		id = "vm-1-a"
		child := simtest.VMSpec{ID: id, Parent: "vm-1", Host: 1, Volumes: volumes}
		err = world.ForkWith(ctx, child, simtest.Handover{Meanwhile: meanwhile})
	} else {
		err = world.MigrateWith(ctx, id, 1, simtest.Handover{Meanwhile: meanwhile})
	}
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatalf("%s was never running on host 1 with its source still serving", id)
	}
	// The window was reached: the destination read a page it had published
	// itself while it was still asking its source for pages.
	if runtime.Probes()[vmmigrate.ProbePublishedSinceHandoff] == 0 {
		t.Fatal("the destination never read a page it had published while its source still served")
	}

	if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
		t.Error(err)
	}
	if err := world.VerifyDurable(ctx, id); err != nil {
		t.Error(err)
	}
	if err := world.CheckSelected(ctx); err != nil {
		t.Error(err)
	}
	if err := world.Close(ctx); err != nil {
		t.Error(err)
	}
	// A migration's destination reclaims only what it published itself, so the
	// source's checkpoints stay behind as a superseded epoch.
	if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
		volume.AllowSupersededEpoch); err != nil {
		t.Error(err)
	}
}
