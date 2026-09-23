package vmmemory_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

func TestStoredIdentitySharesWithoutLoadingOrComparingBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		a, am, ab := f.region(4)
		b, bm, bb := f.region(4)
		access(t, a, am, 0, false)
		if ab.loads != 1 {
			t.Fatalf("first fault loaded %d times", ab.loads)
		}
		access(t, b, bm, 0, false)
		if bb.loads != 0 {
			t.Fatal("a page resident under the same stored identity was loaded again")
		}
		if am.pages[0].slot != bm.pages[0].slot {
			t.Fatal("equal stored identities did not share one slot")
		}
		// Equal bytes stored elsewhere are a different page: contents are never
		// compared, the storage identity decides.
		other := f.newUnrelatedBacking(4)
		c, cm := f.attach(other)
		access(t, c, cm, 0, false)
		if other.loads != 1 || cm.pages[0].slot == am.pages[0].slot {
			t.Fatal("equal bytes with a different stored identity were shared")
		}
		// A private (unpublished) page never shares, even with equal bytes.
		bb.private[1] = true
		access(t, a, am, 1, false)
		access(t, b, bm, 1, false)
		if bb.loads != 1 || am.pages[1].slot == bm.pages[1].slot {
			t.Fatal("an unpublished overlay page was shared")
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.IdentityHits != 1 || stats.Loads != 4 {
			t.Fatalf("stats: %+v %v", stats, err)
		}
	})
}

func TestZeroPagesNeedNoArenaCapacityOrBackingReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		ab, bb := f.newBacking(4), f.newBacking(4)
		clear(ab.data)
		clear(bb.data)
		for page := range uint64(4) {
			ab.zero[page], bb.zero[page] = true, true
		}
		a, am := f.attach(ab)
		b, bm := f.attach(bb)
		for page := range uint64(4) {
			if access(t, a, am, page, false)[0] != 0 || access(t, b, bm, page, false)[0] != 0 {
				t.Fatalf("page %d was not zero", page)
			}
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.ResidentPages != 0 || stats.Loads != 0 {
			t.Fatalf("stats: %+v %v", stats, err)
		}
		access(t, a, am, 2, true)[0] = 9
		if access(t, b, bm, 2, false)[0] != 0 {
			t.Fatal("private write reached the shared zero page")
		}
	})
}

func TestReadAheadLoadsTheWindowContiguouslyAndNeverEvicts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 8})
		r, m, b := f.region(8)
		var loads [][2]int
		b.onLoad = func(offset uint64, length int) {
			loads = append(loads, [2]int{int(offset) / pageSize, length / pageSize})
		}
		access(t, r, m, 5, false)
		// One fault loads its whole window with one read into consecutive
		// slots and installs it with one mapping command.
		if len(loads) != 1 || loads[0] != [2]int{0, 8} {
			t.Fatalf("read-ahead loads = %v", loads)
		}
		if len(m.pages) != 8 || m.maps != 1 {
			t.Fatalf("mapped %d pages with %d commands; want 8 pages in 1 command", len(m.pages), m.maps)
		}
		for page := uint64(1); page < 8; page++ {
			if m.pages[page].slot != m.pages[page-1].slot+1 {
				t.Fatalf("page %d slot %d does not follow page %d slot %d", page, m.pages[page].slot, page-1, m.pages[page-1].slot)
			}
		}
		for page := range uint64(8) {
			if got := access(t, r, m, page, false)[0]; got != byte(page+1) {
				t.Fatalf("page %d holds %d", page, got)
			}
		}
		// A second region of the same image needs no reads at all.
		s, sm, sb := f.region(8)
		access(t, s, sm, 3, false)
		if sb.loads != 0 || len(sm.pages) != 8 || sm.maps != 1 {
			t.Fatalf("sibling fault: loads=%d mapped=%d commands=%d", sb.loads, len(sm.pages), sm.maps)
		}

		// With the arena full, a fault in a new image loads only its own page
		// by evicting one; read-ahead never evicts.
		other := f.newUnrelatedBacking(8)
		o, om := f.attach(other)
		access(t, o, om, 6, false)
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.Evictions != 1 || other.loads != 1 || other.loadedBytes != pageSize || len(om.pages) != 1 {
			t.Fatalf("fault under pressure: %+v loads=%d bytes=%d mapped=%d %v", stats, other.loads, other.loadedBytes, len(om.pages), err)
		}
	})
}

