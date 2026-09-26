package vmmemory

import (
	"sort"

	"github.com/semistrict/sproutfs/vmmemory/internal/pageranges"
)

type binding struct {
	memoryRegion *MemoryRegion
	index        uint64
	// Host.mu protects the pointer. The pointed-to resident's lock protects
	// mapping state. Eviction publishes spill before clearing this pointer.
	resident *resident
	mapped   bool
	zero     bool // explicit zero backing, independent of arena residency
	dirty    bool
	// spillSlot names the dirty reservation this page was admitted under, or
	// -1. Whether the slot holds the page's bytes is the slot's own state, in
	// Host.reservations: a seal hands a reservation to the checkpoint's copy
	// while a reclaim may already be writing the bytes it will hold, and the
	// fact has to follow the slot rather than the binding that named it.
	spillSlot int
	// checkpoint names the detached copy holding this page's sealed bytes while a
	// checkpoint ingests. Such a binding is dirty but owns neither the spill
	// reservation nor the right to store: it shares the checkpoint's page until a
	// store copies away from it. The detached copy itself is not reachable from
	// the memory region's bindings and always has checkpoint == nil.
	checkpoint *binding
	// ahead marks a private page that write-ahead made resident before any
	// store into it. A store into a writable page never faults, so it stays
	// set until the page's dirty epoch ends, and only the bytes written back
	// can tell whether the guest used it.
	ahead bool
	// origin is the resident page this private copy was made from, when that
	// page held a published page identity: eight bytes, and not the identity
	// itself. A write fault is not always a store, so the settle compares the
	// sealed bytes with what that page still holds and re-shares the copy onto
	// it where the two are equal. Nothing is pinned by the pointer — an origin
	// that has been evicted is simply no longer an origin — and a page copied
	// from a checkpoint's held copy, from the name a fork point lent a private
	// page, from another host's unpublished page or from zeros has none.
	origin *resident
}

// writable reports whether the guest may store into this page where it is,
// without faulting: private state no checkpoint still depends on.
func (b *binding) writable() bool { return b.dirty && b.checkpoint == nil }

const bindingBlockPages = 256

type bindingBlock [bindingBlockPages]binding

// Binding addresses stay stable while a memory region is attached because resident
// alias sets retain them. Untouched blocks have no binding allocation.
// The memory region access lock protects lifetime; this mutex only protects the map.
func (r *MemoryRegion) binding(index uint64) *binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b := r.bindingLocked(index)
	if r.zeroRanges.Get(index).Zero {
		// A touched page leaves the compressed zero run and owns its own
		// binding state before any revoke or copy-on-write can begin.
		r.zeroRanges.Set(index, index+1, pageranges.State{})
		b.zero, b.mapped = true, true
	}
	return b
}

// bindingRun is binding for the count pages from first, under one lock, and
// takes the whole run out of the compressed zero runs at once.
func (r *MemoryRegion) bindingRun(first, count uint64) []*binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	bindings := make([]*binding, count)
	touched := false
	for k := range bindings {
		index := first + uint64(k)
		b := r.bindingLocked(index)
		if r.zeroRanges.Get(index).Zero {
			b.zero, b.mapped = true, true
			touched = true
		}
		bindings[k] = b
	}
	if touched {
		r.zeroRanges.Set(first, first+count, pageranges.State{})
	}
	return bindings
}

// bindingLocked is the binding of one page, allocating its block. Caller holds
// bindingsMu.
func (r *MemoryRegion) bindingLocked(index uint64) *binding {
	key := index / bindingBlockPages
	block := r.blocks[key]
	if block == nil {
		block = new(bindingBlock)
		for i := range block {
			block[i] = binding{memoryRegion: r, index: key*bindingBlockPages + uint64(i), spillSlot: -1}
		}
		r.blocks[key] = block
	}
	return &block[index%bindingBlockPages]
}

// spillTarget reports the dirty reservation this binding's bytes go to, and
// whether a page that names none has them held elsewhere: by the checkpoint's
// copy of it, or because the page is not this memory region's own state at all. Both
// are read together under the map lock, because a seal moves the reservation to
// the checkpoint's copy while a reclaim of the resident page is reading it.
func (b *binding) spillTarget() (slot int, elsewhere bool) {
	b.memoryRegion.bindingsMu.Lock()
	defer b.memoryRegion.bindingsMu.Unlock()
	return b.spillSlot, !b.dirty || b.checkpoint != nil
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
	if r.dirtyRuns.Get(b.index).Dirty == sealable {
		// Replacing a run costs the depth of its boundaries, and most of these
		// transitions change nothing: a read of the run is what tells them apart.
		return
	}
	r.dirtyRuns.Set(b.index, b.index+1, pageranges.State{Dirty: sealable})
}

// sealableRuns is the runs of consecutive pages one seal write-protects. It is
// the whole of what a seal does while the guest is paused. Caller holds the
// exclusive memory region lock.
func (r *MemoryRegion) sealableRuns() []PageRun {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	var runs []PageRun
	for page := uint64(0); page < uint64(r.pageCount); {
		state, end := r.dirtyRuns.Run(page, uint64(r.pageCount))
		if state.Dirty {
			runs = append(runs, PageRun{Page: page, Count: int(end - page)})
		}
		page = end
	}
	return runs
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
	for k := range uint64(count) {
		if block := r.blocks[(page+k)/bindingBlockPages]; block != nil {
			b := &block[(page+k)%bindingBlockPages]
			b.mapped = false
			r.noteSealableLocked(b)
		}
	}
}

