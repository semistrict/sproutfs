package vmmemory_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory"
)

// pageSize is the page the fixtures build their pager with. It is a variable
// because the whole of this suite runs once at each page a pager may be built
// with: a pager's page is an instance's now, and a suite that exercised one of
// them would be a suite that let the other rot. TestMain below is what sets it,
// so a test written later is covered at both pages without saying so, which a
// subtest per test would not give.
//
// Nothing in this package runs in parallel, so one reading of it is one page.
var pageSize = checkpoint.PageSize2MiB

// pageSizes is every page a pager may run, which is checkpoint.GeometryFor's
// list and not a second one.
var pageSizes = []int{checkpoint.PageSize4KiB, checkpoint.PageSize2MiB}

// TestMain runs the whole suite once per page. A failure names the page it
// happened at, because the test names cannot.
func TestMain(m *testing.M) {
	for _, size := range pageSizes {
		pageSize = size
		if code := m.Run(); code != 0 {
			fmt.Fprintf(os.Stderr, "vmmemory: the suite failed with the pager's page at %d bytes\n", size)
			os.Exit(code)
		}
	}
	os.Exit(0)
}

// errInjected is the failure a test makes a backing or an arena report.
var errInjected = errors.New("injected failure")

// arena is the fixture's page store. Its slots are a map and not one entry per
// offset, because a real arena is a sparse file: it has more addresses than it
// may hold pages at once, an offset costs nothing until a page is put there,
// and releasing one punches that memory back out. held is how many offsets hold
// a page — the memory the arena is really holding, which is what the pager's
// budget bounds and what an offset space larger than that budget must not
// change.
type arena struct {
	mu       sync.Mutex
	pageSize int
	offsets  int
	held     int
	peak     int
	slots    map[int][]byte
	mappings []*mapping
	// onRead runs before a slot is read, which is where an eviction holds its
	// victims' page locks. A test uses it to stop an eviction mid-transition.
	onRead func(int)
	// failWrite makes filling a slot fail, which is the arena a test gives a
	// store whose private copy cannot be made.
	failWrite bool
	// onWrite runs after a slot is filled, outside the arena's lock.
	onWrite func(int)
	// writes counts slots filled by copying bytes in, and zeroed the calls
	// that gave fresh slots zeros without writing them.
	writes, zeroed int
}

func newArena(pageSize, offsets int) *arena {
	return &arena{pageSize: pageSize, offsets: offsets, slots: make(map[int][]byte)}
}

// at refuses an address this arena does not have.
func (a *arena) at(slot int) error {
	if slot < 0 || slot >= a.offsets {
		return fmt.Errorf("arena offset %d is outside its %d", slot, a.offsets)
	}
	return nil
}

// put and drop keep the count of offsets holding a page, which is the memory.
func (a *arena) put(slot int, data []byte) {
	if a.slots[slot] == nil {
		a.held++
		a.peak = max(a.peak, a.held)
	}
	a.slots[slot] = data
}
func (a *arena) drop(slot int) {
	if a.slots[slot] != nil {
		a.held--
	}
	delete(a.slots, slot)
}

