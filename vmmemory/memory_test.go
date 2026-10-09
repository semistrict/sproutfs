package vmmemory_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testarena"
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

// suiteArena is the arena mode the fixtures build their pagers in, which
// SPROUTFS_ARENA names. The suite is run once in each.
var suiteArena vmmemory.ArenaMode

// TestMain runs the whole suite once per page. A failure names the page and the
// arena mode it happened at, because the test names cannot.
func TestMain(m *testing.M) {
	suiteArena = testarena.MustMode()
	for _, size := range pageSizes {
		pageSize = size
		if code := m.Run(); code != 0 {
			fmt.Fprintf(os.Stderr, "vmmemory: the suite failed with the pager's page at %d bytes in a %s arena\n",
				size, suiteArena)
			os.Exit(code)
		}
	}
	os.Exit(0)
}

// errInjected is the failure a test makes a backing or an arena report.
var errInjected = errors.New("injected failure")

// arena is the fixture's page store: every file the pager makes of it. Its
// slots are a map and not one entry per offset, because a real arena is a
// sparse file: it has more addresses than it may hold pages at once, an offset
// costs nothing until a page is put there, and releasing one punches that
// memory back out. held is how many offsets hold a page, in every file — the
// memory the arena is really holding, which is what the pager's budget bounds
// and what an offset space larger than that budget must not change.
//
// It also holds the pager to who may read what. A file given writable is one
// memory region's private file: it is given to that one mapping only, as its
// file 0, and never to anyone read-only. Every other file is only ever given
// read-only. A map names a file its mapping holds, and a writable map names
// file 0.
type arena struct {
	mu       sync.Mutex
	pageSize int
	// isolated is an arena of a pager that splits it by who may read each
	// page, which is the one whose readers the arena holds it to.
	isolated bool
	files    []*arenaFile
	held     int
	peak     int
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

// place is one slot of one file of the arena, the file by the order the pager
// made it in.
type place struct{ file, slot int }

// next is the slot after this one, in the same file.
func (p place) next() place { return place{p.file, p.slot + 1} }

// arenaFile is one file of the fixture's arena.
type arenaFile struct {
	arena   *arena
	id      int
	offsets int
	slots   map[int][]byte
	// writer is the one mapping this file was given to writable, and readers
	// how many were given it read-only, all of them of tenant unless public.
	// public marks the public file, which every tenant is given as file 2 and
	// which is never given as anything else. closed marks a file the pager gave
	// back.
	writer  *mapping
	readers int
	tenant  string
	public  bool
	closed  bool
}

func newArena(pageSize int) *arena { return &arena{pageSize: pageSize} }

// File makes a file of offsets slots, every one of them a hole. In a campaign
// it fails at random, as making a memfd and mapping it does where the process
// is out of descriptors or memory (EMFILE, ENOMEM).
func (a *arena) File(ctx context.Context, offsets int) (vmmemory.ArenaFile, error) {
	if sim.Buggify(ctx, "vmmemory-test/arena-out-of-memory/file", 0.05) {
		return nil, errInjected
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f := &arenaFile{arena: a, id: len(a.files), offsets: offsets, slots: make(map[int][]byte)}
	a.files = append(a.files, f)
	return f, nil
}

// addresses is how many offsets of every file hold a page.
func (a *arena) addresses() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for _, f := range a.files {
		count += len(f.slots)
	}
	return count
}

// page is the bytes one place holds, nil for a hole. The caller holds a.mu,
// or knows nothing else runs.
func (a *arena) page(at place) []byte { return a.files[at.file].slots[at.slot] }

// pageUnder is page under a.mu, for a caller a prefetch may run beside.
func (a *arena) pageUnder(at place) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.page(at)
}

// fixture is this file, which is what a file a test wraps is too.
func (f *arenaFile) fixture() *arenaFile { return f }

// at refuses an address this file does not have, and a file the pager has
// given back.
func (f *arenaFile) at(slot int) (place, error) {
	if slot < 0 || slot >= f.offsets {
		return place{}, fmt.Errorf("arena offset %d is outside its %d", slot, f.offsets)
	}
	if f.closed {
		return place{}, fmt.Errorf("arena file %d was closed", f.id)
	}
	return place{f.id, slot}, nil
}

