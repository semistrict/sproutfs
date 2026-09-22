// Package slots is the pager's arena addresses: which of its offsets hold no
// page, the consecutive runs of them that let consecutive pages become one
// mapping command, the extents a region places its private pages in, and how
// many pages it may hold at once. It accounts for nothing else — the resource
// reservation a page costs and the statistics it moves belong to the host,
// which serializes every call here under its own lock.
//
// An offset and a page are not the same thing. An offset is an address in the
// arena, and the arena is a sparse file: an offset holds memory only once a
// page is put there, and none once it is given back. A page is that memory. A
// pager whose private pages are placed at an offset of their own — so that
// pages adjacent in a guest are adjacent in the arena and are one mapping —
// needs far more of the first than of the second, because a range holding a
// single private page still owns the whole run of offsets its other pages would
// go at. So the two are separate numbers here, and what bounds an allocation is
// the pages.
//
// The space is in two parts. The offsets below Pages are ordinary: a page goes
// at one of them one at a time, or a run of them at once, which is what
// read-ahead and shared pages take. Everything above is carved into extents of
// a fixed size, aligned to it, handed out and taken back whole; within one, a
// page is put at an offset the caller chooses, because the whole point of an
// extent is that the caller decides where in it a page goes.
package slots

import "math/bits"

// scanSlots bounds the search one run costs. A fragmented arena pays a shorter
// run rather than a long search.
const scanSlots = 1 << 16

// Space holds one bit per offset; a set bit means the offset holds no page and
// belongs to no extent. Its zero value holds nothing at all, so a host builds
// one with New.
type Space struct {
	words []uint64
	// offsets is how many addresses the arena has and pages how many of them
	// may hold memory at once; held is how many do.
	offsets int
	pages   int
	held    int
	// hint is the lowest ordinary offset that may be unoccupied, which is what
	// keeps a scan from walking the taken prefix of a nearly full arena every
	// time.
	hint int
	// ordinary is where the extents begin: the offsets below it are given out
	// one page at a time, and the rest belong to extents. It is pages rounded up
	// to a whole extent, so every extent is aligned to its own size and an
	// offset's place within one is the low bits of its address.
	ordinary int
	// extent is how many offsets one extent has, and free the extents nothing
	// owns, by their first offset. An extent of one offset or none is a pager
	// that places nothing, and then there are no extents at all.
	extent int
	free   []int
}

// New returns a space of offsets addresses, of which at most pages may hold
// memory at once. A page with nowhere to be is not a configuration: pages
// greater than offsets panics, because it is the caller's own arithmetic and
// not a runtime condition.
//
// extent, where it is given and greater than one, is how many offsets one
// extent has. The room past the pages is carved into as many whole extents as
// it holds, each aligned to that size; a space with no room past its pages has
// none, which is the pager whose offsets and pages are one number.
func New(offsets, pages int, extent ...int) *Space {
	if pages > offsets {
		panic("slots: more pages than offsets")
	}
	s := &Space{words: make([]uint64, (offsets+63)/64), offsets: offsets, pages: pages, ordinary: offsets}
	for slot := range offsets {
		s.words[slot/64] |= 1 << (slot % 64)
	}
	if len(extent) != 0 && extent[0] > 1 {
		s.extent = extent[0]
		// The extents start at the first aligned offset past the ordinary ones,
		// so an extent's own offsets are a multiple of its size and a page's
		// place within one is its page number modulo that size. The offsets
		// between the pages and that boundary belong to nobody and are marked
		// taken, so nothing hands one out.
		s.ordinary = min((pages+s.extent-1)/s.extent*s.extent, offsets)
		// Every offset past the ordinary ones reads as taken for the whole of
		// this space's life: it is an extent's, or the alignment between the
		// pages and the first extent, or a tail too short to be one, and none of
		// the three may ever be handed out a page at a time. Which extents are
		// free is the list below, not the bitmap.
		for slot := pages; slot < offsets; slot++ {
			s.words[slot/64] &^= 1 << (slot % 64)
		}
		// Descending, because TakeExtent pops the last: the lowest extent is
		// handed out first, so ranges a guest writes into in order get
		// consecutive extents and their mappings merge rather than multiply.
		for base := (offsets-s.ordinary)/s.extent*s.extent + s.ordinary - s.extent; base >= s.ordinary; base -= s.extent {
			s.free = append(s.free, base)
		}
	}
	return s
}

// Offsets is how many addresses the arena has, occupied or not.
func (s *Space) Offsets() int { return s.offsets }

