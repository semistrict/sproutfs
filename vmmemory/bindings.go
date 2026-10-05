package vmmemory

import (
	"sort"
	"time"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// binding is a page's state in one memory region. It is the content of the
// page's slot in the region's page list, as a page is the content of a slot of
// Zircon's VmPageList. It holds what Zircon's slot holds in its own way — the
// resident page, explicit zero and the spill reservation — and beside them
// what Zircon has no place for: whether the page is mapped, the checkpoint's
// copy it shares, the page it was copied from, whether it is cold and whether
// write-ahead made it.
type binding struct {
	memoryRegion *MemoryRegion
	index        uint64
	// Host.mu protects the pointer. The pointed-to resident's lock protects
	// mapping state. Eviction publishes spill before clearing this pointer.
	resident *resident
	// spill is the dirty reservation this page was admitted under, or none.
	// Whether it holds the page's bytes is the spill storage's own state: a
	// seal hands a reservation to the checkpoint's copy while a reclaim may
	// already be writing the bytes it will hold, and the fact has to follow the
	// reservation rather than the binding that named it.
	spill reservation
	// checkpoint names the detached copy holding this page's sealed bytes while a
	// checkpoint ingests. Such a binding is dirty but owns neither the spill
	// reservation nor the right to store: it shares the checkpoint's page until a
	// store copies away from it. The detached copy itself is not reachable from
	// the memory region's bindings and always has checkpoint == nil.
	checkpoint *binding
	// origin is the resident page this private copy was made from, when that
	// page held a published page identity: eight bytes, and not the identity
	// itself. A write fault is not always a store, so the settle compares the
	// sealed bytes with what that page still holds and re-shares the copy onto
	// it where the two are equal. Nothing is pinned by the pointer — an origin
	// that has been evicted is simply no longer an origin — and a page copied
	// from a checkpoint's held copy, from the name a fork point lent a private
	// page, from another host's unpublished page or from zeros has none.
	origin *resident
	// coldAt is when the copy became cold, in Unix nanoseconds, which is how
	// an eviction tells a copy the guest may still be about to store into from
	// one it has had time to. See coldCopyAge. It is an integer rather than a
	// time.Time because every page has one.
	coldAt int64
	mapped bool
	zero   bool // explicit zero backing, independent of arena residency
	dirty  bool
	// ahead marks a private page that write-ahead made resident before any
	// store into it. A store into a writable page never faults, so it stays
	// set until the page's dirty epoch ends, and only the bytes written back
	// can tell whether the guest used it.
	ahead bool
	// cold marks a copy a store trap made of origin, which is not yet known to
	// be the guest's state and pins origin until it is: see cold.go.
	cold bool
	// inZeroRun marks a bound page that a compressed zero run maps: a page
	// whose binding is clean and holds no memory, which a plan mapped to zero.
	// The run's mapping becomes the page's own state when the page is next
	// bound, as it does for a page of the run with no binding. A slot of the
	// page list holds a binding or lies in an interval, never both, so the
	// run keeps such a page here.
	inZeroRun bool
}

// writable reports whether the guest may store into this page where it is,
// without faulting: private state no checkpoint still depends on.
func (b *binding) writable() bool { return b.dirty && b.checkpoint == nil }

// offset is the page list's offset of a page. The page list keeps Zircon's byte
// offsets; the pager counts pages, and converts here.
func (r *MemoryRegion) offset(index uint64) uint64 { return index * r.host.pageSize }

// Binding addresses stay stable while a memory region is attached because
// resident alias sets retain them: a binding, once in the page list, stays
// there until the memory region detaches. A page nothing has touched has no
// slot. The memory region access lock protects lifetime; bindingsMu protects
// the page list.
func (r *MemoryRegion) binding(index uint64) *binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return r.bindingLocked(index)
}

// bindingRun is binding for the count pages from first, under one lock.
func (r *MemoryRegion) bindingRun(first, count uint64) []*binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	bindings := make([]*binding, count)
	for k := range bindings {
		bindings[k] = r.bindingLocked(first + uint64(k))
	}
	return bindings
}

