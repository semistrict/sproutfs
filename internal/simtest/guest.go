package simtest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/testbacking"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// A volume is a backing a fault can ask for the window it needs, with the pages
// its region already holds left out. It is asserted here because this is where
// the two packages meet: a volume that stopped being one would cost every fault
// a request per stretch of its window and nothing would fail.
var _ vmmemory.SparseLoader = (*volume.Volume)(nil)

// stateBytes is the simulated VMM state a migration carries: the guest's
// current value and how many stores it has made. A destination that restored it
// continues exactly where the source stopped, and one that lost it says so.
const stateBytes = 9

// arena is one pager's shared page store: a byte slice at every offset a page
// has been put at, which is what a real pager's shared memory is. A host has
// one per pager, each of that pager's own page.
//
// It is a map rather than one entry per offset, because a real arena is a
// sparse file: it has more addresses than it may ever hold pages at once, an
// offset costs nothing until a page is put there, and releasing one punches
// that memory back out. held is how many offsets hold a page, which is the
// memory this arena is really holding and what its pager's budget bounds.
type arena struct {
	mu      sync.Mutex
	offsets int
	slots   map[int][]byte
	held    int
	peak    int
}

func newArena(offsets int) *arena {
	return &arena{offsets: offsets, slots: make(map[int][]byte)}
}

// at refuses an address this arena does not have, which is the check a real
// arena's own bounds make.
func (a *arena) at(slot int) error {
	if slot < 0 || slot >= a.offsets {
		return fmt.Errorf("arena offset %d is outside its %d", slot, a.offsets)
	}
	return nil
}

func (a *arena) Read(_ context.Context, slot int, dst []byte) error {
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
	if a.slots[slot] != nil {
		return fmt.Errorf("write into allocated slot %d", slot)
	}
	a.slots[slot] = bytes.Clone(src)
	a.held++
	a.peak = max(a.peak, a.held)
	return nil
}

// Equal compares two slots where they are, as a real arena mapped into this
// process does, so a settle costs the comparison and no copy.
func (a *arena) Equal(_ context.Context, first, second int) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return bytes.Equal(a.slots[first], a.slots[second]), nil
}

func (a *arena) Release(_ context.Context, slot int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.at(slot); err != nil {
		return err
	}
	if a.slots[slot] != nil {
		a.held--
	}
	delete(a.slots, slot)
	return nil
}

type mapped struct {
	slot     int
	writable bool
}

// mapping is one simulated process region's page table. Every lookup takes the
// arena lock, because a guest storing into a page races the seal that
// write-protects it: the store and the writability check must be one step,
// exactly as the hardware makes them.
type mapping struct {
	arena *arena
	mu    sync.Mutex
	pages map[uint64]mapped
}

func newMapping(a *arena) *mapping { return &mapping{arena: a, pages: make(map[uint64]mapped)} }

// mappings counts the mappings this region is to its VMM process, which is what
// the kernel holds a VMA for: a run of consecutive pages at consecutive arena
// offsets, with the same write access, is one mapping, and every break in
// either is another. It is what the placement rule exists to keep down — a
// private page put at the offset it has within its range keeps the private
// pages of that range adjacent, so what the range costs here is how often it
// alternates between shared and private rather than how many of its pages are
// private.
func (m *mapping) mappings() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return mappingRuns(m.pages)
}

// mappingRuns counts the mappings one page table's arena-backed pages are. A
// zero mapping owns no arena offset and is left out: what the placement rule
// governs is where the pages a pager holds sit, and a range of zeros is one
// mapping wherever they are.
func mappingRuns(pages map[uint64]mapped) int {
	numbers := make([]uint64, 0, len(pages))
	for page, m := range pages {
		if m.slot >= 0 {
			numbers = append(numbers, page)
		}
	}
	slices.Sort(numbers)
	count := 0
	for i, page := range numbers {
		if i > 0 {
			previous, current := pages[numbers[i-1]], pages[page]
			if numbers[i-1]+1 == page && current.slot == previous.slot+1 &&
				previous.writable == current.writable {
				continue
			}
		}
		count++
	}
	return count
}

func (m *mapping) Map(_ context.Context, page uint64, slot, count int, writable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)] = mapped{slot + i, writable}
	}
	return nil
}

