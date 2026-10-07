package vmmemory_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// revokeHookedMapping is a test mapping that runs a hook before each
// revocation, which is where a test puts a child's fault while its parent's
// pager holds a page the child maps.
type revokeHookedMapping struct {
	*mapping
	onRevoke func(page uint64)
}

func (m *revokeHookedMapping) Revoke(ctx context.Context, page uint64) error {
	if m.onRevoke != nil {
		m.onRevoke(page)
	}
	return m.mapping.Revoke(ctx, page)
}

// attachRevokeHooked maps one backing as guest RAM through a
// revokeHookedMapping.
func (f *fixture) attachRevokeHooked(b vmmemory.Backing) (*vmmemory.MemoryRegion, *revokeHookedMapping) {
	f.t.Helper()
	m := &revokeHookedMapping{mapping: newMapping(f.a)}
	f.a.mappings = append(f.a.mappings, m.mapping)
	r, err := f.h.Attach(f.ctx, ram(b), m)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		clear(m.pages)
		if err := r.Detach(context.Background()); err != nil {
			f.t.Error(err)
		}
	})
	return r, m
}

// lendingParent is a parent memory region of two pages, 44 and 55 stored into
// their first bytes, sealed, with a fork point lending the sealed pages under
// point. Its child's backing reads the bytes the point gave those pages, as
// a child's volume does once the point is published.
func lendingParent(t *testing.T, f *fixture) (parent *vmmemory.MemoryRegion, pm *mapping, pb *backing,
	child *backing) {
	t.Helper()
	parent, pm, pb = f.memoryRegion(2)
	access(t, parent, pm, 0, true)[0] = 44
	access(t, parent, pm, 1, true)[0] = 55
	if err := parent.Seal(f.ctx); err != nil {
		t.Fatal(err)
	}
	child = f.newBacking(2)
	child.source = control.Ref{VM: f.source.VM + "-point", Sequence: 7}
	child.data[0], child.data[f.pageSize] = 44, 55
	return parent, pm, pb, child
}

// share lends the parent's sealed pages under the child's point.
func share(t *testing.T, f *fixture, parent *vmmemory.MemoryRegion, child *backing) {
	t.Helper()
	if err := parent.Checkpoint().Share(f.ctx, child.source, "v"); err != nil {
		t.Fatal(err)
	}
}

// pointChild is the backing of another child of the point child inherits
// from, which reads what child reads.
func pointChild(f *fixture, child *backing) *backing {
	sibling := f.newBacking(len(child.data) / f.pageSize)
	sibling.source = child.source
	copy(sibling.data, child.data)
	return sibling
}

// A retire that gives back a page a fork point lends takes the point's name
// for it away under the page's lock, so a child that faults on the page while
// the retire goes on reads it through its own volume. Before 2026-10-07 the
// name stayed until the seal ended, and a child that faulted on it in between
// mapped the slot the retire had just given back.
func TestAChildFaultingDuringItsParentsRetireMapsNoPageTheRetireGaveBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 32, DirtyPages: 8,
			ReadAheadPages: 1})
		vmmemory.SetCheckpointBatchPages(t, 1)
		parent, pm, pb, cb := lendingParent(t, f)
		child, cm := f.attach(cb)
		share(t, f, parent, cb)
		// The guest stores into page 0 again: the checkpoint alone holds the
		// page the point lends, and the retire gives it back.
		access(t, parent, pm, 0, true)[0] = 46
		published, err := f.publishCheckpoint(f.ctx, parent, pb)
		if err != nil {
			t.Fatal(err)
		}
		var faulted error
		entered := false
		pb.onLocate = func(offset, _ uint64) {
			if offset != uint64(f.pageSize) || entered {
				return
			}
			// The retire's second batch looks its page up with nothing held,
			// after the first gave page 0 back.
			entered = true
			faulted = child.Fault(f.ctx, 0, false)
		}
		if err := parent.Checkpoint().Retire(f.ctx, published); err != nil {
			t.Fatal(err)
		}
		if !entered || faulted != nil {
			t.Fatalf("the child's fault during the retire ran %t and returned %v, want it run and served", entered, faulted)
		}
		if got := access(t, child, cm, 0, false)[0]; got != 44 {
			t.Fatalf("the child reads %d at page 0, want the 44 the point gave it", got)
		}
		if got := access(t, parent, pm, 0, false)[0]; got != 46 {
			t.Fatalf("the parent reads %d at page 0, want its own 46", got)
		}
	})
}

