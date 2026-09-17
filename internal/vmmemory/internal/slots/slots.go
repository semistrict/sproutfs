// Package slots is the pager's arena free set: which of a fixed number of
// arena slots hold no frame, and the consecutive runs of them that let
// consecutive pages become one mapping command. It accounts for nothing else —
// the resource reservation a slot costs and the statistics it moves belong to
// the host, which serializes every call here under its own lock.
package slots

import "math/bits"

// scanSlots bounds the search one run costs. A fragmented arena pays a shorter
// run rather than a long search.
const scanSlots = 1 << 16

// Set holds one bit per slot; a set bit means free. Its zero value holds no
// slots at all, so a host builds one with New.
type Set struct {
	words []uint64
	total int
	free  int
	// hint is the lowest slot that may be free, which is what keeps a scan
	// from walking the taken prefix of a nearly full arena every time.
	hint int
}

// New returns a set of total slots, all free.
func New(total int) *Set {
	s := &Set{words: make([]uint64, (total+63)/64), total: total, free: total}
	for slot := range total {
		s.words[slot/64] |= 1 << (slot % 64)
	}
	return s
}

// Total is how many slots the set covers, free or not.
func (s *Set) Total() int { return s.total }

// Free is how many slots hold no frame.
func (s *Set) Free() int { return s.free }

// IsFree reports whether one slot holds no frame.
func (s *Set) IsFree(slot int) bool { return s.words[slot/64]&(1<<(slot%64)) != 0 }

// Take removes count consecutive free slots starting at slot.
func (s *Set) Take(slot, count int) {
	for i := slot; i < slot+count; i++ {
		s.words[i/64] &^= 1 << (i % 64)
	}
	s.free -= count
}

// Put returns one slot to the set.
func (s *Set) Put(slot int) {
	s.words[slot/64] |= 1 << (slot % 64)
	s.free++
	s.hint = min(s.hint, slot)
}

// First returns the lowest free slot, or -1.
func (s *Set) First() int {
	if s.free == 0 {
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

// LongestRun finds the longest run of consecutive free slots, up to want, in a
// bounded scan from the lowest free one. It takes nothing: the caller decides
// whether it can afford the run and calls Take for the part it keeps. With no
// free slot at all it reports a length of zero.
func (s *Set) LongestRun(want int) (start, length int) {
	first := s.First()
	if first < 0 {
		return -1, 0
	}
	bestStart, bestLen := -1, 0
	runStart, runLen := -1, 0
	limit := min(s.total, first+scanSlots)
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
