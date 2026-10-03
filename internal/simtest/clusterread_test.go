package simtest_test

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// clusterTopology is hosts hosts and one VM on the first, with its memory and
// a disk.
func clusterTopology(hosts int) simtest.Topology {
	topology := simtest.Topology{VMs: []simtest.VMSpec{{ID: "vm-0", Host: 0, Volumes: []volume.VolumeSpec{
		{Name: simtest.MemoryVolume, Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
		{Name: simtest.DiskVolume, Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}}}}
	for at := range hosts {
		topology.Hosts = append(topology.Hosts, fmt.Sprintf("host-%d", at))
	}
	return topology
}

// befall does one thing to one host of a world: loses it, drains it out of
// the list of caches, or restarts it over its own disk.
func befall(ctx context.Context, world *simtest.World, event string, host int) error {
	switch event {
	case "lost":
		return world.Kill(ctx, host, sim.CrashProcess)
	case "drained":
		if err := world.Shutdown(ctx, host); err != nil {
			return err
		}
		world.Unlist(ctx, host)
		return nil
	default:
		if err := world.Kill(ctx, host, sim.CrashProcess); err != nil {
			return err
		}
		return world.Restart(ctx, host)
	}
}

// A page in the cluster's cache survives losing a host. A VM is suspended on
// one host of six under 4+2, or of two under 1+1, so its publication fills
// the cluster. Any one host other than the one it is opened on is then lost,
// drained or restarted, the suspending host among them, and the VM opened on
// another host reads every page of its memory and disk from the hosts' disks:
// the one part it reads of the store is the VMM state the restore loads, which
// is no page and which no cache keeps.
func TestAVMOpensFromTheClusterAfterAnyOneHostIsLostDrainedOrRestarted(t *testing.T) {
	for _, hosts := range []int{2, 6} {
		befallen := []int{0}
		if hosts > 2 {
			befallen = []int{0, 2, 3, 4, 5}
		}
		for _, event := range []string{"lost", "drained", "restarted"} {
			for _, host := range befallen {
				t.Run(fmt.Sprintf("%d-hosts/%s/host-%d", hosts, event, host), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						runtime := newCampaignRuntime(1, false)
						ctx := sim.WithRuntime(t.Context(), runtime)
						topology := clusterTopology(hosts)
						world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
							Knobs: campaignKnobs(t, runtime, topology), Prefix: newPrefix(t, "cluster/"), Log: t.Logf,
							ClusterCache: true})
						if err := world.StoreAll("vm-0", 7); err != nil {
							t.Fatal(err)
						}
						if err := world.Suspend(ctx, "vm-0"); err != nil {
							t.Fatal(err)
						}
						if err := world.Settle(ctx); err != nil {
							t.Fatal(err)
						}
						if err := befall(ctx, world, event, host); err != nil {
							t.Fatal(err)
						}
						before := world.PartReads(1)
						if err := world.Start(ctx, "vm-0", 1); err != nil {
							t.Fatal(err)
						}
						if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
							t.Fatalf("the VM opened on host-1 reads back something its guest did not write: %v", err)
						}
						if reads := world.PartReads(1) - before; reads != 1 {
							t.Fatalf("with host-%d %s, host-1 read %d parts from the store to open and fault in the VM, want the VMM state alone",
								host, event, reads)
						}
						if read := world.Host(1).Status().Cache.Read; read.Hits == 0 || read.Misses != 0 {
							t.Fatalf("host-1's reads of the cluster came to %+v, want hits and no miss", read)
						}
						if err := world.Close(ctx); err != nil {
							t.Fatal(err)
						}
					})
				})
			}
		}
	}
}
