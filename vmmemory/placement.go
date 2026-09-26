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
// So each 2 MiB-aligned range of a memory region that holds a private page owns one
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
//
// An isolated arena gives each memory region a private file of its own, so no
// extent has to be carved out of anything: a page's offset in its region's
// private file is its index, and a range's extent is that range of the file.
// The second half of the file is each page's other place, which is where the
// first departure goes; see isolation.go.

// rangeBytes is what one extent covers: the 2 MiB-aligned range of a memory region
// whose private pages are placed together. It is the largest page a volume may
// be published in, which is what makes it the unit a whole range could be given
// a huge mapping at.
const rangeBytes = 2 << 20

// extentKey names the one range of one memory region an extent belongs to.
type extentKey struct {
	memoryRegion *MemoryRegion
	rng          uint64
}

// extent is the run of consecutive slots of one file that one range owns.
// held is how many of those slots hold a page; the extent goes back to its
// file when the last of them is given up, so a memory region owns an extent for
// exactly as long as it has a page in the range.
type extent struct {
	key  extentKey
	file *arenaFile
	base int
	held int
	// whole marks a range the half-private rule has filled. A whole range stays
	// whole: it is one mapping and one write-protect command at a seal, and a
	// settle handing one of its pages back would break it up again for a page
	// the guest is about to write anyway. It goes with the extent.
	whole bool
	// fixed marks the extent of a private file, which is that range of the
	// file and is carved out of nothing: its offsets are taken and given back
	// one page at a time, and the extent itself is never handed back.
	fixed bool
}

// placing reports whether this pager places anything in f. A pager whose page
// is the whole range has one page per range, so placement has nothing to
// decide and its offsets and its pages are one number; so has a file whose
// offsets are no more than its capacity, which is a file with no extents to
// give out. A private file is placed in whatever the page: a page's offset
// in it is its index.
func (h *Host) placing(f *arenaFile) bool {
	return f.owner != nil || h.carving(f)
}

// carving reports whether f's extents are carved out of its offset space, and
// so can run out.
func (h *Host) carving(f *arenaFile) bool {
	return f.owner == nil && h.extentPages > 1 && f.slots.Extents() > 0
}

// slotIn is the slot the placement rule gives page within extent e.
func (h *Host) slotIn(e *extent, page uint64) fileSlot {
	return fileSlot{e.file, e.base + int(page%uint64(h.extentPages))}
}

// place reports the slot of r's private file the placement rule gives page
// index of memory region r, having taken a page there.
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
func (h *Host) place(r *MemoryRegion, index uint64) (at fileSlot, placeable bool) {
	f := r.privateFile()
	none := fileSlot{f, -1}
	if !h.placing(f) {
		return none, false
	}
	if f.owner != nil && h.extentPages == 1 {
		// A page that is the whole range is its range's only page: its offset
		// is its own, and there is no run for an extent to keep together.
		at = fileSlot{f, int(index)}
		if _, taken := f.leases[at.slot]; taken {
			return none, false
		}
		if !h.takeFree(at, 1) {
			return none, true
		}
		return at, true
	}
	key := extentKey{r, index / uint64(h.extentPages)}
	e := h.extents[key]
	if e == nil {
		if f.owner != nil {
			e = &extent{key: key, file: f, base: int(key.rng) * h.extentPages, fixed: true}
		} else {
			base := f.slots.TakeExtent()
			if base < 0 {
				return none, false
			}
			e = &extent{key: key, file: f, base: base}
		}
		h.extents[key] = e
	}
	at = h.slotIn(e, index)
	if _, taken := at.file.leases[at.slot]; taken {
		// The page's own offset holds an older version of it — the copy a
		// checkpoint froze, which is being uploaded from where it is. The store
		// takes an ordinary offset until that checkpoint retires.
		h.dropExtent(e)
		return none, false
	}
	if h.freeLocked(f) == 0 {
		h.dropExtent(e)
		return none, true
	}
	lease, err := h.resources.TryAcquire(context.Background(), int64(h.pageSize))
	if err != nil {
		h.dropExtent(e)
		return none, true
	}
	if e.fixed {
		f.slots.Take(at.slot, 1)
	} else {
		f.slots.Fill()
	}
	h.held++
	e.held++
	f.leases[at.slot] = residentSlot{lease: lease, extent: e}
	h.stats.PeakResidentPages = max(h.stats.PeakResidentPages, h.heldLocked())
	return at, true
}