func (m *mapping) MapZero(_ context.Context, page uint64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)] = mapped{-1, false}
	}
	return nil
}

func (m *mapping) Protect(_ context.Context, page uint64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pages, page)
	return nil
}

func (m *mapping) Resolve(_ context.Context, page uint64, count int, writable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		p, ok := m.pages[page+uint64(i)]
		if !ok || p.writable != writable {
			return errors.New("invalid resolution")
		}
	}
	return nil
}

// store writes one page's bytes the way a guest does: only a mapping that is
// writable right now accepts the store. A page a seal has write-protected
// reports false, which is the trap the caller answers with a write fault.
func (m *mapping) store(page uint64, value byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[page]
	if !ok || !p.writable || p.slot < 0 {
		return false
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	slot := m.arena.slots[p.slot]
	for i := range slot {
		slot[i] = value
	}
	return true
}

// guest is the simulated VMM process of one VM: one region per volume, a guest
// that stores into them through their mappings, and the migration Runtime the
// coordinator drives.
//
// It has no life of its own: a store happens when the driver asks for one, so
// what the model holds is exact at every moment the driver looks at it. A
// guest that stored in a loop of its own would make every assertion a race
// against that loop rather than against the fault under test.
type guest struct {
	instance string
	// id names this VMM process apart from every other one of the same VM: the
	// host it runs on and that host's incarnation, which is what makes a
	// restarted host's guest a different logical caller from the dead one's.
	id  string
	ctx context.Context
	// admit is what every concurrent region seal passes through, and reverse
	// the order they are created in. Both are the recorded scenario's; a
	// campaign that chooses no completion order has neither.
	admit   func(ctx context.Context, id string) error
	reverse bool
	// captures counts the pauses this process has taken, which names one
	// capture's seals apart from the next one's.
	captures int

	names    []string
	pages    map[string]int
	regions  map[string]*vmmemory.Region
	mappings map[string]*mapping
	// pageBytes is the page each of this guest's volumes is mapped in, which is
	// the page of the pager holding that region. A VM's memory and its disks are
	// two pagers of two pages, so every byte offset here is per volume.
	pageBytes map[string]int

	mu sync.Mutex
	// model is the byte the guest last stored into every page of every volume,
	// which is what any later read of that page must return.
	model  map[string][]byte
	writes int64
	value  byte
	// refuseStop is the fault that fails a migration's pause after the guest
	// has already stopped: the release must unseal the regions and leave the
	// guest running again.
	refuseStop error
	// stopped reports vCPUs that are not running. A guest stores nothing while
	// it is stopped, which is what makes a capture or an abandoned migration
	// that never resumed it visible: the next store fails rather than quietly
	// succeeding against pages nothing is driving.
	stopped bool
	closed  bool
}

// newGuest attaches one region per volume of vm through backing, which is the
// volume itself on a source and a peer-backed volume on a destination. Every
// region enters the simulator through a stable identity of its own — the VM,
// the host running it and that host's incarnation — so two concurrent
// identical reads are ordered by their logical caller rather than by
// completion.
func (w *World) newGuest(h *hostState, p *pager, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (*guest, error) {
	ctx := w.ctx
	g := &guest{instance: vm.ID(), ctx: ctx, pages: map[string]int{},
		regions: map[string]*vmmemory.Region{}, mappings: map[string]*mapping{},
		pageBytes: map[string]int{},
		model:     map[string][]byte{}, admit: w.config.Admit, reverse: w.config.ReverseRegions,
		id: fmt.Sprintf("%s/%s/%d/%d", vm.ID(), h.name, h.incarnation, w.nextGuest())}
	for _, v := range vm.Volumes() {
		name := v.Name()
		var backing vmmemory.Backing = v
		if supplied, ok := backings[name]; ok {
			backing = supplied
		}
		// The simulated machine binds the same shapes the real one does: one
		// RAM volume, and every other volume a PMEM disk. Each attaches to the
		// pager of its own kind, over that pager's own arena.
		kind := vmmemory.Pmem
		if name == MemoryVolume {
			kind = vmmemory.Ram
		}
		pager := p.pagers.For(kind)
		mp := newMapping(p.arenaOf(kind))
		admitted, _ := testbacking.New(backing, p.runtime, g.id+"/"+name)
		region, err := pager.Attach(ctx, vmmemory.RegionBacking{Kind: kind, Backing: admitted}, mp)
		if err != nil {
			return nil, err
		}
		g.names = append(g.names, name)
		g.regions[name] = region
		g.mappings[name] = mp
		g.pageBytes[name] = int(pager.PageSize())
		g.pages[name] = int(v.Size() / pager.PageSize())
		g.model[name] = make([]byte, v.Size())
	}
	switch len(state) {
	case 0:
	case stateBytes:
		// The VMM state carries the guest's own counters, so a destination that
		// restored it continues exactly where the source stopped.
		g.value = state[0]
		g.writes = int64(binary.LittleEndian.Uint64(state[1:]))
	default:
		return nil, fmt.Errorf("simtest: restored %d state bytes, want %d", len(state), stateBytes)
	}
	return g, nil
}

// Regions reports this process's memory by volume name, which is what a
// migration hands over and a capture seals.
func (g *guest) Regions() map[string]*vmmemory.Region {
	result := make(map[string]*vmmemory.Region, len(g.regions))
	for name, region := range g.regions {
		result[name] = region
	}
	return result
}

// Prepare is a capture's pause: the guest stops storing, its state is captured,
// and every region seals the pages the checkpoint will publish.
func (g *guest) Prepare(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
	state, err := g.Stop(ctx)
	if err != nil {
		return nil, nil, err
	}
	g.captures++
	names := slices.Clone(g.names)
	if g.reverse {
		slices.Reverse(names)
	}
	if g.admit == nil {
		sources := make(map[string]volume.DirtySource, len(g.regions))
		for _, name := range names {
			if err := g.regions[name].Seal(ctx); err != nil {
				return nil, nil, err
			}
			sources[name] = g.regions[name].Checkpoint()
		}
		return state, sources, nil
	}
	// Every region seals concurrently, each admitted as a caller of its own, so
	// the order they seal in is the scheduler's rather than the order the
	// goroutines happened to be created in.
	sources := map[string]volume.DirtySource{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var result error
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.admit(ctx, fmt.Sprintf("capture/%s/%d/%s", g.id, g.captures, name))
			if err == nil {
				err = g.regions[name].Seal(ctx)
			}
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				sources[name] = g.regions[name].Checkpoint()
			}
			result = errors.Join(result, err)
		}()
	}
	wg.Wait()
	if result != nil {
		return nil, nil, result
	}
	return state, sources, nil
}

