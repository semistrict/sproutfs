package vmmemory

import (
	"context"
	"errors"
)

// What a store makes private besides the page the guest wrote.
//
// Placement alone keeps the private pages of a range adjacent; it cannot make
// them fewer runs than the guest's own writes are. Two rules do that.
//
//   - A store closes a small gap, once its memory region is near its mapping budget.
//     A store landing within gapPages of a page the same range already holds
//     makes the pages between them private in the same fault, in one mapping
//     command, so the two runs are one. A gap is never closed across a range's
//     boundary: the extent belongs to the range. It waits for the budget
//     because what it saves is mappings and what it costs is memory: a
//     process with mappings to spare pays for a scattered store with one, and
//     scattered small stores into a large heap — a seeded database updated at
//     random — are exactly where closing every gap copied ten times what the
//     guest wrote. A node's limit is a million mappings, of which a process is
//     given half, so a memory region is near it only once its process has refused it
//     a mapping; from then on it closes gaps, and the range that refusal was
//     for is made whole.
//
//   - A range that is half private becomes private. When half a range's pages
//     are at their offsets in its extent, the rest are copied into the holes of
//     it: the range is then one mapping, one write-protect command at a seal,
//     and a candidate for a huge mapping. Nothing already there is copied again,
//     so it costs at most twice what the guest wrote into that range.
//
// Both are bounded by what is free. A page with no free dirty reservation, no
// offset of its own or a checkpoint still holding it is left alone and the run
// ends there: neither rule ever waits, and neither ever evicts.
const (
	// gapPages is how near a store must land to a page its range already holds
	// for the pages between to be made private with it. It was measured rather
	// than chosen: of the 1,979 gaps between the private runs a 4 KiB fan-out
	// recorded on 2026-09-21, 1,532 — 77 % — are 16 pages or fewer, so sixteen
	// is the bucket boundary at which three quarters of the alternations a fork
	// holds stop being alternations. It bounds the worst case at seventeen times
	// what a guest wrote, against 512 times at a 2 MiB page.
	gapPages = 16
	// wholeRangeShare is the fraction of a range's offsets that must hold a page
	// before the rest are copied into the holes of its extent.
	wholeRangeShare = 2
)

// closeAround is what a store does after the page it faulted on is private: it
// makes the shared pages the two rules name private too and reports the run of
// pages the store maps, which is one mapping command because the whole of it
// sits at consecutive offsets of one extent.
//
// A store the placement rule had no offset for is its own page and nothing
// else: without the extent there is no run to be part of.
func (r *MemoryRegion) closeAround(ctx context.Context, index uint64, at fileSlot, replaced *replacement) (first, last uint64, err error) {
	h := r.host
	h.mu.Lock()
	placed := h.placedAt(r, index, at)
	first, last = index, index+1
	whole := false
	if placed {
		first, last = h.nearby(r, index)
		whole = h.halfPrivate(r, index)
	}
	h.mu.Unlock()
	if !placed {
		return index, index + 1, nil
	}
	if whole {
		// The range is half its own already, so the rest of it is copied into
		// the holes of its extent and the range becomes one mapping.
		span := uint64(h.extentPages)
		first, last = index-index%span, index-index%span+span
		last = min(last, uint64(r.pageCount))
	}
	if last-first == 1 {
		return first, last, nil
	}
	if err := r.takeShared(ctx, first, index, last, replaced); err != nil {
		return 0, 0, err
	}
	if whole {
		h.markWhole(r, index)
	}
	// What the rules could not take — a page with no free reservation, or one a
	// checkpoint holds — is not part of this store's run. The run is the pages
	// either side of the faulting one that really are at their own offsets now.
	h.mu.Lock()
	first, last = h.placedRun(r, index, first, last)
	h.mu.Unlock()
	return first, last, nil
}

// placedAt reports whether one page's resident slot is the one the placement
// rule gives it, which is what makes it part of a run of its range rather than
// a page of its own somewhere in the arena. Caller holds h.mu.
func (h *Host) placedAt(r *MemoryRegion, index uint64, at fileSlot) bool {
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	return e != nil && at == h.slotIn(e, index)
}

// nearby reports the pages one store makes private by the gap rule: the
// faulting page, and the pages between it and the nearest page of its range the
// guest already stores into at that page's own offset, where that page is within
// gapPages. Caller holds h.mu.
func (h *Host) nearby(r *MemoryRegion, index uint64) (first, last uint64) {
	if !r.pressed.Load() {
		return index, index + 1
	}
	span := uint64(h.extentPages)
	e := h.extents[extentKey{r, index / span}]
	low := index - index%span
	high := min(low+span, uint64(r.pageCount))
	first, last = index, index+1
	for d := uint64(1); d <= gapPages && index-d >= low && index >= d; d++ {
		if h.placedPrivateAt(r, e, index-d) {
			first = index - d + 1
			break
		}
	}
	for d := uint64(1); d <= gapPages && index+d < high; d++ {
		if h.placedPrivateAt(r, e, index+d) {
			last = index + d
			break
		}
	}
	return first, last
}