func (a *arena) Read(_ context.Context, slot int, dst []byte) error {
	if a.onRead != nil {
		a.onRead(slot)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.at(slot); err != nil {
		return err
	}
	// An offset no page has been put at is a hole, and a hole reads as zeros.
	clear(dst)
	copy(dst, a.slots[slot])
	return nil
}
func (a *arena) Write(_ context.Context, slot int, src []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.at(slot); err != nil {
		return err
	}
	if a.failWrite {
		return errInjected
	}
	if a.slots[slot] != nil {
		return fmt.Errorf("write into allocated slot %d", slot)
	}
	a.put(slot, bytes.Clone(src))
	a.writes++
	if onWrite := a.onWrite; onWrite != nil {
		a.mu.Unlock()
		defer a.mu.Lock()
		onWrite(slot)
	}
	return nil
}

// Zero models the Linux arena allocating punched slots: nothing is copied in,
// and from then on the slots hold zeros a mapping can install.
func (a *arena) Zero(_ context.Context, slot, count int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for s := slot; s < slot+count; s++ {
		if err := a.at(s); err != nil {
			return err
		}
		if a.slots[s] != nil {
			return fmt.Errorf("zero of allocated slot %d", s)
		}
		a.put(s, make([]byte, a.pageSize))
	}
	a.zeroed++
	return nil
}
func (a *arena) Release(_ context.Context, slot int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.at(slot); err != nil {
		return err
	}
	for _, m := range a.mappings {
		for _, p := range m.pages {
			if p.slot == slot {
				return fmt.Errorf("release of mapped slot %d", slot)
			}
		}
	}
	a.drop(slot)
	return nil
}

type mapped struct {
	slot     int
	writable bool
}
type mapping struct {
	arena                            *arena
	pages                            map[uint64]mapped
	maps, protects, revokes          int
	failRevoke, failMap, failProtect bool
	// refuseMap and refuseRevoke answer a command the way a client out of
	// mapping budget does: it is refused before anything is touched, so nothing
	// about the mapping moved and no command was issued.
	refuseMap, refuseRevoke bool
	onResolve               func(uint64)
	onProtect               func(uint64, int)
	// onMap runs before a Map replaces anything, which is where a test holds
	// a store's mapping command in flight and looks at what the guest sees.
	onMap func(page uint64, count int)
}

// Map replaces whatever the pages had with the slots, atomically, as the
// client's mremap does. A slot must hold contents: Linux cannot install a
// punched one.
func (m *mapping) Map(_ context.Context, page uint64, slot, count int, writable bool) error {
	if m.onMap != nil {
		m.onMap(page, count)
	}
	if m.refuseMap {
		return vmmemory.ErrMappingRefused
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	for i := range count {
		if m.arena.slots[slot+i] == nil {
			return fmt.Errorf("map of punched slot %d", slot+i)
		}
	}
	for i := range count {
		m.pages[page+uint64(i)] = mapped{slot + i, writable}
	}
	m.maps++
	if m.failMap {
		return errInjected
	}
	return nil
}
func (m *mapping) MapZero(_ context.Context, page uint64, count int) error {
	if m.refuseMap {
		return vmmemory.ErrMappingRefused
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)] = mapped{-1, false}
	}
	m.maps++
	if m.failMap {
		return errInjected
	}
	return nil
}