// Stop is a migration's pause: the guest stops storing and its state is
// captured. Nothing is sealed and nothing is uploaded — the pages this process
// keeps are what the destination fetches.
func (g *guest) Stop(ctx context.Context) ([]byte, error) {
	g.mu.Lock()
	refused := g.refuseStop
	state := make([]byte, stateBytes)
	state[0] = g.value
	binary.LittleEndian.PutUint64(state[1:], uint64(g.writes))
	g.stopped = true
	g.mu.Unlock()
	if refused != nil {
		// A stop that fails after the pause began seals one region on its way
		// out, so the release has to unseal this process and leave both the
		// memory and the disk writable before a migration can be retried.
		if err := g.regions[g.names[0]].Seal(ctx); err != nil {
			return nil, err
		}
		return nil, refused
	}
	return state, nil
}

// setRefuseStop makes this guest's next migration pause fail after it has
// already stopped, or stops it doing so.
func (g *guest) setRefuseStop(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuseStop = err
}

// Resume restarts the vCPUs. This guest only stores when the driver asks it to,
// so what a resume means here is that stores are accepted again — which is
// exactly what an abandoned capture or migration owes the guest it stopped, and
// exactly what a run that never checked would not notice was missing.
func (g *guest) Resume(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = false
	return nil
}

// Release unseals every region and resumes a still-paused process, which is
// what an abandoned capture or migration owes the guest it stopped.
func (g *guest) Release(ctx context.Context) error {
	for _, name := range g.names {
		if err := g.regions[name].Unseal(ctx); err != nil {
			return err
		}
	}
	return g.Resume(ctx)
}

// Wait reports the end of the VMM process. This one has no life of its own:
// only its caller ends it.
func (g *guest) Wait(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }

func (g *guest) Close() error { return g.detach(context.Background()) }

