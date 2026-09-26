package simtest_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// TestTwoTenantsForkingOneImageShareNoPage: two tenants each hold a template
// of one image on one host. The bytes are the same, and each tenant published
// them under checkpoints of its own. Each template also holds a page stored
// since, which only its fork point holds. Each forks two children on that
// host, which map what their template published and what its fork point lent
// them.
//
// A tenant's guests share its template's pages. No page one tenant's guests
// map is a page the other tenant's guests map, and in an isolated arena no
// file is either: a VMM of one tenant is never given the other's pages.
func TestTwoTenantsForkingOneImageShareNoPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 31,
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
			{Name: simtest.MemoryVolume, Size: memoryPages * simtest.RAMPage, PageSize: simtest.RAMPage},
			{Name: "disk", Size: diskPages * simtest.PMEMPage, PageSize: simtest.PMEMPage}}
		tenants := []string{"alpha", "beta"}
		topology := simtest.Topology{Hosts: []string{"host-0"}}
		for _, tenant := range tenants {
			topology.VMs = append(topology.VMs,
				simtest.VMSpec{ID: control.InTenant(tenant, "template"), Host: 0, Volumes: volumes})
		}
		k := knobs.Defaults()
		k.ResidentPages, k.DirtyPages, k.LogicalPages = 64, 64, 256
		k.ReadAheadPages, k.WriteAheadPages = 1, 1
		if err := k.Validate(); err != nil {
			t.Fatal(err)
		}
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: k, Prefix: prefix, Log: t.Logf})

		for _, tenant := range tenants {
			template := control.InTenant(tenant, "template")
			// The image, which each tenant imports on its own.
			if err := world.StoreAll(template, 0x5c); err != nil {
				t.Fatal(err)
			}
			if err := world.Checkpoint(ctx, template); err != nil {
				t.Fatal(err)
			}
			// A page stored since, which only the fork point holds.
			if err := world.StorePages(template, simtest.MemoryVolume, []uint64{1}, 0x3a); err != nil {
				t.Fatal(err)
			}
			children := []simtest.VMSpec{
				{ID: control.InTenant(tenant, "child-a"), Parent: template, Host: 0, Volumes: volumes},
				{ID: control.InTenant(tenant, "child-b"), Parent: template, Host: 0, Volumes: volumes},
			}
			if err := world.FanOut(ctx, template, children); err != nil {
				t.Fatal(err)
			}
			for _, child := range children {
				if host := world.HostOf(child.ID); host != 0 {
					t.Fatalf("%s runs on host %d, want host 0 beside its template", child.ID, host)
				}
			}
		}

		// Every guest reads every page it has, which maps it.
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		within, across := world.Sharing()
		if len(across) != 0 {
			t.Fatalf("pages crossed between tenants:\n%v", across)
		}
		for _, tenant := range tenants {
			if want := memoryPages - 1 + diskPages; within[tenant] != want {
				t.Fatalf("%s's guests share %d pages, want the %d their template published", tenant, within[tenant], want)
			}
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Error(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Error(err)
		}
	})
}
