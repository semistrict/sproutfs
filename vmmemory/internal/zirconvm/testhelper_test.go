// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/unittests/test_helper.cc and vm/unittests/test_helper.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// vmoEnv is what a ported VMO case runs over: a context carrying a simulated
// runtime, so the departures' guards can be turned on, and a node with its
// pmm, page queues and compression. Zircon's cases run over the kernel's
// globals.
type vmoEnv struct {
	ctx         context.Context
	runtime     *sim.Runtime
	ps          uint64
	pmm         *testPmm
	node        *Node
	compression *Compression
	storage     *SpillStorage
	disk        *sim.Disk
	// disks is how many spill disks the case has made.
	disks int
}

// forEachVmoPageSize runs body at each page size a pager runs at, in a
// synctest bubble over a simulated runtime, as every ported case does. The
// node's compression stores pages as they are in a spill file on a simulated
// disk, as the pager's does.
func forEachVmoPageSize(t *testing.T, body func(t *testing.T, env *vmoEnv)) {
	t.Helper()
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		pmm := newTestPmm(ps)
		env := &vmoEnv{ctx: ctx, runtime: runtime, ps: ps, pmm: pmm}
		env.storage, env.disk = env.newSpillStorage(t, testSpillPages)
		env.compression = NewCompression(pmm, ps, env.storage, StoreAsIs{}, ps)
		env.node = NewNode(pmm, ps, env.compression)
		body(t, env)
	})
}

// testSpillPages is the dirty budget of a case's spill: more pages than any
// case dirties.
const testSpillPages = 64

// newSpillStorage is a spill of pages pages on a disk of its own, with room
// for it, and the disk.
func (env *vmoEnv) newSpillStorage(t *testing.T, pages int) (*SpillStorage, *sim.Disk) {
	t.Helper()
	env.disks++
	disk := env.runtime.NewDisk(fmt.Sprintf("spill-%d", env.disks),
		sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: int64(pages) * int64(env.ps) * 2}})
	file, err := disk.Open(env.ctx, "spill", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewSpillStorage(env.ctx, file, env.ps, pages)
	if err != nil {
		t.Fatal(err)
	}
	return storage, disk
}

// testPmm hands out pages of one size and counts those out. Zircon's cases
// allocate from the kernel's pmm.
type testPmm struct {
	ps       uint64
	zero     *VmPage
	out      int
	failNext bool
}

func newTestPmm(ps uint64) *testPmm {
	return &testPmm{ps: ps, zero: NewPage(make([]byte, ps))}
}

func (p *testPmm) AllocPage() (*VmPage, error) {
	if p.failNext {
		p.failNext = false
		return nil, ErrNoMemory
	}
	p.out++
	data := make([]byte, p.ps)
	// A new page holds whatever it held; make that not zero, so a page
	// that should have been zeroed is caught.
	for i := range data {
		data[i] = 0xa5
	}
	return NewPage(data), nil
}

func (p *testPmm) FreePage(page *VmPage) {
	if page.queue != nil {
		panic("a freed page is in a page queue")
	}
	p.out--
}

func (p *testPmm) ZeroPage() *VmPage { return p.zero }

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// stubPageProvider believes it backs an object a user pager backs, but can
// provide nothing: Zircon's StubPageProvider. It panics on a request unless
// told to ignore requests.
type stubPageProvider struct {
	trapDirty      bool
	ignoreRequests bool
}

func (p *stubPageProvider) Properties() PageSourceProperties {
	return PageSourceProperties{
		IsUserPager:         true,
		SupportsRequestType: [numPageRequestTypes]bool{true, p.trapDirty, false},
	}
}

func (p *stubPageProvider) SendAsyncRequest(*PageRequest) {
	if !p.ignoreRequests {
		panic("the stub provider was sent a request")
	}
}

func (p *stubPageProvider) ClearAsyncRequest(*PageRequest) {
	if !p.ignoreRequests {
		panic("the stub provider was cleared a request")
	}
}

func (p *stubPageProvider) SwapAsyncRequest(_, _ *PageRequest) {
	if !p.ignoreRequests {
		panic("the stub provider was swapped a request")
	}
}

func (p *stubPageProvider) DebugIsPageOk(*VmPage, uint64) bool { return true }
func (p *stubPageProvider) OnDetach()                          {}
func (p *stubPageProvider) OnClose()                           {}

func (p *stubPageProvider) WaitOnEvent(context.Context, *Event) error {
	panic("not implemented")
}

// makePartiallyCommittedPagerVmo is make_partially_committed_pager_vmo: an
// object a stub pager backs, of numPages pages, the first committedPages of
// which are supplied, from an anonymous object's pages, so no fault is taken.
// It returns the pages supplied.
func makePartiallyCommittedPagerVmo(t *testing.T, env *vmoEnv, numPages, committedPages uint64, trapDirty,
	ignoreRequests bool) (*ObjectPaged, []*VmPage) {
	t.Helper()
	src := NewPageSource(&stubPageProvider{trapDirty: trapDirty, ignoreRequests: ignoreRequests})
	vmo, err := CreateExternal(env.node, src, numPages*env.ps)
	mustNotFail(t, "create external", err)
	var pages []*VmPage
	if committedPages > 0 {
		pages = supplyPagerVmoPages(t, env, vmo, 0, committedPages)
	}
	return vmo, pages
}

