package vmmemory

import (
	"context"
	"errors"
)

// What a store makes private besides the page the guest wrote.
//
// Placement alone keeps the private pages of a range adjacent; it cannot make
// them fewer runs than the guest's own writes are. Two rules do that, and both
// hold all the time rather than waiting for a budget to be exceeded — a budget
// that merges when it is passed does nothing until the limit and then puts a
// copy of up to a whole range on the fault path at the moment the guest is
// busiest.
//
//   - A store closes a small gap. A store landing within gapPages of a page the
//     same range already holds makes the pages between them private in the same
//     fault, in one mapping command, so the two runs are one. Writes cluster, so
//     the unit a store copies grows where the guest is writing and stays one
//     page where a write is alone. A gap is never closed across a range's
//     boundary: the extent belongs to the range.
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
func (r *Region) closeAround(ctx context.Context, index uint64, slot int) (first, last uint64, err error) {
	h := r.host
	h.mu.Lock()
	placed := h.placedAt(r, index, slot)
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
	if err := r.takeShared(ctx, first, index, last); err != nil {
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

// placedAt reports whether one page's resident offset is the one the placement
// rule gives it, which is what makes it part of a run of its range rather than
// a page of its own somewhere in the arena. Caller holds h.mu.
func (h *Host) placedAt(r *Region, index uint64, slot int) bool {
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	return e != nil && slot == e.base+int(index%uint64(h.extentPages))
}

// nearby reports the pages one store makes private by the gap rule: the
// faulting page, and the pages between it and the nearest page its range
// already holds at an offset of the range's extent, where that page is within
// gapPages. Caller holds h.mu.
func (h *Host) nearby(r *Region, index uint64) (first, last uint64) {
	span := uint64(h.extentPages)
	e := h.extents[extentKey{r, index / span}]
	low := index - index%span
	high := min(low+span, uint64(r.pageCount))
	first, last = index, index+1
	for d := uint64(1); d <= gapPages && index-d >= low && index >= d; d++ {
		if h.heldAt(e, index-d) {
			first = index - d + 1
			break
		}
	}
	for d := uint64(1); d <= gapPages && index+d < high; d++ {
		if h.heldAt(e, index+d) {
			last = index + d
			break
		}
	}
	return first, last
}

// halfPrivate reports a range whose extent holds pages at half its offsets,
// which is when the rest are copied into its holes. Caller holds h.mu.
func (h *Host) halfPrivate(r *Region, index uint64) bool {
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	return e != nil && e.held*wholeRangeShare >= h.extentPages
}

// markWhole and wholeRange carry the one thing a settle has to know about the
// rules: a range that was filled stays filled. A settle that handed one of its
// pages back would break the range into three mappings again, for a page the
// guest is about to write anyway.
func (h *Host) markWhole(r *Region, index uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.extents[extentKey{r, index / uint64(h.extentPages)}]; e != nil {
		e.whole = true
	}
}

func (h *Host) wholeRange(r *Region, index uint64) bool {
	if h.extentPages <= 1 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	return e != nil && e.whole
}

// heldAt reports whether one page of a range sits at its own offset in the
// range's extent. Caller holds h.mu.
func (h *Host) heldAt(e *extent, page uint64) bool {
	if e == nil {
		return false
	}
	_, held := h.residentLeases[e.base+int(page%uint64(h.extentPages))]
	return held
}

// placedRun reports the longest run of pages around index whose offsets are all
// the ones the placement rule gives them, within [first, last). It is what the
// store maps: every page of it is at a consecutive offset of one extent, so one
// command installs the lot. Caller holds h.mu.
func (h *Host) placedRun(r *Region, index, first, last uint64) (uint64, uint64) {
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	start, end := index, index+1
	for start > first && h.heldAt(e, start-1) && r.isPrivateAt(start-1) {
		start--
	}
	for end < last && h.heldAt(e, end) && r.isPrivateAt(end) {
		end++
	}
	return start, end
}

// isPrivateAt reports a page this region may store into where it is: its own
// dirty state, held by no checkpoint. It is what the rules leave behind for
// every page they took.
func (r *Region) isPrivateAt(page uint64) bool {
	b := r.lookupBinding(page)
	return b != nil && b.writable()
}

// takeShared makes every page of [first, last) but the faulting one this
// region's own dirty state, at the offset the placement rule gives it, copying
// the bytes it holds now and remembering the page they came from so a settle
// can hand back what the guest never wrote.
//
// It never waits, never evicts and reads nothing: a page with no free dirty
// reservation, no offset of its own, one a checkpoint is still holding, or one
// whose bytes this host does not already have — a page no region has read in —
// is left exactly as it was and the run ends there. Caller holds the region
// shared, as a fault holds it, and the pages it takes are left unmapped for the
// caller's one mapping command.
func (r *Region) takeShared(ctx context.Context, first, index, last uint64) error {
	for page := first; page < last; page++ {
		if page == index {
			continue
		}
		taken, err := r.takeOneShared(ctx, page)
		if err != nil {
			return err
		}
		if !taken && page < index {
			// The run below the store is broken, so nothing further down joins
			// it; the pages above it still can.
			first = index
		}
	}
	return nil
}

// takeOneShared makes one page private for a rule, reporting whether it did.
func (r *Region) takeOneShared(ctx context.Context, page uint64) (bool, error) {
	h := r.host
	b := r.binding(page)
	if b.writable() || r.checkpointCopy(b) != nil {
		return b.writable(), nil
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
	slot, _ := h.place(r, page)
	h.mu.Unlock()
	if slot < 0 {
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
		return false, h.abandonSlots(ctx, slot, 1, nil)
	}
	private, err := h.create(ctx, slot, data, pageKey{}, true, r.kind)
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
	if err := r.takePrivate(ctx, b, old, private, spill, origin); err != nil {
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
func (r *Region) makeWhole(ctx context.Context, index uint64) (bool, error) {
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
	if err := r.takeShared(ctx, first, index, last); err != nil {
		return false, err
	}
	if r.privatePages(first, last) <= before {
		return false, nil
	}
	h.markWhole(r, index)
	h.mu.Lock()
	first, last = h.placedRun(r, index, first, last)
	slot := h.placedSlot(r, first)
	h.stats.MappingMerges++
	h.mu.Unlock()
	// The whole run in one command, which is the point: the mappings the range's
	// alternations were costing the VMM go, and what is left is one.
	count := int(last - first)
	for page := first; page < last; page++ {
		r.setMapped(r.binding(page), true)
	}
	if err := r.mapPages(ctx, first, slot, count, true); err != nil {
		return false, r.mappingFailed(err, func() { r.unmapPages(first, count) })
	}
	return true, nil
}

// placedSlot is the arena offset the placement rule gives one page, or -1 where
// its range owns no extent. Caller holds h.mu.
func (h *Host) placedSlot(r *Region, page uint64) int {
	e := h.extents[extentKey{r, page / uint64(h.extentPages)}]
	if e == nil {
		return -1
	}
	return e.base + int(page%uint64(h.extentPages))
}

// privatePages is how many pages of one range this region may store into where
// they are, which is what a range made whole has all of.
func (r *Region) privatePages(first, last uint64) int {
	count := 0
	for page := first; page < last; page++ {
		if r.isPrivateAt(page) {
			count++
		}
	}
	return count
}