// An unseal that hands a page a fork point lends back to its guest takes the
// point's name for it away under the page's lock, so a child that faults on
// the page while the unseal goes on reads it through its own volume and never
// sees what the parent stores there next. Before 2026-10-07 the name stayed
// until the seal ended, and the child mapped the parent's own dirty page.
func TestAChildFaultingDuringItsParentsUnsealNeverSeesThePagesParentStoresNext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The child maps the parent's own page, which is what a shared arena does.
		f := newPinnedFixture(t, vmmemory.Config{Arena: vmmemory.ArenaShared, ResidentPages: 16, LogicalPages: 32,
			DirtyPages: 8, ReadAheadPages: 1})
		parent, pm, _, cb := lendingParent(t, f)
		child, cm := f.attachRevokeHooked(cb)
		share(t, f, parent, cb)
		for page, want := range []byte{44, 55} {
			if got := access(t, child, cm.mapping, uint64(page), false)[0]; got != want {
				t.Fatalf("the child reads %d at page %d, want the %d the point lends", got, page, want)
			}
		}
		faulted := make(chan error, 1)
		cm.onRevoke = func(page uint64) {
			switch page {
			case 0:
				// The unseal takes page 0 from the child, holding it: the
				// child faults on it again at once, and waits for it.
				go func() { faulted <- child.Fault(f.ctx, 0, false) }()
			case 1:
				// The unseal has given page 0 back to the parent and holds
				// page 1: the child's fault on page 0 goes on to its end.
				synctest.Wait()
			}
		}
		if err := parent.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		cm.onRevoke = nil
		if err := <-faulted; err != nil {
			t.Fatal(err)
		}
		access(t, parent, pm, 0, true)[0] = 99
		if got := access(t, child, cm.mapping, 0, false)[0]; got != 44 {
			t.Fatalf("the child reads %d at page 0, want the 44 the point gave it", got)
		}
	})
}

// A detach that drops a page a fork point lends takes the point's name for it
// away under the page's lock, so a child that faults on the page while the
// detach goes on reads it through its own volume. Before 2026-10-07 the name
// stayed until the seal ended, the child mapped the page again, and the
// detach's layer gave back a page the child mapped: the pager panicked.
func TestAChildFaultingDuringItsParentsDetachMapsNoPageTheDetachDrops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPinnedFixture(t, vmmemory.Config{Arena: vmmemory.ArenaShared, ResidentPages: 16, LogicalPages: 32,
			DirtyPages: 8, ReadAheadPages: 1})
		parent, pm, _, cb := lendingParent(t, f)
		child, cm := f.attachRevokeHooked(cb)
		share(t, f, parent, cb)
		for page := range uint64(2) {
			access(t, child, cm.mapping, page, false)
		}
		faulted := make(chan error, 1)
		cm.onRevoke = func(page uint64) {
			switch page {
			case 0:
				go func() { faulted <- child.Fault(f.ctx, 0, false) }()
			case 1:
				synctest.Wait()
			}
		}
		clear(pm.pages)
		if err := parent.Detach(f.ctx); err != nil {
			t.Fatal(err)
		}
		cm.onRevoke = nil
		if err := <-faulted; err != nil {
			t.Fatal(err)
		}
		if got := access(t, child, cm.mapping, 0, false)[0]; got != 44 {
			t.Fatalf("the child reads %d at page 0, want the 44 the point gave it", got)
		}
	})
}