// dropExtent gives an extent back to its file once nothing of it holds a
// page. A memory region that has detached is no longer in the extent table, so the
// entry is removed only where it is still this extent's. Caller holds h.mu.
func (h *Host) dropExtent(e *extent) {
	if e.held != 0 {
		return
	}
	if h.extents[e.key] == e {
		delete(h.extents, e.key)
	}
	if !e.fixed {
		e.file.slots.PutExtent(e.base)
	}
}

// forgetExtents takes a detached memory region's extents out of the table. The offsets
// go back as their pages do: a page of one may outlive the memory region that placed
// it, because retiring a checkpoint publishes it and another memory region that
// inherits that identity maps it where it is.
func (h *Host) forgetExtents(r *MemoryRegion) {
	for key, e := range h.extents {
		if key.memoryRegion == r {
			delete(h.extents, key)
			if e.held == 0 && !e.fixed {
				e.file.slots.PutExtent(e.base)
			}
		}
	}
}

// placeRun takes slots of r's private file for the pages [first, last), which
// hold index, by the placement rule. It reports the pages it covered and one
// run per set of consecutive slots: a window crossing a range boundary is two
// extents, and two runs unless those extents are themselves consecutive.
//
// The faulting page's range comes first — without it there is no run at all —
// then the ranges after it and then those before it, stopping at the first that
// has no extent or no page budget left. It takes nothing it does not keep.
func (h *Host) placeRun(r *MemoryRegion, index, first, last uint64) (uint64, []MapRun) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.placing(r.privateFile()) {
		return 0, nil
	}
	span := uint64(h.extentPages)
	from, to := max(first, index-index%span), min(last, index-index%span+span)
	base, ok := h.placePages(r, from, to)
	if !ok {
		return 0, nil
	}
	runs := []MapRun{r.runAt(from, base, int(to-from))}
	for to < last {
		end := min(last, to+span)
		base, ok := h.placePages(r, to, end)
		if !ok {
			break
		}
		runs = append(runs, r.runAt(to, base, int(end-to)))
		to = end
	}
	for from > first {
		start := max(first, from-span)
		base, ok := h.placePages(r, start, from)
		if !ok {
			break
		}
		runs = append([]MapRun{r.runAt(start, base, int(from-start))}, runs...)
		from = start
	}
	return from, mergeRuns(runs)
}

// placePages places every page of one range's part of a window, which lands
// them at consecutive offsets of that range's extent. It reports the first of
// those offsets. A page it cannot place undoes the pages before it, so the
// caller gets the whole sub-run or nothing. Caller holds h.mu.
func (h *Host) placePages(r *MemoryRegion, from, to uint64) (fileSlot, bool) {
	base := fileSlot{slot: -1}
	for page := from; page < to; page++ {
		at, _ := h.place(r, page)
		if at.slot < 0 {
			for undo := from; undo < page; undo++ {
				h.putFree(base.plus(int(undo - from)))
			}
			return fileSlot{}, false
		}
		if base.slot < 0 {
			base = at
		}
	}
	return base, base.slot >= 0
}

// mergeRuns joins runs whose pages and whose offsets in one file both continue,
// which is what two extents the offset space handed out consecutively come to.
func mergeRuns(runs []MapRun) []MapRun {
	merged := runs[:1]
	for _, run := range runs[1:] {
		last := &merged[len(merged)-1]
		if last.Page+uint64(last.Count) == run.Page && last.File == run.File && last.Slot+last.Count == run.Slot {
			last.Count += run.Count
			continue
		}
		merged = append(merged, run)
	}
	return merged
}
