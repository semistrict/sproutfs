package simtest_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// pairTopology is two hosts and one VM with its memory and a disk, which
// is the smallest deployment the cluster cache serves.
func pairTopology() simtest.Topology {
	return simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{{ID: "vm-0", Host: 0, Volumes: []volume.VolumeSpec{
			{Name: simtest.MemoryVolume, Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
			{Name: simtest.DiskVolume, Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}}}}
}

// Two hosts under 1+1 fill each other: a VM suspended on one host is opened
// on the other, which reads every page of it from its own disk, where the
// suspending host's publication put a whole copy of each window. The copy came
// as a keep the second host took. The one part it reads of the store is the
// VMM state the restore loads, which is no page and which no cache keeps.
func TestOnTwoHostsAVMOpenedOnTheOtherHostReadsItsPagesFromThatHostsDisk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(1, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		topology := pairTopology()
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: campaignKnobs(t, runtime, topology), Prefix: newPrefix(t, "pair/"), Log: t.Logf,
			ClusterCache: true})
		if held := world.Host(1).Membership(); held.List().Len() != 2 || held.Code().String() != "1+1" ||
			len(held.Members()) != 2 {
			t.Fatalf("host-1 holds a membership of %d disks under %s, want both hosts under 1+1",
				held.List().Len(), held.Code())
		}
		if err := world.StoreAll("vm-0", 9); err != nil {
			t.Fatal(err)
		}
		if err := world.Suspend(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if fill := world.Host(1).Status().Cache.Fill; fill.Kept == 0 || fill.Refused != 0 {
			t.Fatalf("host-1's cache took %+v from host-0's fills, want the windows it was sent kept", fill)
		}
		before := world.PartReads(1)
		if err := world.Start(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("the VM opened on host-1 reads back something its guest did not write: %v", err)
		}
		if reads := world.PartReads(1) - before; reads != 1 {
			t.Fatalf("host-1 read %d parts from the store to open and fault in a VM whose windows its disk holds, want the VMM state alone",
				reads)
		}
		if disk := world.Host(1).Status().Cache.Disk; disk.Hits == 0 || disk.Lost != 0 {
			t.Fatalf("host-1's disk served %+v, want its hits", disk)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