// A store reads its window ahead on the same terms a read fault does: only
// free arena slots, never an eviction for a page the guest has not asked for.
// With the arena full it brings in its own page alone — evicting for it, as a
// read fault's faulting page does, and once more for the private copy it makes
// — and the guest's next page is a fault of its own rather than a copy of a
// window nobody had room for.
func TestAStoreReadsAheadOnlyIntoFreeSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 8})
		r, m, _ := f.region(8)
		access(t, r, m, 0, false) // the arena is this image's whole window
		other := f.newUnrelatedBacking(8)
		o, om := f.attach(other)
		before, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		access(t, o, om, 6, true)[0] = 99
		after, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if other.loads != 1 || other.loadedBytes != pageSize {
			t.Fatalf("a store under pressure made %d loads of %d bytes, want one of its own page",
				other.loads, other.loadedBytes)
		}
		if got := after.Evictions - before.Evictions; got != 2 {
			t.Fatalf("the store evicted %d pages, want the one its origin took and the one its copy took", got)
		}
		if len(om.pages) != 1 || !om.pages[6].writable {
			t.Fatalf("the store mapped %d pages (%v), want its own alone and writable", len(om.pages), om.pages)
		}
		if got := access(t, o, om, 6, false)[0]; got != 99 {
			t.Fatalf("the page the guest stored into holds %d, want 99", got)
		}
	})
}

// The populate maps the resident runs that are worth a mapping command each —
// at least one read-ahead window long — and leaves the shorter ones to the
// faults that would cost the same command and only for the pages the guest
// reads. A run is the pages consecutive in both the region and the arena, which
// is neither the window nor the order they were read in: pages 0 to 7 are two
// such runs and are installed, while pages 8 and 9 are a run of two and the
// sibling's own page 10 leaves page 11 a run of one, and neither is.
func TestPopulateMapsEveryResidentRunWorthItsCommandBeforeTheMachineRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 64, DirtyPages: 16, ReadAheadPages: 4})
		a, am, _ := f.region(12)
		access(t, a, am, 0, false) // window 0-3
		access(t, a, am, 9, false) // window 8-11
		// The store takes a private page of its own, which nothing may share;
		// the page it copied away from stays in the sharing index under the
		// identity the volume gives it, and that is what the sibling maps. The
		// store brings its own window in as a read fault does, so pages 4, 6
		// and 7 are shared and populated too.
		access(t, a, am, 5, true)[0] = 7
		bb := f.newBacking(12)
		bb.private[10] = true // the sibling changed this page itself
		b, bm := f.attach(bb)
		want := map[uint64]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true,
			6: true, 7: true}
		for page := range uint64(12) {
			_, mapped := bm.pages[page]
			if mapped != want[page] {
				t.Fatalf("page %d mapped=%t after populate", page, mapped)
			}
			if !mapped {
				continue
			}
			if shared := bm.pages[page].slot == am.pages[page].slot; shared == (page == 5) {
				t.Fatalf("page %d populated from slot %d against the writer's %d",
					page, bm.pages[page].slot, am.pages[page].slot)
			}
		}
		if bb.loads != 0 || bm.maps != 2 {
			t.Fatalf("populate loaded %d times and issued %d commands; want 0 loads, 2 range commands", bb.loads, bm.maps)
		}
		for page := range uint64(12) {
			if got := access(t, b, bm, page, false)[0]; got != byte(page+1) {
				t.Fatalf("page %d holds %d", page, got)
			}
		}
		// Eight hits at the populate, and three more from the fault that mapped
		// the short runs it left behind when the guest reached them.
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.IdentityHits != 11 {
			t.Fatalf("stats: %+v %v", stats, err)
		}
	})
}

func TestFaultsInDifferentWindowsProceedConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		r, m, b := f.region(4)
		entered, release := make(chan struct{}), make(chan struct{})
		b.onLoad = func(offset uint64, _ int) {
			if offset == 0 {
				close(entered)
				<-release
			}
		}
		slow := make(chan error, 1)
		go func() { slow <- r.Fault(t.Context(), 0, false) }()
		<-entered
		fast := make(chan error, 1)
		go func() { fast <- r.Fault(t.Context(), 1, false) }()
		synctest.Wait()
		select {
		case err := <-fast:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("a fault in another window waited behind a stalled load")
		}
		// A seal of the region runs straight through the in-flight fault: the
		// region is held for planning and page-table work, never for a load.
		sealed := make(chan error, 1)
		go func() { sealed <- r.Seal(t.Context()) }()
		synctest.Wait()
		var sealErr error
		blocked := false
		select {
		case sealErr = <-sealed:
		default:
			blocked = true
		}
		close(release)
		if blocked {
			t.Fatal("the seal waited for a fault's backing load")
		}
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		if err := <-slow; err != nil {
			t.Fatal(err)
		}
		if err := r.Unseal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if m.pages[0].slot == m.pages[1].slot {
			t.Fatal("distinct pages shared a slot")
		}
	})
}

func TestFaultPastTheRegionIsRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 4, 2)
		r, _, _ := f.region(2)
		if err := r.Fault(t.Context(), 2, false); !errors.Is(err, vmmemory.ErrRange) {
			t.Fatalf("fault past the region: %v", err)
		}
	})
}

// Read-ahead is one host policy: every region of a host loads the same aligned
// run, and no region chooses its own.
func TestReadAheadFollowsTheHostPolicy(t *testing.T) {
	for _, pages := range []int{1, 4, 8} {
		t.Run(fmt.Sprint(pages), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 32, LogicalPages: 64, DirtyPages: 16, ReadAheadPages: pages})
				b := f.newUnrelatedBacking(8)
				r, m := f.attach(b)
				access(t, r, m, 3, false)
				if b.loadedBytes != pages*pageSize || len(m.pages) != pages {
					t.Fatalf("host policy %d loaded %d bytes and mapped %d pages; want %d pages", pages, b.loadedBytes, len(m.pages), pages)
				}
			})
		})
	}
}

// pagerCluster is one simulated host: an object store and the manager a pager's
// volumes come from.
type pagerCluster struct {
	runtime *sim.Runtime
	manager *volume.Manager
}

func newPagerCluster(t *testing.T) *pagerCluster { return newConfiguredCluster(t, nil) }