// The end of a fork point's seal leaves a page a child read under the point's
// name where it is: in an arena that shares it, that is the point's root,
// which stays the root of the name, and the next child maps the page without
// a read. Before 2026-10-07 the end destroyed a root the point had not
// published, with every page in it, and the pager panicked freeing the page
// the child mapped.
func TestTheEndOfASealLeavesThePagesAChildReadUnderItsPointsName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPinnedFixture(t, vmmemory.Config{Arena: vmmemory.ArenaShared, ResidentPages: 16, LogicalPages: 32,
			DirtyPages: 8, ReadAheadPages: 1})
		parent, _, _, lent := lendingParent(t, f)
		share(t, f, parent, lent)
		// The child's page 2 is under the point's name too, and the parent,
		// two pages long, lends nothing there: the child reads it through its
		// own volume.
		cb := f.newBacking(3)
		cb.source = lent.source
		cb.data[2*f.pageSize] = 77
		child, cm := f.attach(cb)
		if got := access(t, child, cm, 2, false)[0]; got != 77 || cb.loads != 1 {
			t.Fatalf("the child reads %d at page 2 after %d reads, want 77 read once", got, cb.loads)
		}
		if err := parent.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := access(t, child, cm, 2, false)[0]; got != 77 {
			t.Fatalf("the child reads %d at page 2 after the seal ended, want its 77", got)
		}
		sibling := pointChild(f, cb)
		next, nm := f.attach(sibling)
		if got := access(t, next, nm, 2, false)[0]; got != 77 || sibling.loads != 0 {
			t.Fatalf("the next child reads %d at page 2 after %d reads, want the 77 its sibling read, mapped",
				got, sibling.loads)
		}
	})
}

// A Share after its seal has ended lends nothing: a child of the point reads
// what it inherited through its own backing, and its siblings map what it
// read, as any read page. Before 2026-10-07 such a Share named the point's
// root for a seal that had ended and would never take it back, and in an
// isolated arena every child of the point read each of its pages alone.
func TestAShareAfterItsSealEndedLendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPinnedFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 32, DirtyPages: 8,
			ReadAheadPages: 1})
		parent, _, _, cb := lendingParent(t, f)
		checkpoint := parent.Checkpoint()
		if err := parent.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := checkpoint.Share(f.ctx, cb.source, "v"); err != nil {
			t.Fatal(err)
		}
		sibling := pointChild(f, cb)
		first, fm := f.attach(cb)
		if got := access(t, first, fm, 0, false)[0]; got != 44 || cb.loads != 1 {
			t.Fatalf("the first child reads %d at page 0 after %d reads, want 44 read once", got, cb.loads)
		}
		second, sm := f.attach(sibling)
		if got := access(t, second, sm, 0, false)[0]; got != 44 || sibling.loads != 0 {
			t.Fatalf("the second child reads %d at page 0 after %d reads, want the 44 the first read, mapped",
				got, sibling.loads)
		}
	})
}

// A region that reads the pages of two fork points keeps the number its
// process holds one point's file under until that file's drop has landed, so
// the other point's file, given meanwhile, takes another. Before 2026-10-07
// the end of the first point's seal freed the number before its drop, the
// second file was given under it, and the client refused a file given twice.
func TestAForkFileIsGivenUnderNoNumberAnotherIsDroppedFrom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPinnedFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 32, DirtyPages: 8,
			ReadAheadPages: 1})
		first, _, _, a := lendingParent(t, f)
		share(t, f, first, a)
		second, sm, _ := f.memoryRegion(2)
		access(t, second, sm, 1, true)[0] = 66
		if err := second.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		// The reader's page 0 is the first point's, and its page 1 the
		// second's, which lends it only once the reader is attached.
		pointB := control.Ref{VM: f.source.VM + "-second-point", Sequence: 3}
		rb := f.newBacking(2)
		rb.source = a.source
		rb.sources = map[uint64]control.Ref{1: pointB}
		reader, rm := f.attach(rb)
		if got := access(t, reader, rm, 0, false)[0]; got != 44 || rm.number(0) != 3 {
			t.Fatalf("the reader reads %d at page 0 from file %d, want the 44 the first point lends from file 3",
				got, rm.number(0))
		}
		if err := second.Checkpoint().Share(f.ctx, pointB, "v"); err != nil {
			t.Fatal(err)
		}
		var faulted error
		entered := false
		vmmemory.SetEndForkFileSeam(t, func() {
			// The first point's seal is ending, its file not yet dropped: the
			// reader maps the second point's page, and is given its file.
			entered = true
			faulted = reader.Fault(f.ctx, 1, false)
		})
		if err := first.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if !entered || faulted != nil {
			t.Fatalf("the reader's fault on the second point's page ran %t and returned %v, want it run and served",
				entered, faulted)
		}
		if got := access(t, reader, rm, 1, false)[0]; got != 66 {
			t.Fatalf("the reader reads %d at page 1, want the 66 the second point lends", got)
		}
		if err := second.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A publication's read of a spilled page of a checkpoint holds the region