// makeCommittedPagerVmo is make_committed_pager_vmo.
func makeCommittedPagerVmo(t *testing.T, env *vmoEnv, numPages uint64, trapDirty bool) (*ObjectPaged, []*VmPage) {
	t.Helper()
	return makePartiallyCommittedPagerVmo(t, env, numPages, numPages, trapDirty, false)
}

// makeUncommittedPagerVmo is make_uncommitted_pager_vmo.
func makeUncommittedPagerVmo(t *testing.T, env *vmoEnv, numPages uint64, trapDirty bool) *ObjectPaged {
	t.Helper()
	vmo, _ := makePartiallyCommittedPagerVmo(t, env, numPages, 0, trapDirty, false)
	return vmo
}

// supplyPagerVmoPages is supply_pager_vmo_pages: it supplies numPages pages
// from pageOffset, taken from an anonymous object, and returns them.
func supplyPagerVmoPages(t *testing.T, env *vmoEnv, vmo *ObjectPaged, pageOffset, numPages uint64) []*VmPage {
	t.Helper()
	aux, err := CreateObjectPaged(env.node, numPages*env.ps)
	mustNotFail(t, "create aux", err)
	mustNotFail(t, "commit aux", aux.CommitRange(env.ctx, 0, numPages*env.ps))
	splice := NewPageSpliceList[VmPage](env.ps, env.node)
	mustNotFail(t, "take", aux.TakePages(env.ctx, 0, numPages*env.ps, splice))
	mustNotFail(t, "supply", vmo.SupplyPages(env.ctx, pageOffset*env.ps, numPages*env.ps, splice, PagerSupply))
	aux.Destroy()
	pages := make([]*VmPage, numPages)
	for i := range numPages {
		page, err := vmo.GetPage(env.ctx, (pageOffset+i)*env.ps, 0, nil)
		mustNotFail(t, "get page", err)
		pages[i] = page
	}
	return pages
}

// testRand is test_rand.
func testRand(seed uint32) uint32 { return seed*1664525 + 1013904223 }

// fillRegion is fill_region: a pattern from seed.
func fillRegion(seed uint64, b []byte) {
	val := uint32(seed) ^ uint32(seed>>32)
	for i := 0; i+4 <= len(b); i += 4 {
		binary.LittleEndian.PutUint32(b[i:], val)
		val = testRand(val)
	}
}

// testRegion is test_region: whether b holds fillRegion's pattern.
func testRegion(seed uint64, b []byte) bool {
	want := make([]byte, len(b))
	fillRegion(seed, want)
	return bytes.Equal(b, want)
}

// testMapping maps an object as an address space would, a page at a time
// on fault, and makes the range changes the object asks for: the part of
// Zircon's VmMapping and arch aspace its cases look at. An access to a page
// not mapped, or a write to one mapped read-only, faults it in first.
type testMapping struct {
	t      *testing.T
	env    *vmoEnv
	vmo    *ObjectPaged
	pages  []*VmPage
	write  []bool
	faults int
}

func newTestMapping(t *testing.T, env *vmoEnv, vmo *ObjectPaged) *testMapping {
	n := vmo.Size() / env.ps
	m := &testMapping{t: t, env: env, vmo: vmo, pages: make([]*VmPage, n), write: make([]bool, n)}
	vmo.AddMapping(m)
	return m
}

// RangeChangeUpdate is the mapping's half of a range change.
func (m *testMapping) RangeChangeUpdate(offset, length uint64, op RangeChangeOp) {
	for off := offset; off < offset+length && off/m.env.ps < uint64(len(m.pages)); off += m.env.ps {
		i := off / m.env.ps
		switch op {
		case Unmap, UnmapZeroPage, UnmapAndHarvest:
			m.pages[i] = nil
			m.write[i] = false
		case RemoveWrite:
			m.write[i] = false
		}
	}
}

// fault maps the page at offset, as a page fault does.
func (m *testMapping) fault(offset uint64, write bool) {
	m.t.Helper()
	flags := PfFlagHwFault
	if write {
		flags |= PfFlagWrite
	}
	page, err := m.vmo.GetPageBlocking(m.env.ctx, offset&^(m.env.ps-1), flags)
	mustNotFail(m.t, "fault", err)
	i := offset / m.env.ps
	m.pages[i] = page
	m.write[i] = write
	m.faults++
}

// commitAndMap faults in every page of the object, as a mapping with
// VMM_FLAG_COMMIT does.
func (m *testMapping) commitAndMap(write bool) {
	m.t.Helper()
	for off := uint64(0); off < uint64(len(m.pages))*m.env.ps; off += m.env.ps {
		m.fault(off, write)
	}
}