// put and drop keep the count of offsets holding a page, which is the memory.
func (f *arenaFile) put(slot int, data []byte) {
	if f.slots[slot] == nil {
		f.arena.held++
		f.arena.peak = max(f.arena.peak, f.arena.held)
	}
	f.slots[slot] = data
}
func (f *arenaFile) drop(slot int) {
	if f.slots[slot] != nil {
		f.arena.held--
	}
	delete(f.slots, slot)
}

func (f *arenaFile) Read(_ context.Context, slot int, dst []byte) error {
	a := f.arena
	if a.onRead != nil {
		a.onRead(slot)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := f.at(slot); err != nil {
		return err
	}
	// An offset no page has been put at is a hole, and a hole reads as zeros.
	clear(dst)
	copy(dst, f.slots[slot])
	return nil
}

// Write fills a punched slot. In a campaign it fails at random, as the Linux
// arena's allocation does where the pod or the HugeTLB pool has no memory for
// the page (ENOMEM, ENOSPC), before anything is written.
func (f *arenaFile) Write(ctx context.Context, slot int, src []byte) error {
	if sim.Buggify(ctx, "vmmemory-test/arena-out-of-memory/write", 0.002) {
		return errInjected
	}
	a := f.arena
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := f.at(slot); err != nil {
		return err
	}
	if a.failWrite {
		return errInjected
	}
	if f.slots[slot] != nil {
		return fmt.Errorf("write into allocated slot %d of file %d", slot, f.id)
	}
	f.put(slot, bytes.Clone(src))
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
func (f *arenaFile) Zero(ctx context.Context, slot, count int) error {
	// The allocation fails at random as Write's does, before any slot of the
	// run holds a page.
	if sim.Buggify(ctx, "vmmemory-test/arena-out-of-memory/zero", 0.05) {
		return errInjected
	}
	a := f.arena
	a.mu.Lock()
	defer a.mu.Unlock()
	for s := slot; s < slot+count; s++ {
		if _, err := f.at(s); err != nil {
			return err
		}
		if f.slots[s] != nil {
			return fmt.Errorf("zero of allocated slot %d of file %d", s, f.id)
		}
		f.put(s, make([]byte, a.pageSize))
	}
	a.zeroed++
	return nil
}
func (f *arenaFile) Release(_ context.Context, slot int) error {
	a := f.arena
	a.mu.Lock()
	defer a.mu.Unlock()
	at, err := f.at(slot)
	if err != nil {
		return err
	}
	for i, m := range a.mappings {
		for page, p := range m.pages {
			if p.place == at {
				return fmt.Errorf("release of mapped slot %d of file %d, which mapping %d maps at page %d",
					slot, f.id, i, page)
			}
		}
	}
	f.drop(slot)
	return nil
}

// AllocatedBytes is the memory the file holds, every page put at it whoever
// put it there.
func (f *arenaFile) AllocatedBytes() (uint64, error) {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	return uint64(len(f.slots) * f.arena.pageSize), nil
}

// Punch gives back every page at a slot held says holds none.
func (f *arenaFile) Punch(held func(slot int) bool) error {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	for slot := range f.slots {
		if !held(slot) {
			f.drop(slot)
		}
	}
	return nil
}

// Close gives the file back, which the pager does only once nothing of it is
// held.
func (f *arenaFile) Close() error {
	a := f.arena
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(f.slots) != 0 {
		return fmt.Errorf("arena file %d closed holding %d slots", f.id, len(f.slots))
	}
	f.closed = true
	return nil
}

// mapped is what one page of a mapping maps. writable is whether a store lands
// without trapping, and mappedWritable whether the command that mapped it
// allowed stores at all: a write-protection takes the first away and leaves
// the second, and resolving the page writable gives the first back.
type mapped struct {
	place
	writable, mappedWritable bool
}

// publicFile is the number every session of an isolated arena is given the
// public file under.
const publicFile = 2

type mapping struct {
	arena *arena
	// tenant is the tenant of the memory region this is the mapping of.
	tenant string
	// injected is set once this client injected a fault its region cannot
	// survive: a command whose answer it lost, or a revocation it refused.
	injected atomic.Bool
	pages    map[uint64]mapped
	// files is every file this mapping was given, by the number its maps name
	// it by.
	files                            map[int]*arenaFile
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

func newMapping(a *arena) *mapping {
	return &mapping{arena: a, pages: make(map[uint64]mapped), files: make(map[int]*arenaFile)}
}

// mappedPage is what the mapping maps at page, read under the arena's lock: a
// prefetch maps pages from a goroutine of its own.
func (m *mapping) mappedPage(page uint64) (mapped, bool) {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	p, ok := m.pages[page]
	return p, ok
}

// GiveFile holds the pager to who may read a file: a file given writable is
// one memory region's own, as its file 0, and nobody else's in any way, and a
// file given read-only is given to one tenant's memory regions only, except
// the public file, which is every region's file 2 and nothing else.
func (m *mapping) GiveFile(ctx context.Context, number int, file vmmemory.ArenaFile, writable bool) error {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	fixture, ok := file.(interface{ fixture() *arenaFile })
	if !ok {
		return fmt.Errorf("file %d is a %T, not this arena's", number, file)
	}
	f := fixture.fixture()
	switch {
	case m.files[number] != nil:
		return fmt.Errorf("file %d given twice", number)
	case writable != (number == 0):
		return fmt.Errorf("file %d given writable=%t, and only file 0 is writable", number, writable)
	case !m.arena.isolated:
		// A shared arena is one file, and every memory region's.
	case writable && (f.writer != nil || f.readers > 0):
		return fmt.Errorf("file %d given writable to a second memory region", f.id)
	case !writable && f.writer != nil:
		return fmt.Errorf("the private file %d given read-only to another memory region", f.id)
	case !writable && f.readers > 0 && f.public != (number == publicFile):
		return fmt.Errorf("file %d given as file %d, and as the public file elsewhere %t", f.id, number, f.public)
	case !writable && f.readers > 0 && !f.public && f.tenant != m.tenant:
		return fmt.Errorf("file %d given read-only to tenant %q and to tenant %q", f.id, f.tenant, m.tenant)
	}
	if writable {
		f.writer = m
	} else {
		f.readers++
		f.tenant = m.tenant
		f.public = number == publicFile
	}
	m.files[number] = f
	if m.commandLost(ctx, "give-file") {
		return errInjected
	}
	return nil
}

func (m *mapping) DropFile(ctx context.Context, number int) error {
	if m.commandLost(ctx, "drop-file") {
		return errInjected
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	f := m.files[number]
	if f == nil {
		return fmt.Errorf("drop of file %d, which was never given", number)
	}
	for page, p := range m.pages {
		if p.file == f.id && p.slot >= 0 {
			return fmt.Errorf("drop of file %d while page %d maps it", number, page)
		}
	}
	delete(m.files, number)
	return nil
}

// Map replaces whatever the pages had with the slots, atomically, as the
// client's mremap does. A slot must hold contents: Linux cannot install a
// punched one. The file must be one this mapping was given, and only its
// private file may be mapped writable.
func (m *mapping) Map(ctx context.Context, page uint64, file, slot, count int, writable bool) error {
	if m.onMap != nil {
		m.onMap(page, count)
	}
	if err := m.mappable(file, writable); err != nil {
		return err
	}
	if m.refuseMap || m.outOfMappings(ctx, "map") {
		return vmmemory.ErrMappingRefused
	}
	if err := m.applyMap(page, file, slot, count, writable); err != nil {
		return err
	}
	if m.failMap || m.commandLost(ctx, "map") {
		return errInjected
	}
	return nil
}
func (m *mapping) MapZero(ctx context.Context, page uint64, count int) error {
	if m.refuseMap || m.outOfMappings(ctx, "map-zero") {
		return vmmemory.ErrMappingRefused
	}
	m.applyZero(page, count)
	if m.failMap || m.commandLost(ctx, "map-zero") {
		return errInjected
	}
	return nil
}

// mappable refuses a map of a file this mapping was not given, or a writable
// map of any file but its private one.
func (m *mapping) mappable(file int, writable bool) error {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	if m.files[file] == nil || (writable && file != 0) {
		return fmt.Errorf("map of file %d writable=%t, which this memory region may not map so", file, writable)
	}
	return nil
}

// applyMap installs count slots of a file this mapping was given from page,
// which every one of them must hold contents for.
func (m *mapping) applyMap(page uint64, file, slot, count int, writable bool) error {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	f := m.files[file]
	for i := range count {
		if f.slots[slot+i] == nil {
			return fmt.Errorf("map of punched slot %d of file %d", slot+i, f.id)
		}
	}
	for i := range count {
		m.pages[page+uint64(i)] = mapped{place{f.id, slot + i}, writable, writable}
	}
	m.maps++
	return nil
}

// applyZero installs count pages of zeros from page.
func (m *mapping) applyZero(page uint64, count int) {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)] = mapped{place{-1, -1}, false, false}
	}
	m.maps++
}

// batchedMapping is the client production has: one that takes a fault's runs
// and an eviction's revocations in batches, which is the path every fault and
// every eviction of a real host takes. A batch is refused before it touches
// anything, as a real one is while none of its commands has landed, and is
// otherwise applied whole; one whose answer is lost is applied, and a lost
// revocation is not.
type batchedMapping struct{ *mapping }

func (m batchedMapping) MapBatch(ctx context.Context, runs []vmmemory.MapRun) (int, int, error) {
	for _, run := range runs {
		if !run.Zero {
			if err := m.mappable(run.File, false); err != nil {
				return 0, 0, err
			}
		}
	}
	if m.refuseMap || m.outOfMappings(ctx, "map-batch") {
		return 0, 0, vmmemory.ErrMappingRefused
	}
	for _, run := range runs {
		if run.Zero {
			m.applyZero(run.Page, run.Count)
		} else if err := m.applyMap(run.Page, run.File, run.Slot, run.Count, false); err != nil {
			return 0, 0, err
		}
	}
	if m.failMap || m.commandLost(ctx, "map-batch") {
		return 1, len(runs), errInjected
	}
	return 1, len(runs), nil
}

func (m batchedMapping) RevokeBatch(ctx context.Context, runs []vmmemory.PageRun) (int, int, error) {
	if m.refuseRevoke || m.outOfMappings(ctx, "revoke-batch") {
		return 0, 0, vmmemory.ErrMappingRefused
	}
	lost := m.failRevoke || m.commandLost(ctx, "revoke-batch")
	m.arena.mu.Lock()
	m.revokes++
	if !lost {
		for _, run := range runs {
			for i := range run.Count {
				delete(m.pages, run.Page+uint64(i))
			}
		}
	}
	m.arena.mu.Unlock()
	if lost {
		return 1, len(runs), errInjected
	}
	return 1, len(runs), nil
}

// outOfMappings is a client that has run out of mapping budget, which refuses
// a command before it touches anything. A real client refuses whenever its
// process nears its VMA limit, which a 4 KiB disk written in scattered places
// reaches in minutes; in a controlled run any command may be refused, so the
// campaigns take every path a refusal leads down. Each command has a site of
// its own (scripts/faults/vmmemory.json). A refused revocation is terminal
// for the region, which no revocation can be served again, so the client
// counts it as a fault it injected (injected).
func (m *mapping) outOfMappings(ctx context.Context, command string) bool {
	revocation := command == "revoke" || command == "revoke-batch"
	// A fault maps zeros a few times a run, so its refusal has the larger
	// chance, as the commands a run issues rarely have for a lost answer.
	p := 0.05
	if command == "map-zero" {
		p = 0.3
	}
	if !sim.Buggify(ctx, "vmmemory-test/client-out-of-mappings/"+command, p) {
		return false
	}
	if revocation {
		m.injected.Store(true)
	}
	return true
}

// commandLost is a client command whose answer never came: the pager cannot
// tell whether the client applied it, so the region is terminal from here, as
// a session that times out on a command is. A map or a file given is applied
// before its answer is lost, and a revoke, a protect, a resolve or a file
// dropped is not, which is the way round each can do harm: a page mapped that
// the pager may think is not, and one it may think is gone. Each command has a
// site of its own (scripts/faults/vmmemory.json), and the ones a run issues a
// few times have the larger chance, so a campaign reaches every one: a file
// given, or zeros mapped, a few times a run, and a file dropped only as a fork
// point's seal ends.
func (m *mapping) commandLost(ctx context.Context, command string) bool {
	p := 0.002
	switch command {
	case "give-file", "map-zero":
		p = 0.05
	case "drop-file":
		p = 0.5
	}
	if !sim.Buggify(ctx, "vmmemory-test/client-command-lost/"+command, p) {
		return false
	}
	m.injected.Store(true)
	return true
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
	if m.failProtect || m.commandLost(ctx, "protect") {
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
func (m *mapping) Revoke(ctx context.Context, page uint64) error {
	if m.refuseRevoke || m.outOfMappings(ctx, "revoke") {
		return vmmemory.ErrMappingRefused
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	m.revokes++
	if m.failRevoke || m.commandLost(ctx, "revoke") {
		return errInjected
	}
	delete(m.pages, page)
	return nil
}

// Resolve installs a run's page tables. Resolving writable also takes off a
// write-protection, as the real one's UFFDIO_WRITEPROTECT does, which only a
// page mapped writable can have; resolving read-only is only valid for a page
// a store would trap on.
func (m *mapping) Resolve(ctx context.Context, page uint64, count int, writable bool) error {
	if m.onResolve != nil {
		m.onResolve(page)
	}
	if m.commandLost(ctx, "resolve") {
		return errInjected
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	for i := range count {
		p, ok := m.pages[page+uint64(i)]
		if !ok || (writable && !p.mappedWritable) || (!writable && p.writable) {
			return errors.New("invalid resolution")
		}
	}
	if writable {
		for i := range count {
			p := m.pages[page+uint64(i)]
			p.writable = true
			m.pages[page+uint64(i)] = p
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
	mu       sync.Mutex
	pageSize int // the pager page; the unit of private, zero and every extent
	data     []byte
	source   control.Ref // the checkpoint untouched pages are inherited from
	// sources names another checkpoint an untouched page is inherited from,
	// which a volume forked from one checkpoint and then published has.
	sources              map[uint64]control.Ref
	owner                string // this backing's VM identity, for private pages
	sequence             uint64 // the checkpoint private pages will be published under
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

// Load reads the backing's bytes. In a campaign it fails at random, as a
// volume's read does where its object store or its cache cannot answer, or
// its VM has ended (ErrNeedsRecovery); each of a backing's read paths has a
// site of its own (scripts/faults/vmmemory.json).
func (b *backing) Load(ctx context.Context, off uint64, dst []byte) error {
	if b.failRead || sim.Buggify(ctx, "vmmemory-test/backing-read-fails/load", 0.02) {
		return errInjected
	}
	return b.read(off, dst)
}

// read reads the backing's bytes, as every read path of it does once it has
// not failed.
func (b *backing) read(off uint64, dst []byte) error {
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
	case b.sources[page] != (control.Ref{}):
		return control.Identity{Ref: b.sources[page], Volume: "v", Page: number}
	default:
		return control.Identity{Ref: b.source, Volume: "v", Page: number}
	}
}

// Locate reports the identities of a range. In a campaign it fails at random,
// as a volume's does where the segments of the checkpoint index it fetches
// cannot be read.
func (b *backing) Locate(ctx context.Context, off, length uint64) ([]control.Extent, error) {
	if sim.Buggify(ctx, "vmmemory-test/backing-read-fails/locate", 0.001) {
		return nil, errInjected
	}
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

// Verify fails where a test says, and in a campaign at random, as a volume's
// does once its VM is not this host's any more (ErrHandedOff, ErrClosed) or
// has ended (ErrNeedsRecovery).
func (b *backing) Verify(ctx context.Context) error {
	if b.failVerify || sim.Buggify(ctx, "vmmemory-test/backing-not-owned/verify", 0.02) {
		return errInjected
	}
	return nil
}

type fixture struct {
	t *testing.T
	// ctx is the context the pager was built under. It carries the fixture's
	// simulated runtime, so a call made under it consults the in-tree bug
	// guards SPROUTFS_SIM_BUG names, which a call under a bare test context
	// never does.
	ctx      context.Context
	h        *vmmemory.Host
	a        *arena
	disk     *sim.Disk
	spill    platform.File
	pageSize int
	source   control.Ref // the checkpoint every memory region of the fixture inherits
	owners   int
	// batched attaches every memory region with the client production has,
	// which takes runs in batches (batchedMapping). A campaign sets it on
	// half its seeds, so both of the pager's paths are taken.
	batched bool
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
// not care about the geometry is exercised at both, and one that names no
// arena mode takes the suite's.
func newConfiguredFixture(t *testing.T, cfg vmmemory.Config, shared ...*resource.Budget) *fixture {
	t.Helper()
	if cfg.Arena == vmmemory.ArenaIsolated {
		cfg.Arena = suiteArena
	}
	return newPinnedFixture(t, cfg, shared...)
}

// newPinnedFixture is newConfiguredFixture in the arena mode the configuration
// names, whatever the suite's: a test of what one mode does.
func newPinnedFixture(t *testing.T, cfg vmmemory.Config, shared ...*resource.Budget) *fixture {
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
	runtime := sim.New(sim.Config{})
	return newFixtureOn(t, sim.WithRuntime(t.Context(), runtime), runtime.NewDisk("pager", sim.DiskConfig{}),
		cfg, shared...)
}

// newFixtureOn is newBrokenFixture with its spill file on disk, and its pager
// built under ctx, which carries the runtime whose guards the pager consults.
func newFixtureOn(t *testing.T, ctx context.Context, disk *sim.Disk, cfg vmmemory.Config,
	shared ...*resource.Budget) (*fixture, error) {
	t.Helper()
	spill, err := disk.Open(ctx, "spill", platform.OpenOptions{Create: true})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = spill.Close() })
	a := newArena(int(cfg.PageSize))
	a.isolated = cfg.Arena == vmmemory.ArenaIsolated
	resources := testresource.New()
	if len(shared) != 0 {
		resources = shared[0]
	}
	h, err := vmmemory.New(ctx, resources, cfg, a, spill)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		// A campaign's disk may fail giving the spill file back.
		if err := h.Close(context.Background()); err != nil && !injected(err) {
			t.Error(err)
		}
	})
	return &fixture{t: t, ctx: ctx, h: h, a: a, disk: disk, spill: spill, pageSize: int(cfg.PageSize),
		source: control.Ref{VM: vmName(t), Sequence: 1}}, nil
}

// vmName is the name of a VM of no tenant, after the test. A subtest's name
// has slashes in it, and the first slash of a VM's identity ends its tenant.
func vmName(t testing.TB) string { return strings.ReplaceAll(t.Name(), "/", "-") }

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
	return f.attachBacking(vmmemory.MemoryRegionBacking{Kind: kind, Backing: b})
}

// attachBacking maps one memory region as whoever attaches it states it.
func (f *fixture) attachBacking(backing vmmemory.MemoryRegionBacking) (*vmmemory.MemoryRegion, *mapping) {
	f.t.Helper()
	r, m, err := f.tryAttachBacking(backing)
	if err != nil {
		f.t.Fatal(err)
	}
	return r, m
}

// tryAttachBacking is attachBacking for a campaign, in which an attach may
// fail of a fault it injected, as a machine that never started.
func (f *fixture) tryAttachBacking(backing vmmemory.MemoryRegionBacking) (*vmmemory.MemoryRegion, *mapping, error) {
	m := newMapping(f.a)
	m.tenant = backing.Tenant
	f.a.mappings = append(f.a.mappings, m)
	var client vmmemory.Mapping = m
	if f.batched {
		client = batchedMapping{m}
	}
	r, err := f.h.Attach(f.ctx, backing, client)
	if r == nil {
		return nil, nil, err
	}
	// An attach that failed after its region was admitted returns it, and its
	// owner detaches it, as a session that failed to connect does.
	f.t.Cleanup(func() {
		// A prefetch a failed test left running may still map and resolve
		// pages until the detach waits for it, under the arena's lock.
		m.arena.mu.Lock()
		clear(m.pages)
		m.arena.mu.Unlock()
		if err := r.Detach(context.Background()); err != nil {
			f.t.Error(err)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return r, m, nil
}

// access is what the guest sees of one page: the page it maps, faulted in
// first where it maps none, or maps it read-only and stores.
func access(t *testing.T, r *vmmemory.MemoryRegion, m *mapping, page uint64, write bool) []byte {
	t.Helper()
	return accessUnder(t.Context(), t, r, m, page, write)
}

// accessUnder is access with its fault made under ctx, which may carry the
// runtime whose guards the fault's path consults. The guest it models reads
// on only once the rest of the fault's run is in: it waits for the prefetch
// the fault started. The tests of prefetch itself fault without waiting.
// accessFaults is how many faults one access may take: a store trap a fault
// served read-only, and the protect trap that follows it.
const accessFaults = 2

func accessUnder(ctx context.Context, t *testing.T, r *vmmemory.MemoryRegion, m *mapping, page uint64,
	write bool) []byte {
	t.Helper()
	p, ok := m.mappedPage(page)
	// The access retries after each fault, as a vCPU does: a store trap served
	// read-only traps again on the read-only mapping.
	for faults := 0; !ok || (write && !p.writable); faults++ {
		if faults == accessFaults {
			t.Fatalf("page %d is still not mapped for its access after %d faults", page, faults)
		}
		if err := r.Fault(ctx, page, write); err != nil {
			t.Fatal(err)
		}
		if err := r.SettlePrefetches(ctx); err != nil {
			t.Fatal(err)
		}
		p, ok = m.mappedPage(page)
	}
	if p.slot == -1 {
		return make([]byte, m.arena.pageSize)
	}
	return m.arena.pageUnder(p.place)
}

func TestSharingCOWReclaimAndDurability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 4)
		a, am, ab := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		access(t, a, am, 0, false)
		access(t, b, bm, 0, false)
		if am.pages[0].place != bm.pages[0].place {
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

// A private page an eviction takes is spilled, and the fault that brings it
// back reads the guest's own store out of the spill file rather than whatever
// its slot held before. The second round spills to a slot the first one gave
// back. The faults run under the fixture's runtime, so a spill guard
// SPROUTFS_SIM_BUG names is on in them.
func TestASpilledPageFaultsBackWhatTheGuestStored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 4)
		r, m, b := f.memoryRegion(4)
		for round, value := range []byte{99, 100} {
			accessUnder(f.ctx, t, r, m, 0, true)[0] = value
			// Two more pages than the arena holds besides page zero, so the
			// least recently used of them, page zero, is evicted.
			accessUnder(f.ctx, t, r, m, 1, false)
			accessUnder(f.ctx, t, r, m, 2, false)
			if s := hostStats(t, f); s.Spills != uint64(round+1) {
				t.Fatalf("round %d spilled %d pages in all, want %d", round, s.Spills, round+1)
			}
			if _, mapped := m.pages[0]; mapped {
				t.Fatalf("round %d: the guest still maps the page the eviction took", round)
			}
			if got := accessUnder(f.ctx, t, r, m, 0, false)[0]; got != value {
				t.Fatalf("round %d read back %d, want the guest's own store of %d", round, got, value)
			}
			if b.data[0] != 1 {
				t.Fatalf("round %d: the spill reached the volume, which holds %d", round, b.data[0])
			}
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
		if am.pages[0].place != bm.pages[0].place {
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
				data := m.arena.page(p.place)
				if value != nil {
					data[0] = *value
				}
				result = data[0]
			}
			m.arena.mu.Unlock()
			return result, nil
		}
		m.arena.mu.Unlock()
		// A fault refused for want of mapping budget is served again, as the
		// connection serves it again once it has made room (makeRoom).
		if err := r.Fault(ctx, page, value != nil); err != nil && !errors.Is(err, vmmemory.ErrMappingRefused) {
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