// bindingLocked is the binding of one page, which it makes where the page has
// none. A touched page leaves the compressed zero run and owns its own binding
// state before any revoke or copy-on-write can begin: the run's interval is
// split around the page's slot, as Zircon's page list splits an interval
// around a slot that gets content. Caller holds bindingsMu.
func (r *MemoryRegion) bindingLocked(index uint64) *binding {
	// Most pages a fault binds are bound already, and finding a binding is
	// cheaper than allocating a slot, which has to look for an interval.
	offset := r.offset(index)
	if slot := r.pages.Lookup(offset); slot != nil && slot.IsPage() {
		b := slot.Page()
		if b.inZeroRun {
			b.inZeroRun = false
			b.zero, b.mapped = true, true
		}
		return b
	}
	slot, inRun := r.pages.LookupOrAllocate(offset, zirconvm.SplitInterval)
	b := &binding{memoryRegion: r, index: index}
	if inRun {
		b.zero, b.mapped = true, true
	}
	slot.Set(zirconvm.Page(b))
	return b
}

// lookupLocked is the binding of a page that has one, and whether a
// compressed zero run maps the page. Caller holds bindingsMu.
func (r *MemoryRegion) lookupLocked(index uint64) (b *binding, zeroRun bool) {
	offset := r.offset(index)
	if slot := r.pages.Lookup(offset); slot != nil && slot.IsPage() {
		b = slot.Page()
		return b, b.inZeroRun
	}
	return nil, r.pages.IsOffsetInZeroInterval(offset)
}

// eachBoundLocked calls visit on the binding of every page of [first, last)
// that has one, in order. visit must not change the page list. Caller holds
// bindingsMu.
func (r *MemoryRegion) eachBoundLocked(first, last uint64, visit func(*binding)) {
	if err := r.pages.ForEveryPageInRange(func(slot *zirconvm.PageOrMarker[binding], _ uint64) error {
		if slot.IsPage() {
			visit(slot.Page())
		}
		return nil
	}, r.offset(first), r.offset(last)); err != nil {
		panic("vmmemory: walking the page list: " + err.Error())
	}
}

// spillTarget reports the dirty reservation this binding's bytes go to, and
// whether a page that names none has them held elsewhere: by the checkpoint's
// copy of it, or because the page is not this memory region's own state at all. Both
// are read together under the map lock, because a seal moves the reservation to
// the checkpoint's copy while a reclaim of the resident page is reading it.
func (b *binding) spillTarget() (spill reservation, elsewhere bool) {
	b.memoryRegion.bindingsMu.Lock()
	defer b.memoryRegion.bindingsMu.Unlock()
	return b.spill, !b.dirty || b.checkpoint != nil
}

// setMapped and isMapped carry a page's mapping state across the one pair of
// holders that do not exclude each other: a reclaim revokes a victim's pages
// under that page's lock alone, and a seal reads them under the memory region.
func (r *MemoryRegion) setMapped(b *binding, mapped bool) {
	r.bindingsMu.Lock()
	b.mapped = mapped
	r.noteSealableLocked(b)
	r.bindingsMu.Unlock()
}

// noteSealableLocked records whether this page is one the next seal would
// write-protect: this memory region's own dirty state, held by no checkpoint, and
// mapped. The seal reads the runs of those pages rather than walking the dirty
// set, so its pause costs the commands it issues and not the pages they cover;
// every transition that changes any of the three keeps this up to date, which
// is what makes reading it O(runs). Caller holds bindingsMu.
func (r *MemoryRegion) noteSealableLocked(b *binding) {
	sealable := b.dirty && b.checkpoint == nil && b.mapped
	if r.dirtyRuns.has(b.index) == sealable {
		// Changing a run costs a join or a split, and most of these
		// transitions change nothing: a read of the runs is what tells them
		// apart.
		return
	}
	if sealable {
		r.dirtyRuns.add(b.index, b.index+1)
	} else {
		r.dirtyRuns.remove(b.index)
	}
}

// sealableRuns is the runs of consecutive pages one seal write-protects. It is
// the whole of what a seal does while the guest is paused. Caller holds the
// exclusive memory region lock.
func (r *MemoryRegion) sealableRuns() []PageRun {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return r.dirtyRuns.runs(uint64(r.pageCount))
}

// unmapPages takes back the record that a run of pages is mapped, which only a
// command the client refused may do: it changed nothing, so the pages are not
// mapped and a page recorded as mapped that is not would be resolved with
// nothing behind it. A page the client did map must never lose the record — a
// revocation skips an unmapped binding, and the memory the guest still reads
// through would be released under it.
func (r *MemoryRegion) unmapPages(page uint64, count int) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.eachBoundLocked(page, page+uint64(count), func(b *binding) {
		b.mapped = false
		r.noteSealableLocked(b)
	})
}