func newConfiguredCluster(t *testing.T, adjust func(*volume.Config)) *pagerCluster {
	t.Helper()
	runtime := sim.New(sim.Config{})
	prefix, err := platform.NewObjectPrefix("sproutfs/")
	if err != nil {
		t.Fatal(err)
	}
	client, err := control.NewClient(control.Config{ObjectStore: runtime.ObjectStore(), ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: runtime.ObjectStore(), ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing publishes on its own, so a test decides exactly when a checkpoint
	// happens.
	config := volume.Config{Control: client, Store: store}
	if adjust != nil {
		adjust(&config)
	}
	manager, err := volume.NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return &pagerCluster{runtime: runtime, manager: manager}
}

func (c *pagerCluster) create(t *testing.T, id string, pages int) *volume.VM {
	t.Helper()
	vm, err := c.manager.Create(t.Context(), id, []volume.VolumeSpec{{Name: "ram0", Size: uint64(pages * pageSize), PageSize: uint64(pageSize)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Close(context.Background()) })
	return vm
}

// Two VMs forked at one pause inherit the same page identities, so the
// second maps the first's resident pages without reading its backing at all.
func TestForksShareTheirPointsResidentPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPagerCluster(t)
		source := c.create(t, "source", 4)
		// One marker per page: a page is published whole, so a byte of it is
		// enough to make the whole page the checkpoint's.
		for _, page := range []uint64{1, 2} {
			if err := source.Volume("ram0").Write(t.Context(), page*uint64(pageSize), []byte{37}); err != nil {
				t.Fatal(err)
			}
		}
		if err := source.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := source.ForkPoint(t.Context(), volume.Prepared([]byte("vmm"), nil))
		if err != nil {
			t.Fatal(err)
		}
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		fork := func(id string, from *volume.ForkPoint) (*volume.VM, *vmmemory.Region, *mapping) {
			vm, err := c.manager.Fork(t.Context(), id, from)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = vm.Close(context.Background()) })
			r, m := f.attach(vm.Volume("ram0"))
			return vm, r, m
		}
		_, a, am := fork("a", point)
		bv, b, bm := fork("b", point)
		for _, page := range []uint64{1, 2} {
			if access(t, a, am, page, false)[0] != 37 {
				t.Fatal("first fork lost the captured tail")
			}
		}
		before, _ := f.h.Stats(t.Context())
		for _, page := range []uint64{1, 2} {
			if access(t, b, bm, page, false)[0] != 37 || am.pages[page].slot != bm.pages[page].slot {
				t.Errorf("checkpoint page %d did not share its resident page", page)
			}
		}
		after, _ := f.h.Stats(t.Context())
		if after.IdentityHits-before.IdentityHits != 2 || after.Loads != before.Loads {
			t.Errorf("sibling tail: identity hits=%d loads=%d; want 2 hits, 0 loads",
				after.IdentityHits-before.IdentityHits, after.Loads-before.Loads)
		}
		if point.Parent() != (control.Ref{VM: "source", Sequence: control.Sequence(source.Epoch(), 2)}) ||
			string(point.State()) != "vmm" {
			t.Fatalf("point = %v state=%q", point.Parent(), point.State())
		}
		// A private write breaks sharing; a remaining inherited page preserves
		// its identity across close and recovery of the destination log.
		access(t, b, bm, 1, true)[0] = 81
		if err := checkpointVolume(t, bv, "ram0", b); err != nil {
			t.Fatal(err)
		}
		clear(bm.pages)
		if err := b.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := bv.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		bv, err = c.manager.Open(t.Context(), "b")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bv.Close(context.Background()) })
		b, bm = f.attach(bv.Volume("ram0"))
		if access(t, b, bm, 1, false)[0] != 81 || access(t, a, am, 1, false)[0] != 37 {
			t.Fatal("private destination write changed the captured bytes")
		}
		if access(t, b, bm, 2, false)[0] != 37 || am.pages[2].slot != bm.pages[2].slot {
			t.Error("recovered inherited page lost its stable identity")
		}
		// A nested fork eagerly inherits its parent's and grandparent's pages.
		nested, err := bv.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		before, _ = f.h.Stats(t.Context())
		_, cr, cm := fork("c", nested)
		after, _ = f.h.Stats(t.Context())
		if after.Loads != before.Loads || after.IdentityHits-before.IdentityHits != 2 {
			t.Fatalf("nested attach: loads=%d hits=%d; want 0 and 2", after.Loads-before.Loads, after.IdentityHits-before.IdentityHits)
		}
		if cm.pages[1].slot != bm.pages[1].slot || cm.pages[2].slot != am.pages[2].slot {
			t.Fatal("nested fork did not eagerly inherit parent and grandparent pages")
		}
		if access(t, cr, cm, 1, false)[0] != 81 || access(t, cr, cm, 2, false)[0] != 37 {
			t.Fatal("nested fork lost inherited bytes")
		}
		// A later fork of the same point inherits the same pages.
		before, _ = f.h.Stats(t.Context())
		_, d, dm := fork("materialized", point)
		after, _ = f.h.Stats(t.Context())
		if after.Loads != before.Loads || after.IdentityHits-before.IdentityHits != 2 ||
			dm.pages[1].slot != am.pages[1].slot || dm.pages[2].slot != am.pages[2].slot {
			t.Fatalf("the fork point lost its resident pages: before=%+v after=%+v", before, after)
		}
		if access(t, d, dm, 1, false)[0] != 37 {
			t.Fatal("a later fork of the point lost captured bytes")
		}
	})
}