// unmapRuns is unmapPages for the runs one mapping command carried, including
// the compressed zero ranges a plan records instead of per-page bindings.
func (r *MemoryRegion) unmapRuns(runs []MapRun) {
	for _, run := range runs {
		if run.Zero {
			r.bindingsMu.Lock()
			r.zeroRanges.Set(run.Page, run.Page+uint64(run.Count), pageranges.State{})
			r.bindingsMu.Unlock()
			continue
		}
		r.unmapPages(run.Page, run.Count)
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
	if block := r.blocks[index/bindingBlockPages]; block != nil {
		return &block[index%bindingBlockPages]
	}
	return nil
}

// touchedBlock reports whether any page of the binding block holding index has
// ever been given per-page state. A range operation checks this once per 256
// pages instead of looking each page up.
func (r *MemoryRegion) touchedBlock(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return r.blocks[index/bindingBlockPages] != nil
}

// Snapshot only allocated blocks in logical order. No binding-map lock is held
// while taking resident locks, making kernel changes or accessing backing.
func (r *MemoryRegion) bindings() []*binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	keys := make([]uint64, 0, len(r.blocks))
	for key := range r.blocks {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	result := make([]*binding, 0, len(keys)*bindingBlockPages)
	for _, key := range keys {
		block := r.blocks[key]
		for i := range block {
			if block[i].index < uint64(r.pageCount) {
				result = append(result, &block[i])
			}
		}
	}
	return result
}

// Compressed zeros own no arena alias and need no per-page binding. Faults
// extract individual bindings only when they need private or explicit state.
func (r *MemoryRegion) zeroMapped(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return r.zeroRanges.Get(index).Zero
}

// repeated reports whether a fault on index for this access would be a repeated
// fault: this memory region already maps the page for it. See repeats.go.
func (r *MemoryRegion) repeated(index uint64, write bool) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if r.zeroRanges.Get(index).Zero {
		return !write
	}
	block := r.blocks[index/bindingBlockPages]
	if block == nil {
		return false
	}
	b := &block[index%bindingBlockPages]
	return b.mapped && (!write || b.writable())
}
func (r *MemoryRegion) mapped(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if r.zeroRanges.Get(index).Zero {
		return true
	}
	if block := r.blocks[index/bindingBlockPages]; block != nil {
		return block[index%bindingBlockPages].mapped
	}
	return false
}
func (r *MemoryRegion) mapZeros(start, end uint64) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.zeroRanges.Set(start, end, pageranges.State{Zero: true})
}

// fresh reports what a store may assume about a page whose lock it does not
// hold: zero when the page is zero-mapped, untouched when it holds nothing of
// its own at all, no mapping included, so that its bytes are whatever the
// volume holds. Either way it owns no memory, no private state and no
// checkpoint, and nothing needs fencing before a private page takes its place.
// Caller holds the page's fault stripe.
func (r *MemoryRegion) fresh(index uint64) (zero, untouched bool) {
	r.bindingsMu.Lock()
	if r.zeroRanges.Get(index).Zero {
		r.bindingsMu.Unlock()
		return true, false
	}
	var b *binding
	if block := r.blocks[index/bindingBlockPages]; block != nil {
		b = &block[index%bindingBlockPages]
	}
	r.bindingsMu.Unlock()
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
	block := r.blocks[index/bindingBlockPages]
	if block == nil {
		return true
	}
	b := &block[index%bindingBlockPages]
	return !b.dirty || b.checkpoint != nil
}

// holdInCheckpoint hands b's private page and spill reservation to the
// detached copy the checkpoint keeps. The page stays dirty: the volume does not
// hold its bytes yet, and a store must copy away from the checkpoint before it
// can change them.
func (r *MemoryRegion) holdInCheckpoint(b, held *binding) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b.checkpoint, b.spillSlot = held, -1
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
func (r *MemoryRegion) takeFromCheckpoint(b *binding, slot int, origin *resident) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b.checkpoint, b.spillSlot, b.dirty, b.zero = nil, slot, true, false
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
	b.checkpoint, b.spillSlot, b.dirty, b.ahead = nil, held.spillSlot, true, held.ahead
	b.origin, held.origin = held.origin, nil
	held.spillSlot, held.dirty, held.ahead = -1, false, false
	if r.dirtyBindings == nil {
		r.dirtyBindings = make(map[uint64]*binding)
	}
	r.dirtyBindings[b.index] = b
	r.noteSealableLocked(b)
}
func (r *MemoryRegion) retireFromCheckpoint(b *binding) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	// The page is the volume's again, so where it was copied from says nothing
	// about it any more.
	b.checkpoint, b.dirty, b.origin = nil, false, nil
	delete(r.dirtyBindings, b.index)
	r.noteSealableLocked(b)
}

// heldBy reports whether the live page still shares the checkpoint's copy,
// which is what decides between publishing that copy and discarding it.
func (r *MemoryRegion) heldBy(index uint64, held *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	block := r.blocks[index/bindingBlockPages]
	return block != nil && block[index%bindingBlockPages].checkpoint == held
}

// Dirty ownership changes under the memory region access lock. The map mutex lets
// independent faults add entries without scanning all touched blocks.
func (r *MemoryRegion) setDirty(b *binding, dirty bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
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
		r.dirtyRuns.Set(first, first+uint64(len(bindings)), pageranges.State{Dirty: true})
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
	r.dirtyRuns = pageranges.Map{}
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
