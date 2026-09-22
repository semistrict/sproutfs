package vmmemory

import "context"

// A private page lives at its own offset.
//
// Every separately mapped run of a guest's memory is a mapping in its VMM
// process, and a private page written into the middle of an inherited run makes
// three of one. What a range costs in mappings should be how often it
// alternates between shared and private, not how many of its pages are private
// — and that is true exactly when private pages adjacent in the guest are
// adjacent in the arena, whatever order they were written in.
//
// So each 2 MiB-aligned range of a region that holds a private page owns one
// extent: rangeBytes worth of consecutive arena offsets, of which only the
// pages stored into hold memory. A private page of that range is put at the
// offset within the extent that it has within the range. Extents come off the
// offset space and go back to it whole, and the arena accounts pages, not
// extents — an extent is addresses, and addresses are what a sparse file has
// for nothing.
//
// Two departures are deliberate and named where they happen. A store that
// copies away from the copy a checkpoint froze cannot have its own offset,
// because that offset is holding the bytes the checkpoint is uploading; it
// takes an ordinary one until that checkpoint retires. And a page a migration
// destination loads privately from the host that still holds it arrives in a
// run of its own, like any other load, rather than in its range's extent.

// rangeBytes is what one extent covers: the 2 MiB-aligned range of a region
// whose private pages are placed together. It is the largest page a volume may
// be published in, which is what makes it the unit a whole range could be given
// a huge mapping at.
const rangeBytes = 2 << 20

// extentKey names the one range of one region an extent belongs to.
type extentKey struct {
	region *Region
	rng    uint64
}

// extent is the run of consecutive arena offsets one range owns. held is how
// many of those offsets hold a page; the extent goes back to the offset space
// when the last of them is given up, so a region owns an extent for exactly as
// long as it has a page in the range.
type extent struct {
	key  extentKey
	base int
	held int
}

// placing reports whether this pager places anything. A pager whose page is the
// whole range has one page per range, so placement has nothing to decide and
// its offsets and its pages are one number; so has one whose offset space is no
// larger than its capacity, which is a pager with no extents to give out.
func (h *Host) placing() bool { return h.extentPages > 1 && h.slots.Extents() > 0 }

// place reports the arena offset the placement rule gives page index of region
// r, having taken a page there.
//
// It reports -1 with placeable false where this page has no offset of its own:
// a pager that places nothing, an offset space with no extent left for a range
// that does not already own one, or a page whose own offset is already holding
// the bytes a checkpoint froze. The store falls back to an ordinary offset
// then. It reports -1 with placeable true where the offset is this page's and
// the page budget is what is missing, which an eviction anywhere makes room
// for.
//
// Caller holds h.mu.
func (h *Host) place(r *Region, index uint64) (slot int, placeable bool) {
	if !h.placing() {
		return -1, false
	}
	key := extentKey{r, index / uint64(h.extentPages)}
	e := h.extents[key]
	if e == nil {
		base := h.slots.TakeExtent()
		if base < 0 {
			return -1, false
		}
		e = &extent{key: key, base: base}
		h.extents[key] = e
	}
	slot = e.base + int(index%uint64(h.extentPages))
	if _, taken := h.residentLeases[slot]; taken {
		// The page's own offset holds an older version of it — the copy a
		// checkpoint froze, which is being uploaded from where it is. The store
		// takes an ordinary offset until that checkpoint retires.
		h.dropExtent(e)
		return -1, false
	}
	if h.slots.Free() == 0 {
		h.dropExtent(e)
		return -1, true
	}
	lease, err := h.resources.TryAcquire(context.Background(), int64(h.pageSize))
	if err != nil {
		h.dropExtent(e)
		return -1, true
	}
	h.slots.Fill()
	e.held++
	h.residentLeases[slot] = residentSlot{lease: lease, extent: e}
	h.stats.PeakResidentPages = max(h.stats.PeakResidentPages, h.slots.Held())
	return slot, true
}

// dropExtent gives an extent back to the offset space once nothing of it holds
// a page. A region that has detached is no longer in the extent table, so the
// entry is removed only where it is still this extent's. Caller holds h.mu.
func (h *Host) dropExtent(e *extent) {
	if e.held != 0 {
		return
	}
	if h.extents[e.key] == e {
		delete(h.extents, e.key)
	}
	h.slots.PutExtent(e.base)
}

// forgetExtents takes a detached region's extents out of the table. The offsets
// go back as their pages do: a page of one may outlive the region that placed
// it, because retiring a checkpoint publishes it and another region that
// inherits that identity maps it where it is.
func (h *Host) forgetExtents(r *Region) {
	for key, e := range h.extents {
		if key.region == r {
			delete(h.extents, key)
			if e.held == 0 {
				h.slots.PutExtent(e.base)
			}
		}
	}
}

// placeRun takes arena offsets for the pages [first, last), which hold index,
// by the placement rule. It reports the pages it covered and one run per set of
// consecutive offsets: a window crossing a range boundary is two extents, and
// two runs unless those extents are themselves consecutive.
//
// The faulting page's range comes first — without it there is no run at all —
// then the ranges after it and then those before it, stopping at the first that
// has no extent or no page budget left. It takes nothing it does not keep.
func (h *Host) placeRun(r *Region, index, first, last uint64) (uint64, []MapRun) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.placing() {
		return 0, nil
	}
	span := uint64(h.extentPages)
	from, to := max(first, index-index%span), min(last, index-index%span+span)
	base, ok := h.placePages(r, from, to)
	if !ok {
		return 0, nil
	}
	runs := []MapRun{{Page: from, Slot: base, Count: int(to - from)}}
	for to < last {
		end := min(last, to+span)
		base, ok := h.placePages(r, to, end)
		if !ok {
			break
		}
		runs = append(runs, MapRun{Page: to, Slot: base, Count: int(end - to)})
		to = end
	}
	for from > first {
		start := max(first, from-span)
		base, ok := h.placePages(r, start, from)
		if !ok {
			break
		}
		runs = append([]MapRun{{Page: start, Slot: base, Count: int(from - start)}}, runs...)
		from = start
	}
	return from, mergeRuns(runs)
}

// placePages places every page of one range's part of a window, which lands
// them at consecutive offsets of that range's extent. It reports the first of
// those offsets. A page it cannot place undoes the pages before it, so the
// caller gets the whole sub-run or nothing. Caller holds h.mu.
func (h *Host) placePages(r *Region, from, to uint64) (int, bool) {
	base := -1
	for page := from; page < to; page++ {
		slot, _ := h.place(r, page)
		if slot < 0 {
			for undo := from; undo < page; undo++ {
				h.putFree(base + int(undo-from))
			}
			return 0, false
		}
		if base < 0 {
			base = slot
		}
	}
	return base, base >= 0
}

// mergeRuns joins runs whose pages and whose offsets both continue, which is
// what two extents the offset space handed out consecutively come to.
func mergeRuns(runs []MapRun) []MapRun {
	merged := runs[:1]
	for _, run := range runs[1:] {
		last := &merged[len(merged)-1]
		if last.Page+uint64(last.Count) == run.Page && last.Slot+last.Count == run.Slot {
			last.Count += run.Count
			continue
		}
		merged = append(merged, run)
	}
	return merged
}