// Protect models the range write-protect a seal issues: the page and its
// contents stay, the guest keeps reading, and its next store traps.
func (m *mapping) Protect(ctx context.Context, page uint64, count int) error {
	if m.onProtect != nil {
		m.onProtect(page, count)
	}
	// The client checks the command's deadline before it issues one, so a seal
	// whose time has run out stops here, exactly as the real one does.
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	m.protects++
	if m.failProtect {
		return errInjected
	}
	for i := range count {
		p, ok := m.pages[page+uint64(i)]
		if !ok {
			return fmt.Errorf("protect of unmapped page %d", page+uint64(i))
		}
		p.writable = false
		m.pages[page+uint64(i)] = p
	}
	return nil
}
func (m *mapping) Revoke(_ context.Context, page uint64) error {
	if m.refuseRevoke {
		return vmmemory.ErrMappingRefused
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	m.revokes++
	if m.failRevoke {
		return errInjected
	}
	delete(m.pages, page)
	return nil
}
func (m *mapping) Resolve(_ context.Context, page uint64, count int, writable bool) error {
	if m.onResolve != nil {
		m.onResolve(page)
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	for i := range count {
		p, ok := m.pages[page+uint64(i)]
		if !ok || p.writable != writable {
			return errors.New("invalid resolution")
		}
	}
	return nil
}

// backing models one volume of a VM. Its untouched pages are inherited from one
// checkpoint reference shared by every memory region of a fixture, so equal page
// numbers of two backings report equal page identities exactly as two forks
// of one checkpoint do. A write makes a page private to this backing's own next
// checkpoint until the test publishes it, which is when those bytes acquire an
// identity of their own.
type backing struct {
	mu                   sync.Mutex
	pageSize             int // the pager page; the unit of private, zero and every extent
	data                 []byte
	source               control.Ref // the checkpoint untouched pages are inherited from
	owner                string      // this backing's VM identity, for private pages
	sequence             uint64      // the checkpoint private pages will be published under
	private, zero        map[uint64]bool
	loads, loadedBytes   int
	onLoad               func(uint64, int)
	failVerify, failRead bool
	// onLocate runs before a page's stored identity is reported, which is what
	// retiring a page of a checkpoint consults. A test uses it to stop a
	// publication exactly where the page belongs to neither the guest nor the
	// checkpoint.
	onLocate func(uint64, uint64)
	// checkpoints counts the publications a test has made of this backing's sealed
	// pages, and checkpointPages the pages those publications carried.
	checkpoints, checkpointPages atomic.Int64
	// publishedPages counts the pages those publications gave an object of their
	// own, and publishedBytes what those objects carried. A page that reads as
	// all zeroes is in neither: it leaves the index and costs nothing.
	publishedPages, publishedBytes atomic.Int64
}

func (b *backing) Size() uint64 { return uint64(len(b.data)) }
func (b *backing) Load(_ context.Context, off uint64, dst []byte) error {
	if b.failRead {
		return errInjected
	}
	if b.onLoad != nil {
		b.onLoad(off, len(dst))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loads++
	b.loadedBytes += len(dst)
	copy(dst, b.data[off:])
	return nil
}

// identity is what names one page: a hole, this backing's own unpublished
// overlay, or the checkpoint it inherited the page from.
func (b *backing) identity(page uint64) control.Identity {
	// A page identity is numbered in the volume's own page, which is this
	// pager's: a number in anything else would name a different page and two
	// memory regions inheriting the same checkpoint would stop sharing.
	number := page
	switch {
	case b.zero[page]:
		return control.Identity{Zero: true}
	case b.private[page]:
		return control.Identity{Ref: control.Ref{VM: b.owner, Sequence: b.sequence}, Volume: "v", Page: number}
	default:
		return control.Identity{Ref: b.source, Volume: "v", Page: number}
	}
}

func (b *backing) Locate(_ context.Context, off, length uint64) ([]control.Extent, error) {
	if b.onLocate != nil {
		b.onLocate(off, length)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	size := uint64(b.pageSize)
	return extentsOf(off, length, size, func(cursor uint64) control.Identity { return b.identity(cursor / size) }), nil
}

// extentsOf describes a range in steps of grain bytes, merging neighbours of
// equal identity, which is the shape a volume reports.
func extentsOf(off, length, grain uint64, identity func(offset uint64) control.Identity) []control.Extent {
	var extents []control.Extent
	for cursor := off; cursor < off+length; cursor += grain {
		next := control.Extent{Offset: cursor, Length: grain, Identity: identity(cursor)}
		if n := len(extents); n > 0 && extents[n-1].Identity == next.Identity && extents[n-1].Offset+extents[n-1].Length == next.Offset {
			extents[n-1].Length += next.Length
			continue
		}
		extents = append(extents, next)
	}
	return extents
}

// publish makes the current contents of every page inherited bytes again, under
// a fresh checkpoint reference of this backing's own. It is what selecting a
// checkpoint does to a volume: every page it carried belongs to that checkpoint
// from now on, and a pager retiring its checkpoint against it shares those
// pages by that identity.
func (b *backing) publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.source = control.Ref{VM: b.owner, Sequence: b.sequence}
	b.sequence++
	clear(b.private)
}

// publishPage installs one page a checkpoint carried, or records that the page
// is a hole. A page that reads as all zeroes leaves the index rather than
// becoming a member of a part: it costs no object, not a byte is uploaded for
// it, and it reads back as the zeroes it holds. That is what
// Publication.writeEdits does, and a fixture that published zeroes as bytes
// would hide exactly what a write-ahead run costs.
func (b *backing) publishPage(number uint64, src []byte) {
	size := uint64(b.pageSize)
	if allZeroes(src) {
		b.mu.Lock()
		defer b.mu.Unlock()
		clear(b.data[number*size : (number+1)*size])
		b.zero[number] = true
		delete(b.private, number)
		return
	}
	b.publishedPages.Add(1)
	b.publishedBytes.Add(int64(len(src)))
	b.write(number*size, src)
}

// allZeroes is the publication's own test for a page that costs no object.
func allZeroes(data []byte) bool {
	for _, v := range data {
		if v != 0 {
			return false
		}
	}
	return true
}

// write installs one page's published bytes, exactly as a checkpoint's page
// object holds them. A page a checkpoint carries is no longer a hole.
func (b *backing) write(off uint64, src []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	copy(b.data[off:], src)
	size := uint64(b.pageSize)
	for page := off / size; page < (off+uint64(len(src))+size-1)/size; page++ {
		delete(b.zero, page)
	}
}

func (b *backing) Verify(context.Context) error {
	if b.failVerify {
		return errInjected
	}
	return nil
}

type fixture struct {
	t        *testing.T
	h        *vmmemory.Host
	a        *arena
	disk     *sim.Disk
	spill    platform.File
	pageSize int
	source   control.Ref // the checkpoint every memory region of the fixture inherits
	owners   int
}

// spillBytes is the whole extent of this pager's spill file, which is its fixed
// disk cap: the dirty budget in pages of this pager, and no other pager's.
func (f *fixture) spillBytes() int64 {
	f.t.Helper()
	size, err := f.spill.Size(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	return size
}

// checkpoint is what a checkpoint does to one memory region: it seals the dirty set,
// reads every sealed page out of the pages the guest is still running on,
// writes those bytes into the volume, selects the checkpoint, and retires the
// checkpoint. It is the only way a memory region's pages reach its volume.
func (f *fixture) checkpoint(r *vmmemory.MemoryRegion, b *backing) error {
	return f.checkpointContext(f.t.Context(), r, b)
}

func (f *fixture) checkpointContext(ctx context.Context, r *vmmemory.MemoryRegion, b *backing) error {
	if err := r.Seal(ctx); err != nil {
		return err
	}
	published, err := f.publishCheckpoint(ctx, r, b)
	return errors.Join(err, r.Checkpoint().Retire(ctx, published))
}

// publishCheckpoint reads the sealed pages and installs them in the volume,
// reporting whether the checkpoint that would hold them was selected.
func (f *fixture) publishCheckpoint(ctx context.Context, r *vmmemory.MemoryRegion, b *backing) (bool, error) {
	ckpt := r.Checkpoint()
	if ckpt == nil {
		return false, vmmemory.ErrNotSealed
	}
	page := make([]byte, f.pageSize)
	pages := ckpt.DirtyPages()
	for _, number := range pages {
		if err := ckpt.ReadDirty(ctx, number, page); err != nil {
			return false, err
		}
		b.publishPage(number, page)
	}
	b.publish()
	b.checkpoints.Add(1)
	b.checkpointPages.Add(int64(len(pages)))
	return true, nil
}

// settle is what a publication does behind the pause before it enumerates a
// checkpoint's pages: every page whose sealed bytes are the ones its origin
// still holds leaves the set. It reports how many did.
func (f *fixture) settle(r *vmmemory.MemoryRegion) int {
	f.t.Helper()
	unchanged, err := r.Checkpoint().Settle(f.t.Context())
	if err != nil {
		f.t.Fatalf("settling the checkpoint: %v", err)
	}
	return unchanged
}

// mustCheckpoint fails the test when a checkpoint does not complete.
func (f *fixture) mustCheckpoint(r *vmmemory.MemoryRegion, b *backing) {
	f.t.Helper()
	if err := f.checkpoint(r, b); err != nil {
		f.t.Fatalf("checkpointing the memory region: %v", err)
	}
}

// finishCheckpoint publishes and retires a checkpoint the test has already
// sealed.
func (f *fixture) finishCheckpoint(r *vmmemory.MemoryRegion, b *backing) {
	f.t.Helper()
	if _, err := f.publishCheckpoint(f.t.Context(), r, b); err != nil {
		f.t.Fatalf("publishing the checkpoint: %v", err)
	}
	if err := r.Checkpoint().Retire(f.t.Context(), true); err != nil {
		f.t.Fatalf("retiring the checkpoint: %v", err)
	}
}

func newFixture(t *testing.T, resident, logical, dirty int) *fixture {
	t.Helper()
	return newConfiguredFixture(t, vmmemory.Config{ResidentPages: resident, LogicalPages: logical, DirtyPages: dirty})
}

// newConfiguredFixture builds a host exactly as configured. A configuration
// that names no page takes the one the suite is running at, so a test that does
// not care about the geometry is exercised at both.
func newConfiguredFixture(t *testing.T, cfg vmmemory.Config, shared ...*resource.Budget) *fixture {
	t.Helper()
	if cfg.PageSize == 0 {
		cfg.PageSize = uint64(pageSize)
	}
	f, err := newBrokenFixture(t, cfg, shared...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// newBrokenFixture is newConfiguredFixture reporting rather than failing, which
// is what a test of a configuration the pager refuses needs.
func newBrokenFixture(t *testing.T, cfg vmmemory.Config, shared ...*resource.Budget) (*fixture, error) {
	t.Helper()
	disk := sim.New(sim.Config{}).NewDisk("pager", sim.DiskConfig{})
	spill, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spill.Close() })
	offsets := cfg.ArenaOffsets
	if offsets == 0 {
		offsets = cfg.ResidentPages
	}
	a := newArena(int(cfg.PageSize), offsets)
	resources := testresource.New()
	if len(shared) != 0 {
		resources = shared[0]
	}
	h, err := vmmemory.New(t.Context(), resources, cfg, a, spill)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := h.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return &fixture{t: t, h: h, a: a, disk: disk, spill: spill, pageSize: int(cfg.PageSize),
		source: control.Ref{VM: t.Name(), Sequence: 1}}, nil
}

// newBacking returns a backing whose pages are inherited from the fixture's
// shared checkpoint, every byte of page i holding i+1, so memory regions of one
// fixture are forks of one checkpoint.
func (f *fixture) newBacking(pages int) *backing {
	f.owners++
	b := &backing{pageSize: f.pageSize, data: make([]byte, pages*f.pageSize), source: f.source,
		owner: fmt.Sprintf("%s-vm%d", f.source.VM, f.owners), sequence: 2,
		private: map[uint64]bool{}, zero: map[uint64]bool{}}
	for i := range pages {
		for j := range f.pageSize {
			b.data[i*f.pageSize+j] = byte(i + 1)
		}
	}
	return b
}

// newUnrelatedBacking returns a backing whose bytes are identical but whose
// page identities are not: nothing about it may be shared with the fixture's memory regions.
func (f *fixture) newUnrelatedBacking(pages int) *backing {
	b := f.newBacking(pages)
	b.source = control.Ref{VM: b.owner + "-unrelated", Sequence: 1}
	return b
}

func (f *fixture) memoryRegion(pages int) (*vmmemory.MemoryRegion, *mapping, *backing) {
	f.t.Helper()
	b := f.newBacking(pages)
	r, m := f.attach(b)
	return r, m, b
}

// ram is one backing attached as guest RAM, which is what every test that does
// not care about the kind maps.
func ram(b vmmemory.Backing) vmmemory.MemoryRegionBacking {
	return vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: b}
}

// attach maps one backing as guest RAM, which is what all but the tests of the
// kinds themselves care about; attachKind states the kind.
func (f *fixture) attach(b vmmemory.Backing) (*vmmemory.MemoryRegion, *mapping) {
	f.t.Helper()
	return f.attachKind(vmmemory.Ram, b)
}
func (f *fixture) attachKind(kind vmmemory.MemoryRegionKind, b vmmemory.Backing) (*vmmemory.MemoryRegion, *mapping) {
	f.t.Helper()
	m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
	f.a.mappings = append(f.a.mappings, m)
	r, err := f.h.Attach(f.t.Context(), vmmemory.MemoryRegionBacking{Kind: kind, Backing: b}, m)
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
func access(t *testing.T, r *vmmemory.MemoryRegion, m *mapping, page uint64, write bool) []byte {
	t.Helper()
	p, ok := m.pages[page]
	if !ok || (write && !p.writable) {
		if err := r.Fault(t.Context(), page, write); err != nil {
			t.Fatal(err)
		}
		p = m.pages[page]
	}
	if p.slot == -1 {
		return make([]byte, m.arena.pageSize)
	}
	return m.arena.slots[p.slot]
}

func TestSharingCOWReclaimAndDurability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 4)
		a, am, ab := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		access(t, a, am, 0, false)
		access(t, b, bm, 0, false)
		if am.pages[0].slot != bm.pages[0].slot {
			t.Fatal("equal inherited identities must share")
		}
		access(t, a, am, 0, true)[0] = 99
		if access(t, b, bm, 0, false)[0] != 1 {
			t.Fatal("COW mutated sibling")
		}
		for i := uint64(1); i < 4; i++ {
			access(t, b, bm, i, false)
		}
		if access(t, a, am, 0, false)[0] != 99 {
			t.Fatal("dirty spill lost contents")
		}
		if ab.data[0] != 1 {
			t.Fatal("local spill claimed volume durability")
		}
		f.mustCheckpoint(a, ab)
		if ab.data[0] != 99 {
			t.Fatal("the checkpoint did not publish mapped writes")
		}
		access(t, a, am, 0, true)[0] = 100
		for i := uint64(1); i < 4; i++ {
			access(t, b, bm, i, false)
		}
		if access(t, a, am, 0, false)[0] != 100 {
			t.Fatal("refault used previous spill epoch")
		}
	})
}