// isClosed reports a VMM process that has been ended, by its host or by
// whatever gave the VM up.
func (g *guest) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// detach gives this process's pages back. It is what closing a VMM does, and
// what the loss of a host does to every VMM it was running.
func (g *guest) detach(ctx context.Context) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	var errs []error
	for _, name := range g.names {
		// The region goes first: the pager maps and protects through this
		// mapping, so emptying it before the detach leaves a capture in flight
		// driving a map that is being cleared underneath it.
		errs = append(errs, g.regions[name].Detach(ctx))
		mp := g.mappings[name]
		mp.mu.Lock()
		clear(mp.pages)
		mp.mu.Unlock()
	}
	return errors.Join(errs...)
}

// store writes one page the way a guest does: it stores through the mapping
// when the mapping is writable, and takes a write fault when a seal took that
// access away. The model is updated under the mapping lock the store took, so a
// checkpoint can never contain bytes the model does not.
func (g *guest) store(name string, page uint64) error { return g.storeIn(g.ctx, name, page) }

// storeIn is store under a caller's own context, which is what a scenario about
// a store the pager holds back needs: the guest's context outlives the fault,
// so a test with nothing else to cancel would wait for a checkpoint that is
// never coming.
func (g *guest) storeIn(ctx context.Context, name string, page uint64) error {
	g.mu.Lock()
	g.value++
	if g.value == 0 {
		g.value = 1
	}
	value := g.value
	g.mu.Unlock()
	return g.storeValueIn(ctx, name, page, value)
}

// storeValue stores one named byte, which is what a campaign reading a VM back
// as one whole generation writes into every page of it.
func (g *guest) storeValue(name string, page uint64, value byte) error {
	return g.storeValueIn(g.ctx, name, page, value)
}

func (g *guest) storeValueIn(ctx context.Context, name string, page uint64, value byte) error {
	g.mu.Lock()
	stopped := g.stopped
	g.mu.Unlock()
	if stopped {
		return fmt.Errorf("%s: the guest's vCPUs are stopped, so it stores nothing", g.instance)
	}
	mp := g.mappings[name]
	for range 8 {
		if g.storeModel(name, mp, page, value) {
			return nil
		}
		if err := g.regions[name].Fault(ctx, page, true); err != nil {
			return fmt.Errorf("%s store fault on %s page %d: %w", g.instance, name, page, err)
		}
	}
	return fmt.Errorf("%s store on %s page %d never resolved", g.instance, name, page)
}

// takeWritable takes one page writable and stores nothing into it, which is the
// fault a guest cannot avoid making: KVM finishes a cold read from a worker
// that always asks for the page writable, and an architecture can report a
// guest kernel's cache maintenance as a write. The model records nothing,
// because nothing was written, so whatever the page reads afterwards — the
// copy, or the page it was copied from once the settle gives it back — must
// still be the bytes the guest last wrote.
func (g *guest) takeWritable(ctx context.Context, name string, page uint64) error {
	g.mu.Lock()
	stopped := g.stopped
	g.mu.Unlock()
	if stopped {
		return fmt.Errorf("%s: the guest's vCPUs are stopped, so it faults on nothing", g.instance)
	}
	if err := g.regions[name].Fault(ctx, page, true); err != nil {
		return fmt.Errorf("%s write fault on %s page %d: %w", g.instance, name, page, err)
	}
	return nil
}

func (g *guest) storeModel(name string, mp *mapping, page uint64, value byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !mp.store(page, value) {
		return false
	}
	size := g.pageBytes[name]
	for i := range size {
		g.model[name][int(page)*size+i] = value
	}
	g.writes++
	return true
}

// stored is how many stores this guest has made, which is the counter its VMM
// state carries: a destination or a takeover that restored it continues exactly
// where the guest before it stopped, and one that lost it says so.
func (g *guest) stored() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writes
}