// live, so a detach that discards the checkpoint waits for the read, and the
// read returns the bytes the seal froze. Before 2026-10-07 the read held
// nothing of the region, and a detach between its look at the reservation
// and its read of it freed the reservation under it.
func TestADetachWaitsForAPublicationsReadOfASpilledPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: 16, DirtyPages: 8,
			ReadAheadPages: 1})
		r, m, _ := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 0xab
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		// Reading the rest of the region spills the sealed page.
		for page := uint64(1); page < 4; page++ {
			access(t, r, m, page, false)
		}
		if s := hostStats(t, f); s.Spills != 1 {
			t.Fatalf("the region spilled %d pages, want the sealed one", s.Spills)
		}
		checkpoint := r.Checkpoint()
		detached := make(chan error, 1)
		vmmemory.SetReadSpillSeam(t, func() {
			// The guest's VMM has gone, and the region detaches while the
			// publication reads the sealed page.
			f.a.mu.Lock()
			clear(m.pages)
			f.a.mu.Unlock()
			go func() { detached <- r.Detach(f.ctx) }()
			synctest.Wait()
		})
		data := make([]byte, f.pageSize)
		if err := checkpoint.ReadDirty(f.ctx, 0, data); err != nil || data[0] != 0xab {
			t.Fatalf("the publication read %#x at page 0 and %v, want the 0xab the seal froze", data[0], err)
		}
		if err := <-detached; err != nil {
			t.Fatal(err)
		}
	})
}

// A page a journal capture took stays write-protected through a seal that is
// abandoned, so the guest's next store into it traps and makes it
// unjournaled, and the next flush takes it. Before 2026-10-07 the seal
// dropped the capture's protection and the abandon did not give it back: a
// read fault mapped the page writable, and a store into it reached no flush.
func TestAPageACaptureTookStaysProtectedThroughAnAbandonedSeal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8,
			ReadAheadPages: 1})
		r, m, _ := f.pmemRegion(2)
		f.storeAt(r, m, 0, 0, 0xab)
		f.capture(r)
		if got := r.Unjournaled(); len(got) != 0 {
			t.Fatalf("unjournaled %v after the capture, want none", got)
		}
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := accessUnder(f.ctx, t, r, m, 0, false)[0]; got != 0xab {
			t.Fatalf("page 0 reads %#x after the abandon, want its 0xab", got)
		}
		writableIsUnjournaled(t, r, m)
		f.storeAt(r, m, 0, 0, 0xcd)
		if got := r.Unjournaled(); len(got) != 1 || got[0] != 0 {
			t.Fatalf("unjournaled %v after a store into page 0, want [0]", got)
		}
	})
}

// forkCampaignSeeds are the seeds the fork campaign runs.
var forkCampaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

// forkCampaignPages is the parent's memory, and each child's, in pages.
const forkCampaignPages = 8

// forkGuest is one guest of the fork campaign: its memory region, what it
// maps, and the byte it must read at each page.
type forkGuest struct {
	name   string
	region *vmmemory.MemoryRegion
	m      *mapping
	want   []byte
}

