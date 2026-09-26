package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// tenantBacking is a backing of a VM of tenant. Its pages hold newBacking's
// bytes under the tenant's own import of them, so two tenants' backings read
// one image under two identities.
func (f *fixture) tenantBacking(tenant string, pages int) *backing {
	b := f.newBacking(pages)
	b.source = control.Ref{VM: control.InTenant(tenant, "image"), Sequence: 1}
	b.owner = control.InTenant(tenant, b.owner)
	return b
}

// attachIn maps one backing as the RAM of a VM of tenant.
func (f *fixture) attachIn(tenant string, b *backing) (*vmmemory.MemoryRegion, *mapping) {
	f.t.Helper()
	return f.attachBacking(vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: b, Tenant: tenant})
}

// A page's identity names its tenant. A memory region whose backing names a
// page of another tenant is refused it: the fault fails and nothing is read.
// So is a fork point that would name its pages under a checkpoint of another
// tenant.
func TestAPageOfAnotherTenantFailsTheFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		b := f.tenantBacking("beta", 4)
		r, _ := f.attachIn("alpha", b)
		if err := r.Fault(t.Context(), 0, false); !errors.Is(err, vmmemory.ErrOtherTenant) {
			t.Fatalf("a fault on a page of another tenant = %v, want ErrOtherTenant", err)
		}
		if b.loads != 0 {
			t.Fatalf("the refused fault read the volume %d times, want none", b.loads)
		}
		parent, pm := f.attachIn("alpha", f.tenantBacking("alpha", 4))
		access(t, parent, pm, 0, true)[0] = 44
		if err := parent.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		point := control.Ref{VM: control.InTenant("beta", "point"), Sequence: 7}
		if err := parent.Checkpoint().Share(t.Context(), point, "v"); !errors.Is(err, vmmemory.ErrOtherTenant) {
			t.Fatalf("naming a fork point's pages under another tenant's checkpoint = %v, want ErrOtherTenant", err)
		}
		if err := parent.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
	})
}

// Two tenants each import one image, so its bytes are the same and its
// identities are each tenant's own. A page two memory regions of one tenant
// inherit is one page of that tenant's shared file, which each maps as its
// file 1. The other tenant's memory region reads the same bytes from a shared
// file of its own, which the first tenant's regions are never given.
func TestEachTenantHasASharedFileOfItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		a, am := f.attachIn("alpha", f.tenantBacking("alpha", 4))
		sibling, sm := f.attachIn("alpha", f.tenantBacking("alpha", 4))
		b, bm := f.attachIn("beta", f.tenantBacking("beta", 4))
		for _, reads := range []struct {
			r *vmmemory.MemoryRegion
			m *mapping
		}{{a, am}, {sibling, sm}, {b, bm}} {
			if got := access(t, reads.r, reads.m, 2, false)[0]; got != 3 {
				t.Fatalf("a region reads %d, want the image's 3", got)
			}
		}
		if am.pages[2].place != sm.pages[2].place || am.number(2) != 1 || sm.number(2) != 1 {
			t.Fatalf("alpha's regions map %+v as file %d and %+v as file %d, want one page of their shared file, file 1",
				am.pages[2], am.number(2), sm.pages[2], sm.number(2))
		}
		if bm.number(2) != 1 || bm.pages[2].file == am.pages[2].file || bm.files[1] == am.files[1] {
			t.Fatalf("beta's region maps %+v as file %d, and was given alpha's file 1 %t; want a shared file of its own",
				bm.pages[2], bm.number(2), bm.files[1] == am.files[1])
		}
		if s := hostStats(t, f); s.ResidentPages != 2 {
			t.Fatalf("the pager holds %d pages, want one for each tenant", s.ResidentPages)
		}
	})
}

// A tenant's shared file outlives the tenant's last memory region while it
// holds an idle page, and the tenant's next region maps that page without
// reading its volume. The file goes back with its last page.
func TestATenantsSharedFileLastsAsLongAsItsPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		first, fm := f.attachIn("alpha", f.tenantBacking("alpha", 4))
		access(t, first, fm, 1, false)
		shared := fm.pages[1].file
		clear(fm.pages)
		if err := first.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.a.files[shared].closed {
			t.Fatal("the tenant's shared file went with its last region, taking the idle page it held")
		}
		nb := f.tenantBacking("alpha", 4)
		next, nm := f.attachIn("alpha", nb)
		if got := access(t, next, nm, 1, false)[0]; got != 2 || nb.loads != 0 || nm.pages[1].file != shared {
			t.Fatalf("the tenant's next region reads %d from file %d after %d volume reads, want the idle 2 from file %d and no read",
				got, nm.pages[1].file, nb.loads, shared)
		}
		clear(nm.pages)
		if err := next.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.h.DropIdle(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !f.a.files[shared].closed {
			t.Fatal("the tenant's shared file outlived its last page")
		}
	})
}