// storeAll writes one whole generation into every page of every volume, which
// is what a campaign that reads a recovered VM back as one generation needs in
// it.
func (g *guest) storeAll(value byte) error {
	for _, name := range g.names {
		for page := range uint64(g.pages[name]) {
			if err := g.storeValue(name, page, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// read faults one page in for reading and returns its bytes, which is what this
// guest sees.
func (g *guest) read(ctx context.Context, name string, page uint64) ([]byte, error) {
	mp := g.mappings[name]
	mp.mu.Lock()
	p, ok := mp.pages[page]
	mp.mu.Unlock()
	if !ok {
		if err := g.regions[name].Fault(ctx, page, false); err != nil {
			return nil, err
		}
		mp.mu.Lock()
		p, ok = mp.pages[page]
		mp.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("%s page %d unmapped after a fault", name, page)
		}
	}
	if p.slot < 0 {
		return make([]byte, g.pageBytes[name]), nil
	}
	mp.arena.mu.Lock()
	defer mp.arena.mu.Unlock()
	return bytes.Clone(mp.arena.slots[p.slot]), nil
}

// readAll reads every page of every region through this guest's own fault path
// and reports what it read, which pages could not be read at all, and the
// failures that made them unreadable. It is what a VM that came back somewhere
// else is compared against: the pager reconstructing a page and the volume
// holding it are the same claim from here.
func (g *guest) readAll(ctx context.Context) (map[string][]byte, map[string][]bool, error) {
	read := map[string][]byte{}
	missing := map[string][]bool{}
	var errs []error
	for _, name := range g.names {
		size := g.pageBytes[name]
		read[name] = make([]byte, g.pages[name]*size)
		missing[name] = make([]bool, g.pages[name])
		for page := range uint64(g.pages[name]) {
			got, err := g.read(ctx, name, page)
			if err != nil {
				missing[name][page] = true
				errs = append(errs, fmt.Errorf("%s %s page %d: %w: %w",
					g.instance, name, page, errUnreadable, err))
				continue
			}
			copy(read[name][int(page)*size:], got)
		}
	}
	return read, missing, errors.Join(errs...)
}

// snapshot is the bytes this guest believes it has, which is what a destination
// or a restart must read back.
func (g *guest) snapshot() map[string][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make(map[string][]byte, len(g.model))
	for name, data := range g.model {
		result[name] = bytes.Clone(data)
	}
	return result
}

// adopt takes bytes a migration, a fork or a restart handed this guest as its
// own model: memory it did not write itself.
func (g *guest) adopt(model map[string][]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, data := range model {
		g.model[name] = bytes.Clone(data)
	}
}

// errUnreadable is a page whose bytes could not be fetched at all: the only
// copy of them is on a peer a fault has taken away, so there is nothing to
// compare. It is not a byte read wrong, which is why it is told apart from one.
var errUnreadable = errors.New("the page could not be read")

// verify reads every page of every region and requires it to equal model. It is
// the campaign's first invariant: a guest never reads bytes it did not write.
//
// A page that reads the wrong bytes ends the verification, because there is
// nothing further to learn from a guest whose memory is already wrong. A page
// that cannot be read at all does not: every other page is still checked, and
// the caller decides whether a fault was on that excuses it.
func (g *guest) verify(ctx context.Context, model map[string][]byte) error {
	var unreadable error
	for _, name := range g.names {
		want, found := model[name]
		if !found {
			return fmt.Errorf("%s: the model has no volume %s", g.instance, name)
		}
		for page := range uint64(g.pages[name]) {
			got, err := g.read(ctx, name, page)
			if err != nil {
				unreadable = fmt.Errorf("%s %s page %d: %w: %w", g.instance, name, page, errUnreadable, err)
				continue
			}
			size := g.pageBytes[name]
			if !bytes.Equal(got, want[int(page)*size:(int(page)+1)*size]) {
				return fmt.Errorf("%s %s page %d reads %d, want %d",
					g.instance, name, page, got[0], want[int(page)*size])
			}
		}
	}
	return unreadable
}

// pager is one host's pagers and the arena and spill file each of them owns.
// The two share nothing: a RAM page and a PMEM page are different numbers of
// bytes, so a slot of one arena could not hold a page of the other.
type pager struct {
	pagers  vmmemory.Pagers
	arenas  map[vmmemory.RegionKind]*arena
	spills  map[vmmemory.RegionKind]platform.File
	runtime *sim.Runtime
}

// arenaOf is the shared page store the regions of one kind map through.
func (p *pager) arenaOf(kind vmmemory.RegionKind) *arena { return p.arenas[kind] }

func (p *pager) close(ctx context.Context) error {
	var errs []error
	for _, kind := range []vmmemory.RegionKind{vmmemory.Ram, vmmemory.Pmem} {
		errs = append(errs, p.pagers.For(kind).Close(ctx), p.spills[kind].Close())
	}
	return errors.Join(errs...)
}