// step is one access of a guest, a store where value is set, which must read
// what the guest holds.
func (g *forkGuest) step(ctx context.Context, page uint64, value *byte) error {
	got, err := memoryByte(ctx, g.region, g.m, page, value)
	if err != nil {
		return fmt.Errorf("%s at page %d: %w", g.name, page, err)
	}
	if value != nil {
		g.want[page] = *value
		return nil
	}
	if got != g.want[page] {
		return fmt.Errorf("%s reads %d at page %d, want %d", g.name, got, page, g.want[page])
	}
	return nil
}

// A fork point's children read and store into what it lends while its
// parent stores into the same pages and the point's seal ends: by a
// publication's retire, by an unseal, or by the parent's detach. Every read
// of a child returns what the point lent it or what it stored since, and
// every read of the parent what it stored. The seal's end, the fork point's
// lending and the children's faults go on at the points a seed's scheduler
// chooses, which is what puts a child's fault between two pages of the end.
func TestAForkPointsChildrenReadWhatItLentWhileItsSealEnds(t *testing.T) {
	vmmemory.SetCheckpointBatchPages(t, 2)
	for _, seed := range forkCampaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { forkCampaign(t, seed) })
		})
	}
}

// A seed of the fork campaign replays: run twice, it releases every operation
// in the same order.
func TestForkCampaignReplaysItsSeeds(t *testing.T) {
	vmmemory.SetCheckpointBatchPages(t, 2)
	for _, seed := range []uint64{2, 9} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			var orders [2][]byte
			for run := range 2 {
				synctest.Test(t, func(t *testing.T) { orders[run] = forkCampaign(t, seed) })
			}
			if !bytes.Equal(orders[0], orders[1]) {
				t.Fatalf("seed %d released its operations in another order on its second run", seed)
			}
		})
	}
}