// unmapRuns is unmapPages for the runs one mapping command carried, including
// the compressed zero ranges a plan records instead of per-page bindings.
func (r *MemoryRegion) unmapRuns(runs []MapRun) {
	for _, run := range runs {
		if run.Zero {
			r.bindingsMu.Lock()
			r.unmapZerosLocked(run.Page, run.Page+uint64(run.Count))
			r.bindingsMu.Unlock()
			continue
		}
		r.unmapPages(run.Page, run.Count)
	}
}

// unmapZerosLocked takes every page of [first, last) out of the compressed zero
// runs. An interval that crosses either end is split there, and every interval
// then inside the range goes. Caller holds bindingsMu.
func (r *MemoryRegion) unmapZerosLocked(first, last uint64) {
	r.eachBoundLocked(first, last, func(b *binding) { b.inZeroRun = false })
	start, end := r.offset(first), r.offset(last)
	if r.pages.IsOffsetInZeroInterval(start) {
		r.pages.LookupOrAllocate(start, zirconvm.SplitInterval)
	}
	if lastPage := end - r.host.pageSize; lastPage > start && r.pages.IsOffsetInZeroInterval(lastPage) {
		r.pages.LookupOrAllocate(lastPage, zirconvm.SplitInterval)
	}
	if err := r.pages.RemovePages(func(slot *zirconvm.PageOrMarker[binding], _ uint64) error {
		if slot.IsInterval() {
			slot.Take()
		}
		return nil
	}, start, end); err != nil {
		panic("vmmemory: walking the page list: " + err.Error())
	}
}

func (r *MemoryRegion) isMapped(b *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.mapped
}

func (r *MemoryRegion) lookupBinding(index uint64) *binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b
}

// lookupBindings is lookupBinding of every page of [first, last), under one
// hold of the binding lock and one walk of the page list: nil for a page with
// no binding.
func (r *MemoryRegion) lookupBindings(first, last uint64) []*binding {
	bindings := make([]*binding, last-first)
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.eachBoundLocked(first, last, func(b *binding) { bindings[b.index-first] = b })
	return bindings
}

// boundIn is the bindings of the pages of [first, last) that have one, in
// order, under one hold of the binding lock and one walk of the page list.
func (r *MemoryRegion) boundIn(first, last uint64) []*binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	var bound []*binding
	r.eachBoundLocked(first, last, func(b *binding) { bound = append(bound, b) })
	return bound
}

// bindings is every binding of the memory region, in logical order. No
// binding-map lock is held while taking resident locks, making kernel changes
// or accessing backing.
func (r *MemoryRegion) bindings() []*binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	var result []*binding
	r.eachBoundLocked(0, uint64(r.pageCount), func(b *binding) { result = append(result, b) })
	return result
}

// Compressed zeros own no arena alias and need no per-page binding. Faults
// extract individual bindings only when they need private or explicit state.
func (r *MemoryRegion) zeroMapped(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	_, zero := r.lookupLocked(index)
	return zero
}

// repeated reports whether a fault on index for this access would be a repeated
// fault: this memory region already maps the page for it. See repeats.go.
func (r *MemoryRegion) repeated(index uint64, write bool) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, zero := r.lookupLocked(index)
	if zero {
		return !write
	}
	return b != nil && b.mapped && (!write || b.writable())
}
func (r *MemoryRegion) mapped(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, zero := r.lookupLocked(index)
	return zero || b != nil && b.mapped
}

// mapZeros records that a plan mapped every page of [start, end) to zero. A
// page with a binding keeps the run in it, and every other page joins an
// Untracked zero interval, which the page list merges with the intervals
// beside it.
func (r *MemoryRegion) mapZeros(start, end uint64) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	var gaps [][2]uint64
	if err := r.pages.ForEveryPageAndGapInRange(func(slot *zirconvm.PageOrMarker[binding], _ uint64) error {
		if slot.IsPage() {
			slot.Page().inZeroRun = true
		}
		return nil
	}, func(start, end uint64) error {
		gaps = append(gaps, [2]uint64{start, end})
		return nil
	}, r.offset(start), r.offset(end)); err != nil {
		panic("vmmemory: walking the page list: " + err.Error())
	}
	for _, gap := range gaps {
		if err := r.pages.AddZeroInterval(gap[0], gap[1], zirconvm.IntervalUntracked); err != nil {
			panic("vmmemory: adding a zero run: " + err.Error())
		}
	}
}