func TestBudgetOneSlotCOWAndDirtyAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 1, 4, 1)
		a, am, ab := f.memoryRegion(2)
		b, bm, _ := f.memoryRegion(2)
		access(t, a, am, 0, false)
		access(t, b, bm, 0, false)
		access(t, a, am, 0, true)[0] = 71
		// No checkpoint is in flight and no supervisor answers this host's
		// pressure, so the store is a stall: the deliberate stop's error, not
		// the capacity failure that kills a VMM through the fault path.
		if err := b.Fault(t.Context(), 0, true); !errors.Is(err, vmmemory.ErrDirtyStalled) {
			t.Fatalf("dirty admission: %v", err)
		}
		if access(t, b, bm, 0, false)[0] != 1 {
			t.Fatal("rejected write changed sibling")
		}
		if access(t, a, am, 0, false)[0] != 71 {
			t.Fatal("single-slot COW lost data")
		}
		f.mustCheckpoint(a, ab)
		access(t, b, bm, 0, true)[0] = 82
		s, err := f.h.Stats(t.Context())
		if err != nil || s.ResidentPages != 1 || s.DirtyPages != 1 || s.LogicalPages != 4 {
			t.Fatalf("accounting: %+v %v", s, err)
		}
		if _, err := f.h.Attach(t.Context(), ram(f.newBacking(1)), am); !errors.Is(err, vmmemory.ErrCapacity) {
			t.Fatalf("logical admission: %v", err)
		}
	})
}