// forkCampaign runs one seed and reports the order its scheduler released
// every operation in.
func forkCampaign(t *testing.T, seed uint64) []byte {
	scheduler := sim.NewScheduler(seed)
	runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := sim.WithRuntime(t.Context(), runtime)
		disk := sim.New(sim.Config{Seed: seed}).NewDisk("pager", sim.DiskConfig{})
		f, err := newFixtureOn(t, ctx, disk, vmmemory.Config{PageSize: uint64(pageSize), Arena: suiteArena,
			ResidentPages: 16, LogicalPages: 64, DirtyPages: 32, ReadAheadPages: 1})
		if err != nil {
			t.Error(err)
			return
		}
		random := rand.New(rand.NewPCG(seed, 0))
		parent, err := forkParent(sim.WithTask(ctx, "parent"), f)
		if err != nil {
			t.Error(err)
			return
		}
		var children []*forkGuest
		for _, name := range []string{"child-a", "child-b"} {
			b := f.newBacking(forkCampaignPages)
			b.source = parent.point
			for page, value := range parent.lent {
				b.data[page*f.pageSize] = value
			}
			r, m := f.attach(b)
			children = append(children, &forkGuest{name: name, region: r, m: m, want: bytes.Clone(parent.lent)})
		}
		end := random.IntN(3)
		var wg sync.WaitGroup
		wg.Go(func() {
			if err := parent.run(sim.WithTask(ctx, "parent"), rand.New(rand.NewPCG(seed, 1)), end); err != nil {
				t.Error(err)
			}
		})
		for at, child := range children {
			wg.Go(func() {
				if err := runForkChild(sim.WithTask(ctx, child.name), child,
					rand.New(rand.NewPCG(seed, uint64(2+at)))); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		guests := children
		if end != forkEndDetach {
			guests = append(guests, &parent.forkGuest)
		}
		for _, g := range guests {
			for page := range uint64(forkCampaignPages) {
				if err := g.step(sim.WithTask(ctx, g.name+"-check"), page, nil); err != nil {
					t.Errorf("at the end: %v", err)
				}
			}
		}
		// The guests stop and detach while the scheduler still runs.
		for _, g := range guests {
			g.m.arena.mu.Lock()
			clear(g.m.pages)
			g.m.arena.mu.Unlock()
			if err := g.region.Detach(ctx); err != nil {
				t.Error(err)
			}
		}
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	recording, err := scheduler.Recording(nil)
	if err != nil {
		t.Fatal(err)
	}
	return recording.Execution
}

// How the fork campaign's parent ends the point's seal.
const (
	forkEndRetire = iota
	forkEndUnseal
	forkEndDetach
)

// forkingParent is the fork campaign's parent: a guest whose every page it
// stored into is sealed and lent under point, lent being the bytes it lent.
type forkingParent struct {
	forkGuest
	f       *fixture
	backing *backing
	point   control.Ref
	lent    []byte
}

// forkParent attaches the parent, stores into every page, seals and lends
// the sealed pages under a fork point's name.
func forkParent(ctx context.Context, f *fixture) (*forkingParent, error) {
	r, m, b := f.memoryRegion(forkCampaignPages)
	p := &forkingParent{forkGuest: forkGuest{name: "parent", region: r, m: m, want: initialBytes(forkCampaignPages)},
		f: f, backing: b, point: control.Ref{VM: f.source.VM + "-point", Sequence: 7}}
	for page := range uint64(forkCampaignPages) {
		value := byte(0x40 + page)
		if err := p.step(ctx, page, &value); err != nil {
			return nil, err
		}
	}
	if err := r.Seal(ctx); err != nil {
		return nil, err
	}
	p.lent = bytes.Clone(p.want)
	return p, r.Checkpoint().Share(ctx, p.point, "v")
}

// run stores into and reads the parent's pages, so the checkpoint keeps the
// pages it lent where the guest copies away from them, and then ends the
// seal as end says, and goes on.
func (p *forkingParent) run(ctx context.Context, random *rand.Rand, end int) error {
	steps := func(count int, from byte) error {
		for op := range count {
			if err := sim.Admit(ctx, "campaign/access"); err != nil {
				return err
			}
			page := random.Uint64N(forkCampaignPages)
			var value *byte
			if random.IntN(2) == 0 {
				stored := from + byte(op)
				value = &stored
			}
			if err := p.step(ctx, page, value); err != nil {
				return err
			}
		}
		return nil
	}
	if err := steps(6, 0x80); err != nil {
		return err
	}
	if err := sim.Admit(ctx, "campaign/end"); err != nil {
		return err
	}
	checkpoint := p.region.Checkpoint()
	switch end {
	case forkEndRetire:
		published, err := p.f.publishCheckpoint(ctx, p.region, p.backing)
		if err != nil {
			return err
		}
		if err := checkpoint.Retire(ctx, published); err != nil {
			return err
		}
	case forkEndUnseal:
		if err := p.region.Unseal(ctx); err != nil {
			return err
		}
	case forkEndDetach:
		p.m.arena.mu.Lock()
		clear(p.m.pages)
		p.m.arena.mu.Unlock()
		return p.region.Detach(ctx)
	}
	return steps(6, 0xc0)
}

// runForkChild reads a child's pages, half the time the page after the last
// and otherwise one at random, and now and then stores into one it maps. A
// child stores into no page it does not map: in an isolated arena such a
// store, into a page the point names and does not hold, ends the child's
// region (a store copies a page it read into its own file, and the copy's
// supply loses to the page it was copied from), which is a bug of the store
// and no lock's.
func runForkChild(ctx context.Context, g *forkGuest, random *rand.Rand) error {
	page := uint64(0)
	for op := range 24 {
		if err := sim.Admit(ctx, "campaign/access"); err != nil {
			return err
		}
		if random.IntN(2) == 0 {
			page = (page + 1) % forkCampaignPages
		} else {
			page = random.Uint64N(forkCampaignPages)
		}
		_, mapped := g.m.mappedPage(page)
		var value *byte
		if random.IntN(6) == 0 && mapped {
			stored := byte(0x20 + op)
			value = &stored
		}
		if err := g.step(ctx, page, value); err != nil {
			return err
		}
	}
	return nil
}