// halfPrivate reports a range whose extent holds pages at half its offsets,
// which is when the rest are copied into its holes. Caller holds h.mu.
func (h *Host) halfPrivate(r *MemoryRegion, index uint64) bool {
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	return e != nil && e.held*wholeRangeShare >= h.extentPages
}

// markWhole and wholeRange carry the one thing a settle has to know about the
// rules: a range that was filled stays filled. A settle that handed one of its
// pages back would break the range into three mappings again, for a page the
// guest is about to write anyway.
func (h *Host) markWhole(r *MemoryRegion, index uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.extents[extentKey{r, index / uint64(h.extentPages)}]; e != nil {
		e.whole = true
	}
}

func (h *Host) wholeRange(r *MemoryRegion, index uint64) bool {
	if h.extentPages <= 1 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	return e != nil && e.whole
}

// placedPrivateAt reports the one thing every rule asks of a page: that this
// memory region may store into it where the placement rule put it. That is its own
// dirty state, held by no checkpoint, resident at the offset its range's extent
// gives it — and it is exactly what one store's mapping command can cover, so a
// run ends at the first page that is not it.
//
// The offset alone answers nothing, and the difference is a store the guest
// loses. A checkpoint freezes the guest's copy where it is and retiring it
// leaves that page published at the same offset, so the store that follows
// finds its own offset occupied and takes an ordinary one — after which the
// offset goes on holding a page this memory region no longer stores into. Reading the
// offset as this page's would put the run's mapping over that older page: the
// store the guest made would be lost, and every memory region that inherited the
// published identity would have its page written under it. Caller holds h.mu.
func (h *Host) placedPrivateAt(r *MemoryRegion, e *extent, page uint64) bool {
	if e == nil {
		return false
	}
	b := r.lookupBinding(page)
	return b != nil && b.writable() && b.resident != nil &&
		b.resident.fileSlot == h.slotIn(e, page)
}

// placedRun reports the longest run of pages around index that one mapping
// command covers, within [first, last): every page of it is this memory region's own
// at a consecutive offset of one extent. Caller holds h.mu.
func (h *Host) placedRun(r *MemoryRegion, index, first, last uint64) (uint64, uint64) {
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	start, end := index, index+1
	for start > first && h.placedPrivateAt(r, e, start-1) {
		start--
	}
	for end < last && h.placedPrivateAt(r, e, end) {
		end++
	}
	return start, end
}

// isPrivateAt reports a page this memory region may store into where it is: its own
// dirty state, held by no checkpoint. It is what the rules leave behind for
// every page they took.
func (r *MemoryRegion) isPrivateAt(page uint64) bool {
	b := r.lookupBinding(page)
	return b != nil && b.writable()
}

// takeShared makes the pages either side of the faulting one this memory region's own
// dirty state, at the offset the placement rule gives each, copying the bytes
// it holds now and remembering the page they came from so a settle can hand
// back what the guest never wrote.
//
// It works outward from the faulting page and stops on each side at the first
// page that cannot join the store's run: one the rule could not take — no free
// dirty reservation, no offset of its own, a checkpoint still holding it, or
// bytes this host does not already have — and one that is already the guest's
// somewhere else in the arena. So what it takes is exactly what the caller's
// one mapping command covers. A page made private and left out of that command
// would keep the guest's mapping of the page it was copied from: the copy it
// was given would hold nothing the guest ever reached, and the guest's next
// store would be resolved against a page its volume publishes.
//
// It never waits, never evicts and reads nothing. Caller holds the memory region
// shared, as a fault holds it, and the pages it takes are left for the caller's
// one mapping command.
func (r *MemoryRegion) takeShared(ctx context.Context, first, index, last uint64, replaced *replacement) error {
	for page := index + 1; page < last; page++ {
		joined, err := r.takeOneShared(ctx, page, replaced)
		if err != nil {
			return err
		}
		if !joined {
			break
		}
	}
	for page := index; page > first; page-- {
		joined, err := r.takeOneShared(ctx, page-1, replaced)
		if err != nil {
			return err
		}
		if !joined {
			break
		}
	}
	return nil
}

// joinsRun reports a page one store's mapping command may cover, taking the
// host lock to ask it.
func (r *MemoryRegion) joinsRun(page uint64) bool {
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.placedPrivateAt(r, h.extents[extentKey{r, page / uint64(h.extentPages)}], page)
}