func TestSpillFailureKeepsCurrentCopy(t *testing.T) {
	for _, operation := range []sim.DiskOperation{sim.DiskWrite} {
		t.Run(string(operation), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, 1, 2, 2)
				r, m, _ := f.memoryRegion(2)
				access(t, r, m, 0, true)[0] = 51
				f.disk.FailNext(operation, 1)
				if err := r.Fault(t.Context(), 1, false); !errors.Is(err, platform.ErrInjectedFault) {
					t.Fatalf("spill failure: %v", err)
				}
				if access(t, r, m, 0, false)[0] != 51 {
					t.Fatal("spill failure lost current copy")
				}
				access(t, r, m, 0, true)[0] = 52
				access(t, r, m, 1, false)
				if access(t, r, m, 0, false)[0] != 52 {
					t.Fatal("retry used old spill")
				}
			})
		})
	}
}

// A publication that never lands retains every dirty page, and the checkpoint
// after it carries the guest's later stores as well.
func TestAbandonedCheckpointRetainsEveryDirtyPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 2, 2)
		r, m, b := f.memoryRegion(2)
		access(t, r, m, 0, true)[0] = 33
		access(t, r, m, 1, true)[0] = 44
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := r.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		s, _ := f.h.Stats(t.Context())
		if s.DirtyPages != 2 {
			t.Fatal("the abandoned checkpoint discarded dirty state")
		}
		access(t, r, m, 0, true)[0] = 55
		f.mustCheckpoint(r, b)
		if b.data[0] != 55 || b.data[pageSize] != 44 {
			t.Fatal("the retried checkpoint published the wrong bytes")
		}
	})
}

