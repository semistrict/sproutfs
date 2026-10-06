package vmmemory

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

// placedAt reports whether one page's resident slot is the one the placement
// rule gives it, which is what makes it part of a run of its range rather than
// a page of its own somewhere in the arena. Caller holds h.mu.
func (h *Host) placedAt(r *MemoryRegion, index uint64, at fileSlot) bool {
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	return e != nil && at == h.slotIn(e, index)
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

// placedSlot is the slot of r's private file the placement rule gives one page,
// or slot -1 of it where its range owns no extent. Caller holds h.mu.
func (h *Host) placedSlot(r *MemoryRegion, page uint64) fileSlot {
	e := h.extents[extentKey{r, page / uint64(h.extentPages)}]
	if e == nil {
		return fileSlot{r.privateFile(), -1}
	}
	return h.slotIn(e, page)
}
