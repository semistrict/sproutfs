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
		within, public, across := world.Sharing()
		if len(across) != 0 || public != 0 {
			t.Fatalf("pages crossed between tenants, %d of them public:\n%v", public, across)
		}
		// Every page is shared, the one only the fork point held included: the
		// template published the point once, and its children name it.
		for _, tenant := range tenants {
			if want := memoryPages + diskPages; within[tenant] != want {
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

// TestTwoTenantsCreatingFromAPublicTemplateShareOnlyItsPages: a template of no
// tenant is public. It keeps a checkpoint of its memory and stops, and three
// VMs of two tenants are created from that checkpoint on one host. Each reads
// the template's image, and the guests of both tenants map one page of the
// public file for every page the template published. They then write, and
// what they write crosses no tenant: in an isolated arena no other file is
// mapped by both tenants.
func TestTwoTenantsCreatingFromAPublicTemplateShareOnlyItsPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(37, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		const memoryPages = 4
		volumes := []volume.VolumeSpec{
			{Name: simtest.MemoryVolume, Size: memoryPages * simtest.RAMPage, PageSize: simtest.RAMPage}}
		topology := simtest.Topology{Hosts: []string{"host-0"}, VMs: []simtest.VMSpec{
			{ID: "template-image", Host: 0, Volumes: volumes},
			{ID: "alpha/child-a", Host: 0, Parent: "template-image", Kept: true, Volumes: volumes},
			{ID: "alpha/child-b", Host: 0, Parent: "template-image", Kept: true, Volumes: volumes},
			{ID: "beta/child-a", Host: 0, Parent: "template-image", Kept: true, Volumes: volumes},
		}}
		k := knobs.Defaults()
		k.ResidentPages, k.DirtyPages, k.LogicalPages = 64, 64, 256
		k.ReadAheadPages, k.WriteAheadPages = 1, 1
		if err := k.Validate(); err != nil {
			t.Fatal(err)
		}
		prefix := newPrefix(t, "public/")
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: k, Prefix: prefix, Log: t.Logf})

		if err := world.StoreAll("template-image", 0x5c); err != nil {
			t.Fatal(err)
		}
		if err := world.Keep(ctx, "template-image", false); err != nil {
			t.Fatal(err)
		}
		if err := world.Stop(ctx, "template-image"); err != nil {
			t.Fatal(err)
		}
		kept, _ := world.KeptOf(ctx, "template-image")
		if len(kept) != 1 {
			t.Fatalf("the template keeps %v, want the one checkpoint it asked to keep", kept)
		}
		for _, child := range topology.VMs[1:] {
			if err := world.CreateFromKept(ctx, child, kept[0], false); err != nil {
				t.Fatalf("creating %s from the public template: %v", child.ID, err)
			}
			if world.HostOf(child.ID) != 0 {
				t.Fatalf("%s was not created", child.ID)
			}
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		within, public, across := world.Sharing()
		if len(across) != 0 {
			t.Fatalf("pages other than the public template's crossed between tenants:\n%v", across)
		}
		// Every page the guests share is the template's, so all of it is counted
		// as public and none as shared within alpha.
		if public != memoryPages || within["alpha"] != 0 {
			t.Fatalf("the tenants share %d public pages and alpha's guests %d other pages, want the template's %d and none",
				public, within["alpha"], memoryPages)
		}

		for value, child := range topology.VMs[1:] {
			if err := world.StorePages(child.ID, simtest.MemoryVolume, []uint64{1}, byte(0x70+value)); err != nil {
				t.Fatal(err)
			}
			if err := world.Checkpoint(ctx, child.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if _, _, across := world.Sharing(); len(across) != 0 {
			t.Fatalf("what the children wrote crossed between tenants:\n%v", across)
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Error(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := volume.CheckDeployment(ctx, runtime.ObjectStore(), prefix); err != nil {
			t.Fatal(err)
		}
	})
}
