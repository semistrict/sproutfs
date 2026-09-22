// Package slots is the pager's arena addresses: which of its offsets hold no
// page, the consecutive runs of them that let consecutive pages become one
// mapping command, and how many pages it may hold at once. It accounts for
// nothing else — the resource reservation a page costs and the statistics it
// moves belong to the host, which serializes every call here under its own
// lock.
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
package slots

import "math/bits"

// scanSlots bounds the search one run costs. A fragmented arena pays a shorter
// run rather than a long search.
const scanSlots = 1 << 16

// Space holds one bit per offset; a set bit means the offset holds no page. Its
// zero value holds nothing at all, so a host builds one with New.
type Space struct {
	words []uint64
	// offsets is how many addresses the arena has and pages how many of them
	// may hold memory at once; held is how many do.
	offsets int
	pages   int
	held    int
	// hint is the lowest offset that may be unoccupied, which is what keeps a
	// scan from walking the taken prefix of a nearly full arena every time.
	hint int
}

// New returns a space of offsets addresses, of which at most pages may hold
// memory at once. A page with nowhere to be is not a configuration: pages
// greater than offsets panics, because it is the caller's own arithmetic and
// not a runtime condition.
func New(offsets, pages int) *Space {
	if pages > offsets {
		panic("slots: more pages than offsets")
	}
	s := &Space{words: make([]uint64, (offsets+63)/64), offsets: offsets, pages: pages}
	for slot := range offsets {
		s.words[slot/64] |= 1 << (slot % 64)
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

// IsFree reports whether one offset holds no page.
func (s *Space) IsFree(slot int) bool { return s.words[slot/64]&(1<<(slot%64)) != 0 }

// Take puts count pages at count consecutive unoccupied offsets starting at
// slot. The caller has already found room for them: Free bounds what may be
// taken and it is the host that checks it.
func (s *Space) Take(slot, count int) {
	for i := slot; i < slot+count; i++ {
		s.words[i/64] &^= 1 << (i % 64)
	}
	s.held += count
}

// Put takes the page at one offset away, leaving the offset unoccupied.
func (s *Space) Put(slot int) {
	s.words[slot/64] |= 1 << (slot % 64)
	s.held--
	s.hint = min(s.hint, slot)
}

// First returns the lowest unoccupied offset a page may be put at, or -1. A
// space with no page left has none, however many of its addresses are
// unoccupied.
func (s *Space) First() int {
	if s.Free() == 0 {
		return -1
	}
	for word := s.hint / 64; word < len(s.words); word++ {
		if s.words[word] != 0 {
			slot := word*64 + bits.TrailingZeros64(s.words[word])
			s.hint = slot
			return slot
		}
	}
	return -1
}

// LongestRun finds the longest run of consecutive unoccupied offsets, up to
// want and never longer than the pages left, in a bounded scan from the lowest
// unoccupied one. It takes nothing: the caller decides whether it can afford
// the run and calls Take for the part it keeps. With no page left at all it
// reports a length of zero.
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
	limit := min(s.offsets, first+scanSlots)
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