// takeOneShared makes one page private for a rule, reporting whether the page
// is one the store's run now covers.
func (r *MemoryRegion) takeOneShared(ctx context.Context, page uint64, replaced *replacement) (bool, error) {
	h := r.host
	b := r.binding(page)
	if b.writable() || r.checkpointCopy(b) != nil {
		return r.joinsRun(page), nil
	}
	spill, err := h.tryTakeSpill()
	if err != nil {
		if errors.Is(err, ErrCapacity) {
			return false, nil
		}
		return false, err
	}
	release := func() { h.releaseSpill(spill) }
	pg, err := h.current(ctx, b)
	if err != nil {
		release()
		return false, err
	}
	if pg == nil && !b.zero && !b.dirty {
		// This host does not hold the page's bytes, so copying it would mean a
		// backing read and a page to read it into — which is an eviction, and a
		// rule evicts for nothing the guest did not write. The run ends here.
		release()
		return false, nil
	}
	var origin *resident
	if pg.published() {
		origin = pg
	}
	data := make([]byte, h.pageSize)
	if _, err := r.readForCopy(ctx, b, pg, data); err != nil {
		if pg != nil {
			h.unlock(pg)
		}
		release()
		return false, err
	}
	if pg != nil {
		h.unlock(pg)
	}
	h.mu.Lock()
	at, _ := h.place(r, page)
	h.mu.Unlock()
	if at.slot < 0 {
		// The offset of this page's own is not to be had — the budget is full,
		// or a checkpoint's copy is sitting on it. A rule never evicts for a
		// page the guest did not write, so the run ends here.
		release()
		return false, nil
	}
	if r.checkpointCopy(b) != nil || b.writable() {
		// A seal ran while the bytes were being read. The page is the
		// checkpoint's or the guest's now, and either way not this rule's.
		release()
		return false, h.abandonSlots(ctx, at, 1, nil)
	}
	private, err := h.create(ctx, at, data, pageKey{}, true, r.kind)
	if err != nil {
		release()
		return false, err
	}
	old, err := h.current(ctx, b)
	if err != nil {
		h.unlock(private)
		release()
		return false, err
	}
	if err := r.takePrivate(ctx, b, old, private, spill, origin, replaced); err != nil {
		h.unlock(private)
		return false, err
	}
	h.touch(private)
	h.unlock(private)
	h.mu.Lock()
	h.stats.RuleCopies++
	h.mu.Unlock()
	return true, nil
}

// makeWhole copies every page of one range into the holes of its extent, which
// is what the mapping budget's backstop does when a client refuses a store's
// mapping command: the range the guest is writing in becomes one run, so the
// mappings it was costing that process go with it and the store is served
// again. It reports whether it took anything.
func (r *MemoryRegion) makeWhole(ctx context.Context, index uint64) (bool, error) {
	h := r.host
	span := uint64(h.extentPages)
	h.mu.Lock()
	placed := h.extents[extentKey{r, index / span}] != nil
	h.mu.Unlock()
	if !placed {
		return false, nil
	}
	first := index - index%span
	last := min(first+span, uint64(r.pageCount))
	before := r.privatePages(first, last)
	replaced := &replacement{memoryRegion: r}
	if err := r.takeShared(ctx, first, index, last, replaced); err != nil {
		return false, errors.Join(err, replaced.revoke(ctx))
	}
	if r.privatePages(first, last) <= before {
		return false, replaced.done(ctx)
	}
	h.markWhole(r, index)
	h.mu.Lock()
	first, last = h.placedRun(r, index, first, last)
	at := h.placedSlot(r, first)
	h.stats.MappingMerges++
	h.mu.Unlock()
	// The whole run in one command, which is the point: the mappings the range's
	// alternations were costing the VMM go, and what is left is one.
	count := int(last - first)
	for page := first; page < last; page++ {
		r.setMapped(r.binding(page), true)
	}
	if err := r.mapPages(ctx, runAt(first, at, count), true); err != nil {
		if revoked := replaced.revoke(ctx); revoked != nil {
			return false, errors.Join(r.fail(err), revoked)
		}
		return false, r.mappingFailed(err, func() { r.unmapPages(first, count) })
	}
	return true, replaced.done(ctx)
}

// placedSlot is the slot of r's private file the placement rule gives one page,
// or slot -1 of it where its range owns no extent. Caller holds h.mu.
func (h *Host) placedSlot(r *MemoryRegion, page uint64) fileSlot {
	e := h.extents[extentKey{r, page / uint64(h.extentPages)}]
	if e == nil {
		return fileSlot{r.privateFile(), -1}
	}
	return h.slotIn(e, page)
}

// privatePages is how many pages of one range this memory region may store into where
// they are, which is what a range made whole has all of.
func (r *MemoryRegion) privatePages(first, last uint64) int {
	count := 0
	for page := first; page < last; page++ {
		if r.isPrivateAt(page) {
			count++
		}
	}
	return count
}