func TestAmbiguousMappingFailurePinsUntilProcessExit(t *testing.T) {
	for _, op := range []string{"map", "revoke"} {
		t.Run(op, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, 1, 2, 2)
				a, am, _ := f.memoryRegion(1)
				b, bm := f.attach(f.newUnrelatedBacking(1))
				if op == "map" {
					am.failMap = true
					if err := a.Fault(t.Context(), 0, false); !errors.Is(err, errInjected) {
						t.Fatal(err)
					}
				} else {
					access(t, a, am, 0, false)
					am.failRevoke = true
					// The revocation that failed is the other memory region's, and so
					// is the failure: this fault only wanted a page, and the
					// one it tried for turned out not to be this host's to take
					// back. It is told the arena has nothing, which is true,
					// rather than told the other memory region's error, which would end
					// this machine for that one's death.
					err := b.Fault(t.Context(), 0, false)
					if !errors.Is(err, vmmemory.ErrCapacity) {
						t.Fatalf("one memory region's failed revocation was reported to another: %v", err)
					}
					if errors.Is(err, errInjected) {
						t.Fatal("the other memory region's injected failure reached this one")
					}
				}
				if err := b.Fault(t.Context(), 0, false); !errors.Is(err, vmmemory.ErrCapacity) {
					t.Fatalf("uncertain alias was recycled: %v", err)
				}
				clear(am.pages) // simulated process exit, before detach
				if err := a.Detach(t.Context()); err != nil {
					t.Fatal(err)
				}
				if access(t, b, bm, 0, false)[0] != 1 {
					t.Fatal("slot not reusable after process exit")
				}
			})
		})
	}
}

func TestASharedPageStillChecksWriterAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 2, 2)
		a, am, ab := f.memoryRegion(1)
		b, bm, _ := f.memoryRegion(1)
		access(t, a, am, 0, false)
		access(t, b, bm, 0, false)
		if am.pages[0].slot != bm.pages[0].slot {
			t.Fatal("matching identities did not share a resident page")
		}
		ab.failVerify = true
		if err := a.Verify(t.Context()); !errors.Is(err, errInjected) {
			t.Fatal(err)
		}
		if err := a.Fault(t.Context(), 0, true); !errors.Is(err, errInjected) {
			t.Fatal("fenced memory region resumed")
		}
		if access(t, b, bm, 0, false)[0] != 1 {
			t.Fatal("fencing one memory region changed its sibling's bytes")
		}
	})
}

func TestRandomizedEvictionAgainstIndependentByteModel(t *testing.T) {
	for seed := uint64(0); seed < 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, 3, 16, 16)
				var memoryRegions []*vmmemory.MemoryRegion
				var mappings []*mapping
				var backings []*backing
				var expected [][]byte
				for range 2 {
					r, m, b := f.memoryRegion(8)
					memoryRegions = append(memoryRegions, r)
					mappings = append(mappings, m)
					backings = append(backings, b)
					expected = append(expected, bytes.Clone(b.data))
				}
				rng := rand.New(rand.NewPCG(seed, seed+1))
				for step := range 500 {
					vm := rng.IntN(2)
					p := uint64(rng.IntN(8))
					off := rng.IntN(pageSize)
					write := rng.IntN(3) == 0
					data := access(t, memoryRegions[vm], mappings[vm], p, write)
					if write {
						value := byte(rng.Uint32())
						data[off] = value
						expected[vm][int(p)*pageSize+off] = value
					}
					if !bytes.Equal(data, expected[vm][int(p)*pageSize:(int(p)+1)*pageSize]) {
						t.Fatalf("step %d: page mismatch", step)
					}
					if step%37 == 0 {
						if err := f.checkpoint(memoryRegions[vm], backings[vm]); err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(backings[vm].data, expected[vm]) {
							t.Fatalf("step %d: durable checkpoint mismatch", step)
						}
					}
				}
			})
		})
	}
}