// A fork point carries the pages the parent holds that no checkpoint has, and
// on the parent's host the child maps those pages instead of reading them: the
// point names every one of them with an identity nothing else ever claims, so
// the pager shares them for as long as the seal lasts. When the seal ends the
// pages are the guest's own dirty state again, so nothing may still be reading
// them: a child that has published the pages as its own reads them from there.
func TestForkPointSharesThePagesItSealed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPagerCluster(t)
		parent := c.create(t, "parent", 4)
		if err := parent.Volume("ram0").Write(t.Context(), uint64(pageSize), bytes.Repeat([]byte{37}, pageSize)); err != nil {
			t.Fatal(err)
		}
		if err := parent.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		pr, pm := f.attach(parent.Volume("ram0"))
		// The guest stores after that checkpoint, so page 1 is the parent's own
		// dirty state: no volume holds those bytes, only the page does.
		access(t, pr, pm, 1, true)[0] = 91
		if err := pr.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := parent.ForkPoint(t.Context(), volume.Prepared(nil,
			map[string]volume.DirtySource{"ram0": pr.Checkpoint()}))
		if err != nil {
			t.Fatal(err)
		}
		child, err := c.manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.Close(context.Background()) })
		// Offering the pages is what a host taking the child in does before the
		// child's regions attach: it is the whole of the local backing's attach.
		if err := point.Share(t.Context()); err != nil {
			t.Fatal(err)
		}
		before, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		cr, cm := f.attach(child.Volume("ram0"))
		if got := access(t, cr, cm, 1, false)[0]; got != 91 {
			t.Fatalf("the child read %d from the page the point sealed, want 91", got)
		}
		after, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.Loads != before.Loads || after.IdentityHits-before.IdentityHits != 1 ||
			cm.pages[1].slot != pm.pages[1].slot {
			t.Fatalf("the child read the sealed page instead of mapping the parent's copy: before=%+v after=%+v", before, after)
		}
		// The child's first checkpoint publishes the inherited page as its own
		// and retires the point, which hands the page back to the parent's
		// guest as dirty state.
		if err := child.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		access(t, pr, pm, 1, true)[0] = 55
		if got := access(t, cr, cm, 1, false)[0]; got != 91 {
			t.Fatalf("the parent's store after the point retired reached the child: %d", got)
		}
	})
}

// A page a fork point shared is still the parent's private dirty state, so
// evicting it spills through the reservation the seal holds, however many
// machines were mapping it. Each of them reads the page through its own backing
// afterwards, which reaches those same bytes through the seal.
func TestSharingASealedPageLeavesItSpilledByItsSeal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPagerCluster(t)
		parent := c.create(t, "parent", 4)
		for page := range uint64(4) {
			if err := parent.Volume("ram0").Write(t.Context(), page*uint64(pageSize),
				bytes.Repeat([]byte{byte(page + 1)}, pageSize)); err != nil {
				t.Fatal(err)
			}
		}
		if err := parent.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Three pages: the parent's sealed page and two of the child's leave
		// the next fault of the child nothing free.
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 3, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		pr, pm := f.attach(parent.Volume("ram0"))
		access(t, pr, pm, 1, true)[0] = 91
		if err := pr.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := parent.ForkPoint(t.Context(), volume.Prepared(nil,
			map[string]volume.DirtySource{"ram0": pr.Checkpoint()}))
		if err != nil {
			t.Fatal(err)
		}
		child, err := c.manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.Close(context.Background()) })
		cr, cm := f.attach(child.Volume("ram0"))
		if access(t, cr, cm, 1, false)[0] != 91 {
			t.Fatal("the child did not read the sealed page")
		}
		for _, page := range []uint64{2, 3, 0} {
			if got := access(t, cr, cm, page, false)[0]; got != byte(page+1) {
				t.Fatalf("the child read %d from page %d", got, page)
			}
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.Spills != 1 || stats.Evictions == 0 {
			t.Fatalf("the shared sealed page was not spilled by its seal: %+v %v", stats, err)
		}
		// Both sides read the page out of that spill: the child through the
		// point it forked at, the parent as the dirty state it never gave up.
		if got := access(t, cr, cm, 1, false)[0]; got != 91 {
			t.Fatalf("the child read %d from the evicted sealed page, want 91", got)
		}
		if got := access(t, pr, pm, 1, false)[0]; got != 91 {
			t.Fatalf("the parent read %d from the evicted sealed page, want 91", got)
		}
	})
}