// fresh reports what a store may assume about a page whose lock it does not
// hold: zero when the page is zero-mapped, untouched when it holds nothing of
// its own at all, no mapping included, so that its bytes are whatever the
// volume holds. Either way it owns no memory, no private state and no
// checkpoint, and nothing needs fencing before a private page takes its place.
// Caller holds the page's fault stripe.
func (r *MemoryRegion) fresh(index uint64) (zero, untouched bool) {
	r.bindingsMu.Lock()
	b, zeroRun := r.lookupLocked(index)
	r.bindingsMu.Unlock()
	if zeroRun {
		return true, false
	}
	if b == nil {
		return false, true
	}
	// The resident page is checked first: an eviction changes a resident page's
	// mapping state under that page's lock alone, and publishes that the page
	// is gone under the host lock.
	r.host.mu.Lock()
	resident := b.resident != nil
	r.host.mu.Unlock()
	if resident {
		return false, false
	}
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if b.dirty || b.checkpoint != nil {
		return false, false
	}
	return b.zero, !b.zero && !b.mapped
}

// needsPrivatePage reports whether a store to index would have to allocate a
// private page, and with it a dirty reservation. It takes no memory region lock: a
// store decides this before it competes for one, and rechecks it afterwards.
func (r *MemoryRegion) needsPrivatePage(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b == nil || !b.dirty || b.checkpoint != nil
}

// holdInCheckpoint hands b's private page and spill reservation to the
// detached copy the checkpoint keeps. The page stays dirty: the volume does not
// hold its bytes yet, and a store must copy away from the checkpoint before it
// can change them.
func (r *MemoryRegion) holdInCheckpoint(b, held *binding) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.uncoldLocked(b)
	b.checkpoint, b.spill = held, noReservation
	held.ahead, b.ahead = b.ahead, false
	// The bytes the seal froze are the ones that were copied, so the page they
	// came from is the checkpoint's to compare them with.
	held.origin, b.origin = b.origin, nil
	delete(r.dirtyBindings, b.index)
	r.noteSealableLocked(b)
}

// originOf reports the page a checkpoint's copy was made from, nil where it was
// made from nothing a settle may compare it with.
func (r *MemoryRegion) originOf(b *binding) *resident {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.origin
}

// privateEpoch reports whether this page is the memory region's own dirty state and
// which checkpoint's copy it shares, read together so that a fault which gave
// the memory region up can tell whether a seal or a retire ran while it was away. Both
// change under the exclusive memory region lock, so a fault holding it shared reads
// the pair it decided on.
func (r *MemoryRegion) privateEpoch(b *binding) (dirty bool, held *binding) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.dirty, b.checkpoint
}

// checkpointCopy reports the checkpoint's copy of this page while the two
// share a resident page, nil when the page holds its own state.
func (r *MemoryRegion) checkpointCopy(b *binding) *binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.checkpoint
}

// takeFromCheckpoint ends a page's dependency on the checkpoint's copy of it
// and gives it the reservation its store was admitted under, in one step,
// because a page that is dirty with neither of them is a page a reclaim would
// punch. It is also what a store into a clean page does, which depends on no
// checkpoint and takes the same reservation.
func (r *MemoryRegion) takeFromCheckpoint(b *binding, spill reservation, origin *resident) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.uncoldLocked(b)
	b.checkpoint, b.spill, b.dirty, b.zero = nil, spill, true, false
	b.origin = origin
	if r.dirtyBindings == nil {
		r.dirtyBindings = make(map[uint64]*binding)
	}
	r.dirtyBindings[b.index] = b
	r.noteSealableLocked(b)
	r.noteDirtyLocked()
}