// writeAt stores b at offset through the mapping.
func (m *testMapping) writeAt(b []byte, offset uint64) {
	m.t.Helper()
	for done := uint64(0); done < uint64(len(b)); {
		off := offset + done
		i := off / m.env.ps
		if m.pages[i] == nil || !m.write[i] {
			m.fault(off, true)
		}
		n := copy(m.pages[i].data[off%m.env.ps:], b[done:])
		done += uint64(n)
	}
}

// readAt loads b from offset through the mapping.
func (m *testMapping) readAt(b []byte, offset uint64) {
	m.t.Helper()
	for done := uint64(0); done < uint64(len(b)); {
		off := offset + done
		i := off / m.env.ps
		if m.pages[i] == nil {
			m.fault(off, false)
		}
		n := copy(b[done:], m.pages[i].data[off%m.env.ps:])
		done += uint64(n)
	}
}

// query is the arch aspace's Query: the page mapped at offset, and whether
// it is writable.
func (m *testMapping) query(offset uint64) (*VmPage, bool, bool) {
	i := offset / m.env.ps
	return m.pages[i], m.write[i], m.pages[i] != nil
}

// unmap removes the mapping, as FreeRegion does.
func (m *testMapping) unmap() { m.vmo.RemoveMapping(m) }

// fillAndTestUser is fill_and_test_user: fill a mapping with a pattern and
// read it back.
func fillAndTestUser(t *testing.T, m *testMapping, length uint64) bool {
	t.Helper()
	pattern := make([]byte, length)
	fillRegion(0x1000, pattern)
	m.writeAt(pattern, 0)
	got := make([]byte, length)
	m.readAt(got, 0)
	return bytes.Equal(got, pattern)
}

// reclaim is the vmo_unittest.cc helper of that name that simulates the
// reclamation thread: a page not dirty moves to the don't-need queue first,
// as a dirty page is never isolated, and its pages reclaimed are counted.
func reclaim(env *vmoEnv, vmo *ObjectPaged, page *VmPage, offset uint64, action EvictionAction) uint64 {
	pq := env.node.PageQueues()
	if !pq.DebugPageIsPagerBackedDirty(page) {
		pq.MoveToReclaimDontNeed(page)
	}
	return reclaimCow(env.ctx, vmo.DebugGetCowPages(), page, offset, action, nil)
}

// reclaimCow is the helper's form over a CowPages, with a compressor.
func reclaimCow(ctx context.Context, cow *CowPages, page *VmPage, offset uint64, action EvictionAction, compressor *Compressor) uint64 {
	result, failure := cow.ReclaimPage(ctx, page, offset, action, compressor)
	if failure != ReclaimSucceeded {
		return 0
	}
	return result.NumPages
}

// compressPage is compress_page: reclaim by compression with an armed
// compressor.
func compressPage(t *testing.T, env *vmoEnv, cow *CowPages, page *VmPage, offset uint64) uint64 {
	t.Helper()
	guard := env.compression.AcquireCompressor()
	defer guard.Release()
	compressor := guard.Get()
	mustNotFail(t, "arm", compressor.Arm())
	return reclaimCow(env.ctx, cow, page, offset, FollowHint, compressor)
}

// pagesMatch is AllPagesMatch: every page in the range passes pred.
func pagesMatch(vmo *ObjectPaged, pred func(*VmPage) bool, offset, length uint64) bool {
	matches := true
	err := vmo.Lookup(offset, length, func(_ uint64, page *VmPage) error {
		if !pred(page) {
			matches = false
			return ErrStop
		}
		return nil
	})
	return err == nil && matches
}

// pagesInAnyAnonymousQueue is PagesInAnyAnonymousQueue.
func pagesInAnyAnonymousQueue(env *vmoEnv, vmo *ObjectPaged, offset, length uint64) bool {
	return pagesMatch(vmo, env.node.PageQueues().DebugPageIsAnyAnonymous, offset, length)
}

// expectAttribution checks an object's attribution.
func expectAttribution(t *testing.T, what string, got, want AttributionCounts) {
	t.Helper()
	if got != want {
		t.Errorf("%s: attributed %+v, want %+v", what, got, want)
	}
}

// expectContinuousAttribution is verify_continuous_attribution_bytes: the
// populated slots tracked are the bytes expected.
func expectContinuousAttribution(t *testing.T, env *vmoEnv, vmo *ObjectPaged, bytes uint64) {
	t.Helper()
	if got := uint64(vmo.DebugGetCowPages().DebugGetPopulatedSlotsCount()) * env.ps; got != bytes {
		t.Errorf("continuous attribution: tracked %#x bytes, want %#x", got, bytes)
	}
}

// expectReclaim checks a page's reclaim queue and its age.
func expectReclaim(t *testing.T, env *vmoEnv, what string, page *VmPage, wantQueue uint64) {
	t.Helper()
	ok, queue := env.node.PageQueues().DebugPageIsReclaim(page)
	if !ok || queue != wantQueue {
		t.Errorf("%s: in a reclaim queue %v, queue %d; want true, %d", what, ok, queue, wantQueue)
	}
}

// putUint64 is the 8 bytes of v, as Zircon's cases write a uint64_t.
func putUint64(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}