// The pager keeps a VM's own writes private to it: a checkpoint that is still
// publishing owns the sequence it froze, so the writes that continue during it
// can never be mistaken for the frozen bytes.
func TestWritesDuringPublicationNeverAliasTheCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPagerCluster(t)
		source := c.create(t, "source", 4)
		if err := source.Volume("ram0").Write(t.Context(), 0, bytes.Repeat([]byte{11}, pageSize)); err != nil {
			t.Fatal(err)
		}
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		r, m := f.attach(source.Volume("ram0"))
		if got := access(t, r, m, 0, false)[0]; got != 11 {
			t.Fatalf("page 0 = %d", got)
		}
		locate := func(v *volume.Volume) control.Identity {
			extents, err := v.Locate(t.Context(), 0, uint64(pageSize))
			if err != nil || len(extents) != 1 {
				t.Fatalf("locate: %v %v", extents, err)
			}
			return extents[0].Identity
		}
		frozen := locate(source.Volume("ram0"))
		point, err := source.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		access(t, r, m, 0, true)[0] = 22
		// The fork point took the identity this page had; a write through the
		// volume itself, which is what image building does, belongs to the
		// checkpoint after it.
		if err := source.Volume("ram0").Write(t.Context(), 0, bytes.Repeat([]byte{22}, pageSize)); err != nil {
			t.Fatal(err)
		}
		if written := locate(source.Volume("ram0")); written == frozen {
			t.Fatalf("a write after the fork reported the point's identity %v", written)
		}
		fork, err := c.manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fork.Close(context.Background()) })
		fr, fm := f.attach(fork.Volume("ram0"))
		if got := access(t, fr, fm, 0, false)[0]; got != 11 {
			t.Fatalf("fork of the checkpoint read the source's later write: %d", got)
		}
		if got := access(t, r, m, 0, false)[0]; got != 22 {
			t.Fatalf("source lost its own write: %d", got)
		}
		if fm.pages[0].slot == m.pages[0].slot {
			t.Fatal("two different contents shared one page")
		}
	})
}

// A published page no region maps any more is still the page its identity
// names. It stays in the arena, idle, and the next region that inherits the
// identity maps it without reading the volume. An idle page is the first thing
// an allocation short of a slot gives up, before any page a region maps.
func TestAPublishedPageOutlivesTheLastRegionThatMappedIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		a, am, _ := f.region(4)
		access(t, a, am, 0, false)
		access(t, a, am, 1, false)
		clear(am.pages) // its VMM is gone
		if err := a.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.ResidentPages != 2 || stats.IdlePages != 2 {
			t.Fatalf("after the last region detached: %+v %v", stats, err)
		}
		// A region of other pages needs a slot of the full arena: the older idle
		// page goes, and nothing is evicted.
		other := f.newUnrelatedBacking(4)
		c, cm := f.attach(other)
		access(t, c, cm, 0, false)
		stats, err = f.h.Stats(t.Context())
		if err != nil || stats.IdleDrops != 1 || stats.Evictions != 0 || stats.IdlePages != 1 {
			t.Fatalf("a slot for another region's page: %+v %v", stats, err)
		}
		// A region inheriting a's identities maps the idle page 1 without a read,
		// and reads page 0 again, which was given up.
		b, bm, bb := f.region(4)
		access(t, b, bm, 1, false)
		if bb.loads != 0 {
			t.Fatalf("the idle page was loaded again %d times", bb.loads)
		}
		access(t, b, bm, 0, false)
		if bb.loads != 1 {
			t.Fatalf("the page given up was loaded %d times, want once", bb.loads)
		}
	})
}