// restoreFromCheckpoint hands an abandoned checkpoint's copy back to the page
// it was taken from: the reservation returns and the page is dirty again,
// exactly as it was before the seal. retireFromCheckpoint instead ends the
// page's dirty epoch, because the volume now holds its bytes. Both leave the
// resident page with an alias that owns what it needs at every moment, so they
// run under that page's lock.
func (r *MemoryRegion) restoreFromCheckpoint(b, held *binding) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.uncoldLocked(b)
	b.checkpoint, b.spill, b.dirty, b.ahead = nil, held.spill, true, held.ahead
	b.origin, held.origin = held.origin, nil
	held.spill, held.dirty, held.ahead = noReservation, false, false
	if r.dirtyBindings == nil {
		r.dirtyBindings = make(map[uint64]*binding)
	}
	r.dirtyBindings[b.index] = b
	r.noteSealableLocked(b)
}
func (r *MemoryRegion) retireFromCheckpoint(b *binding) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.uncoldLocked(b)
	// The page is the volume's again, so where it was copied from says nothing
	// about it any more.
	b.checkpoint, b.dirty, b.origin = nil, false, nil
	delete(r.dirtyBindings, b.index)
	r.noteSealableLocked(b)
}

// forgetOrigin stops comparing this page with the page it was copied from:
// the guest changed it, or that page has gone. It is left alone where the page
// has been copied again since, from something else.
func (r *MemoryRegion) forgetOrigin(b *binding, origin *resident) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if b.origin == origin {
		r.uncoldLocked(b)
		b.origin = nil
	}
}

// endDirty ends a page's dirty epoch with no checkpoint, because its bytes are
// the ones the page it now shares holds: see GiveBack. It reports the dirty
// reservation the page was admitted under, for the caller to give back. A
// memory region left with no dirty page holds no unpublished write, so its
// loss window ends too.
func (r *MemoryRegion) endDirty(b *binding) reservation {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.uncoldLocked(b)
	spill := b.spill
	b.spill, b.dirty, b.ahead, b.origin = noReservation, false, false, nil
	delete(r.dirtyBindings, b.index)
	r.noteSealableLocked(b)
	if len(r.dirtyBindings) == 0 {
		r.dirtySince = time.Time{}
	}
	return spill
}

// heldBy reports whether the live page still shares the checkpoint's copy,
// which is what decides between publishing that copy and discarding it.
func (r *MemoryRegion) heldBy(index uint64, held *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b != nil && b.checkpoint == held
}

// Dirty ownership changes under the memory region access lock. The map mutex lets
// independent faults add entries without walking the page list.
func (r *MemoryRegion) setDirty(b *binding, dirty bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if !dirty {
		r.uncoldLocked(b)
	}
	b.dirty = dirty
	if !dirty {
		// The dirty epoch that write-ahead began has ended, and with it
		// whatever this page's bytes were once copied from.
		b.ahead, b.origin = false, nil
	}
	if dirty {
		if r.dirtyBindings == nil {
			r.dirtyBindings = make(map[uint64]*binding)
		}
		r.dirtyBindings[b.index] = b
		r.noteDirtyLocked()
	} else {
		delete(r.dirtyBindings, b.index)
	}
	r.noteSealableLocked(b)
}

// setDirtyMappedRun is setDirty(b, true) then setMapped(b, true) for the
// consecutive pages of a run, under one lock. A run of fresh pages no
// checkpoint holds is one sealable run, which is one change to the runs a seal
// reads rather than one per page.
func (r *MemoryRegion) setDirtyMappedRun(bindings []*binding) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if r.dirtyBindings == nil {
		r.dirtyBindings = make(map[uint64]*binding)
	}
	held := false
	for _, b := range bindings {
		b.dirty, b.mapped = true, true
		r.dirtyBindings[b.index] = b
		held = held || b.checkpoint != nil
	}
	r.noteDirtyLocked()
	if !held {
		first := bindings[0].index
		r.dirtyRuns.add(first, first+uint64(len(bindings)))
		return
	}
	for _, b := range bindings {
		r.noteSealableLocked(b)
	}
}

// dirtyCount reports how many pages hold private state a checkpoint has not
// taken, which is what the next seal takes.
func (r *MemoryRegion) dirtyCount() int {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return len(r.dirtyBindings)
}

// takeDirtySet hands the whole dirty set to a seal in one step and leaves the
// memory region with none. It is O(1): a pause may not walk what it is freezing, and
// what the seal has to do per page it does afterwards, with the guest running.
// Caller holds the exclusive memory region lock.
func (r *MemoryRegion) takeDirtySet() map[uint64]*binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	pending := r.dirtyBindings
	r.dirtyBindings = nil
	r.dirtyRuns = newPageRuns(r.host.pageSize)
	return pending
}

func sortedBindings(set map[uint64]*binding) []*binding {
	result := make([]*binding, 0, len(set))
	for _, b := range set {
		result = append(result, b)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].index < result[j].index })
	return result
}