// A checkpoint of one volume must not stop another volume's guest, and must not
// stop its own: a sealed memory region keeps faulting and storing for the whole of the
// publication that is reading its pages.
func TestACheckpointInFlightStopsNeitherVolume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 8)
		a, am, ab := f.memoryRegion(2)
		b, bm, bb := f.memoryRegion(2)
		access(t, a, am, 0, true)[0] = 90
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The other volume runs its own checkpoint end to end while this one is
		// sealed.
		access(t, b, bm, 1, true)[0] = 72
		if err := f.checkpoint(b, bb); err != nil {
			t.Fatal(err)
		}
		if bb.data[pageSize] != 72 {
			t.Fatalf("the unrelated checkpoint published %d, want 72", bb.data[pageSize])
		}
		// The sealed volume's own guest keeps faulting, reading and storing.
		if got, err := memoryByte(t.Context(), a, am, 0, nil); err != nil || got != 90 {
			t.Fatalf("a read of a sealed page returned %d: %v", got, err)
		}
		value := byte(91)
		if _, err := memoryByte(t.Context(), a, am, 0, &value); err != nil {
			t.Fatal(err)
		}
		if err := a.Fault(t.Context(), 1, false); err != nil {
			t.Fatalf("a clean fault on the sealed volume: %v", err)
		}
		if _, err := f.publishCheckpoint(t.Context(), a, ab); err != nil {
			t.Fatal(err)
		}
		if err := a.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if ab.data[0] != 90 {
			t.Fatalf("the checkpoint published %d, want the 90 the seal froze", ab.data[0])
		}
		if got, err := memoryByte(t.Context(), a, am, 0, nil); err != nil || got != 91 {
			t.Fatalf("the guest reads %d after the checkpoint, want its own 91: %v", got, err)
		}
	})
}

// A simulated CPU byte access atomically checks its PTE and accesses its slot.
// Revocation can happen between Fault and retry, just as it can with real UFFD.
// A zero mapping reads zeros and is never writable.
func memoryByte(ctx context.Context, r *vmmemory.MemoryRegion, m *mapping, page uint64, value *byte) (byte, error) {
	for {
		m.arena.mu.Lock()
		p, ok := m.pages[page]
		if ok && (value == nil || p.writable) {
			result := byte(0)
			if p.slot >= 0 {
				data := m.arena.slots[p.slot]
				if value != nil {
					data[0] = *value
				}
				result = data[0]
			}
			m.arena.mu.Unlock()
			return result, nil
		}
		m.arena.mu.Unlock()
		if err := r.Fault(ctx, page, value != nil); err != nil {
			return 0, err
		}
	}
}

func TestConcurrentVolumesShareReclaimAndCheckpoints(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 32, 32)
		var memoryRegions []*vmmemory.MemoryRegion
		var mappings []*mapping
		var backings []*backing
		for range 4 {
			r, m, b := f.memoryRegion(8)
			memoryRegions = append(memoryRegions, r)
			mappings = append(mappings, m)
			backings = append(backings, b)
		}
		var wg sync.WaitGroup
		for vm := range memoryRegions {
			wg.Go(func() {
				rng := rand.New(rand.NewPCG(uint64(vm), uint64(vm+7)))
				var expected [8]byte
				for i := range expected {
					expected[i] = byte(i + 1)
				}
				for step := range 200 {
					page := rng.IntN(8)
					var write *byte
					if rng.IntN(3) == 0 {
						v := byte(rng.Uint32())
						expected[page] = v
						write = &v
					}
					got, err := memoryByte(t.Context(), memoryRegions[vm], mappings[vm], uint64(page), write)
					if err != nil {
						t.Error(err)
						return
					}
					if got != expected[page] {
						t.Errorf("VM %d step %d: got %d, want %d", vm, step, got, expected[page])
						return
					}
					if step%23 == 0 {
						if err := f.checkpoint(memoryRegions[vm], backings[vm]); err != nil {
							t.Error(err)
							return
						}
						for i, value := range expected {
							if backings[vm].data[i*pageSize] != value {
								t.Errorf("VM %d: incorrect durable checkpoint", vm)
								return
							}
						}
					}
				}
			})
		}
		wg.Wait()
	})
}