// Pages is how many of them may hold memory at once, which is the arena's
// capacity and what a deployment budgets.
func (s *Space) Pages() int { return s.pages }

// Held is how many offsets hold a page.
func (s *Space) Held() int { return s.held }

// Free is how many more pages this space may hold. It is the page budget and
// not the offsets left: an arena with room for one more page has room for one
// more page wherever its addresses are.
func (s *Space) Free() int { return s.pages - s.held }

// ExtentOffsets is how many offsets one extent has, zero where this space has
// none.
func (s *Space) ExtentOffsets() int { return s.extent }

// Extents is how many extents the space was carved into, and FreeExtents how
// many of them no region owns.
func (s *Space) Extents() int {
	if s.extent == 0 {
		return 0
	}
	return (s.offsets - s.ordinary) / s.extent
}
func (s *Space) FreeExtents() int { return len(s.free) }

// TakeExtent gives one extent's offsets to a caller whole, reporting its first
// offset, or -1 where the space has none left. Every offset of it is the
// caller's from then on, holding a page or not, until PutExtent takes it back.
func (s *Space) TakeExtent() int {
	if len(s.free) == 0 {
		return -1
	}
	base := s.free[len(s.free)-1]
	s.free = s.free[:len(s.free)-1]
	return base
}

// PutExtent takes one extent back. The caller has already emptied it: an offset
// of it still holding a page is a page nothing would ever release.
func (s *Space) PutExtent(base int) { s.free = append(s.free, base) }

// Fill records a page put at one offset of an extent the caller already owns,
// and Empty the page leaving it again. Only the memory moves: the offset is the
// extent's either way, which is what keeps a range's private pages adjacent
// however many of them are there at once. Free bounds what may be filled and it
// is the host that checks it.
func (s *Space) Fill()  { s.held++ }
func (s *Space) Empty() { s.held-- }

// IsFree reports whether one offset may be given a page on its own: an
// ordinary offset holding none. An offset of an extent is never one, taken or
// free, because only the placement rule may put a page there.
func (s *Space) IsFree(slot int) bool { return s.words[slot/64]&(1<<(slot%64)) != 0 }

// Take puts count pages at count consecutive unoccupied ordinary offsets
// starting at slot. The caller has already found room for them: Free bounds
// what may be taken and it is the host that checks it.
func (s *Space) Take(slot, count int) {
	for i := slot; i < slot+count; i++ {
		s.words[i/64] &^= 1 << (i % 64)
	}
	s.held += count
}

// Put takes the page at one ordinary offset away, leaving the offset
// unoccupied.
func (s *Space) Put(slot int) {
	s.words[slot/64] |= 1 << (slot % 64)
	s.held--
	s.hint = min(s.hint, slot)
}

// First returns the lowest unoccupied ordinary offset a page may be put at, or
// -1. A space with no page left has none, however many of its addresses are
// unoccupied.
func (s *Space) First() int {
	if s.Free() == 0 {
		return -1
	}
	for word := s.hint / 64; word < len(s.words); word++ {
		if s.words[word] != 0 {
			slot := word*64 + bits.TrailingZeros64(s.words[word])
			if slot >= s.ordinary {
				return -1
			}
			s.hint = slot
			return slot
		}
	}
	return -1
}

// LongestRun finds the longest run of consecutive unoccupied ordinary offsets,
// up to want and never longer than the pages left, in a bounded scan from the
// lowest unoccupied one. It takes nothing: the caller decides whether it can
// afford the run and calls Take for the part it keeps. With no page left at all
// it reports a length of zero.
func (s *Space) LongestRun(want int) (start, length int) {
	want = min(want, s.Free())
	if want < 1 {
		return -1, 0
	}
	first := s.First()
	if first < 0 {
		return -1, 0
	}
	bestStart, bestLen := -1, 0
	runStart, runLen := -1, 0
	limit := min(s.ordinary, first+scanSlots)
	for slot := first; slot < limit; slot++ {
		word := s.words[slot/64]
		if slot%64 == 0 && word == 0 {
			slot += 63
			runStart, runLen = -1, 0
			continue
		}
		if word&(1<<(slot%64)) != 0 {
			if runLen == 0 {
				runStart = slot
			}
			runLen++
			if runLen > bestLen {
				bestStart, bestLen = runStart, runLen
			}
			if runLen == want {
				break
			}
		} else {
			runStart, runLen = -1, 0
		}
	}
	return bestStart, bestLen
}
