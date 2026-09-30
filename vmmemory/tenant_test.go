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

// publicBacking is a backing of a VM of tenant created from a public template:
// its untouched pages are the template's.
func (f *fixture) publicBacking(tenant string, pages int) *backing {
	b := f.newBacking(pages)
	b.source = control.Ref{VM: "template-image", Sequence: 1}
	b.owner = control.InTenant(tenant, b.owner)
	return b
}

// A public template's page is the one page every tenant maps. It is loaded
// into the public file, which each region is given as its file 2, and never
// into a tenant's shared file: two tenants' regions map one page of it, and
// neither is given the other's shared file.
func TestEveryTenantMapsAPublicPageFromThePublicFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		a, am := f.attachIn("alpha", f.publicBacking("alpha", 4))
		b, bm := f.attachIn("beta", f.publicBacking("beta", 4))
		for _, reads := range []struct {
			r *vmmemory.MemoryRegion
			m *mapping
		}{{a, am}, {b, bm}} {
			if got := access(t, reads.r, reads.m, 2, false)[0]; got != 3 {
				t.Fatalf("a region reads %d, want the image's 3", got)
			}
		}
		if am.number(2) != publicFile || bm.number(2) != publicFile || am.pages[2].place != bm.pages[2].place {
			t.Fatalf("alpha maps %+v as file %d and beta %+v as file %d, want one page of the public file, file %d",
				am.pages[2], am.number(2), bm.pages[2], bm.number(2), publicFile)
		}
		if am.files[1] == bm.files[1] || am.files[publicFile] != bm.files[publicFile] {
			t.Fatal("the tenants share a shared file, or do not share the public file")
		}
		if s := hostStats(t, f); s.ResidentPages != 1 {
			t.Fatalf("the pager holds %d pages, want the one public page", s.ResidentPages)
		}
	})
}

// A window that reads a public page beside a page the tenant published loads
// each into its own file: the public page into the public file, and the
// tenant's page into the tenant's shared file, which no other tenant is given.
// No page a tenant published ever enters the public file.
func TestATenantsOwnPageNeverEntersThePublicFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 4})
		ab := f.publicBacking("alpha", 4)
		ab.sources = map[uint64]control.Ref{1: {VM: control.InTenant("alpha", "vm-a"), Sequence: 3}}
		a, am := f.attachIn("alpha", ab)
		access(t, a, am, 0, false)
		access(t, a, am, 1, false)
		if am.number(0) != publicFile || am.number(1) != 1 || am.pages[0].file == am.pages[1].file {
			t.Fatalf("alpha maps page 0 as file %d and its own page 1 as file %d, want the public file %d and its shared file 1",
				am.number(0), am.number(1), publicFile)
		}
		if ab.loads != 1 {
			t.Fatalf("the window read the volume %d times, want once for both files", ab.loads)
		}
		bb := f.publicBacking("beta", 4)
		bb.sources = map[uint64]control.Ref{1: {VM: control.InTenant("beta", "vm-b"), Sequence: 3}}
		b, bm := f.attachIn("beta", bb)
		access(t, b, bm, 1, false)
		if bm.number(1) != 1 || bm.pages[1].file == am.pages[1].file {
			t.Fatalf("beta maps its own page 1 %+v as file %d, want its shared file 1 and not alpha's %+v",
				bm.pages[1], bm.number(1), am.pages[1])
		}
	})
}

// Only a template of no tenant is public. A page of a VM of no tenant that is
// no template, or of another tenant's template, fails the fault, and no fork
// point names its pages under a public template's checkpoint.
func TestOnlyAPublicTemplatesPageCrossesTenants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		for _, source := range []string{"vm-plain", "beta/template-image"} {
			b := f.newBacking(4)
			b.source = control.Ref{VM: source, Sequence: 1}
			r, _ := f.attachIn("alpha", b)
			if err := r.Fault(t.Context(), 0, false); !errors.Is(err, vmmemory.ErrOtherTenant) {
				t.Fatalf("a fault on a page of %s = %v, want ErrOtherTenant", source, err)
			}
		}
		parent, pm := f.attachIn("alpha", f.tenantBacking("alpha", 4))
		access(t, parent, pm, 0, true)[0] = 44
		if err := parent.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		public := control.Ref{VM: "template-point", Sequence: 7}
		if err := parent.Checkpoint().Share(t.Context(), public, "v"); !errors.Is(err, vmmemory.ErrOtherTenant) {
			t.Fatalf("naming a fork point's pages under a public checkpoint = %v, want ErrOtherTenant", err)
		}
		if err := parent.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
	})
}

// A public template's page resident in the private file of the region that
// published it — the template's own, which production never runs but a
// simulation does — moves into the public file when a region of a tenant
// inherits it, never into that tenant's shared file. A second tenant then maps
// the same copy.
func TestAPublicPageMovesIntoThePublicFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		tb := f.newBacking(4)
		tb.owner = "template-image"
		template, tm := f.attachIn("", tb)
		access(t, template, tm, 0, true)[0] = 77
		f.mustCheckpoint(template, tb)
		a, am := f.attachIn("alpha", f.inheritor(tb))
		b, bm := f.attachIn("beta", f.inheritor(tb))
		for _, reads := range []struct {
			r *vmmemory.MemoryRegion
			m *mapping
		}{{a, am}, {b, bm}} {
			if got := access(t, reads.r, reads.m, 0, false)[0]; got != 77 {
				t.Fatalf("a tenant's region reads %d, want the template's 77", got)
			}
		}
		if am.number(0) != publicFile || bm.number(0) != publicFile || am.pages[0].place != bm.pages[0].place {
			t.Fatalf("alpha maps %+v as file %d and beta %+v as file %d, want one copy in the public file %d",
				am.pages[0], am.number(0), bm.pages[0], bm.number(0), publicFile)
		}
		if s := hostStats(t, f); s.MovedPages != 1 {
			t.Fatalf("moved %d pages, want the one", s.MovedPages)
		}
	})
}
