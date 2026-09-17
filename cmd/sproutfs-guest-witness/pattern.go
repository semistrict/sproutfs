package main

import "encoding/binary"

// The pattern a witness holds is a pure function of three numbers: the seed the
// guest was filled under, the step it has been mutated to, and the page. That
// is the whole design. Nothing about what a guest should hold is remembered
// anywhere — not in the guest, not on its disk, not in the control plane — so a
// script that knows one VM's (seed, step) can demand every byte of its memory
// and its disk after a fork, a migration, a stop, a start or a host loss, and a
// guest that came back holding another instant's bytes is caught by arithmetic
// the guest had no part in.
//
// It is built out of one 64-bit avalanche and nothing else: no allocation, no
// table, no dependency. A page of one (seed, step, page) shares no eight-byte
// word with a page of any other, so memory that came back with two pages
// swapped, a page of an older checkpoint, or one torn word is a mismatch with
// an offset rather than a pass.

const (
	// pageBytes is the unit the pattern is generated and mutated in. It is the
	// guest's own page rather than the host's 2 MiB one, so a step's scattered
	// fraction dirties host pages sparsely: the checkpoint behind it seals whole
	// 2 MiB pages of which the guest rewrote a little, which is the expensive
	// case and therefore the one worth measuring.
	pageBytes = 4096
	// mutateInverse is one over the fraction of pages a step rewrites: one page
	// in eight. Enough that a step is a scattered set across the whole range,
	// and little enough that the pages an earlier step wrote go on being the
	// expectation — a witness whose every page was rewritten every step would
	// never test that an untouched page survived anything.
	mutateInverse = 8
)

// mix is splitmix64's finaliser: one 64-bit avalanche. Every bit of the output
// depends on every bit of the input, which is what makes two pattern words that
// differ in one input bit share nothing.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// The odd constants each input is folded through before they are combined, so
// that a seed, a step and a page index of the same small number do not cancel.
const (
	seedSalt = 0x9e3779b97f4a7c15
	stepSalt = 0x632be59bd9b4e019
	pageSalt = 0xc2b2ae3d27d4eb4f
)

// key is the 64-bit value one page of the pattern is generated from.
func key(seed, step, page uint64) uint64 {
	return mix(mix(seed*seedSalt) ^ mix(step*stepSalt) ^ mix(page*pageSalt))
}

// writePage fills dst with the pattern of one page. dst is at most pageBytes,
// and a shorter one is the tail of a range that is not a whole page — which
// nothing here produces, because a witness is a whole number of pages.
func writePage(dst []byte, seed, step, page uint64) {
	base := key(seed, step, page)
	var word [8]byte
	for index := 0; index*8 < len(dst); index++ {
		binary.LittleEndian.PutUint64(word[:], mix(base+uint64(index)*seedSalt))
		copy(dst[index*8:], word[:])
	}
}

// touches reports whether one step rewrites one page. Step zero is the fill and
// writes every page, which is what makes every later step's expectation well
// defined: every page has a step that last wrote it.
//
// Every other step takes a seeded fraction, drawn from the same avalanche, so
// the pages one step writes are scattered over the whole range and are not the
// pages the step before it wrote.
func touches(seed, step, page uint64) bool {
	if step == 0 {
		return true
	}
	return key(seed, step, page)%mutateInverse == 0
}

// owner is the latest step at or below step that wrote one page, which is the
// step whose pattern that page must hold. A page a later step did not touch
// still holds what an earlier one wrote, so a check works this out per page
// rather than expecting one step's pattern everywhere.
//
// It walks down from step, which is a handful of iterations: a run is counted
// in rounds, not in millions.
func owner(seed, step, page uint64) uint64 {
	for at := step; at > 0; at-- {
		if touches(seed, at, page) {
			return at
		}
	}
	return 0
}

// expect fills dst with what one page must hold at (seed, step), whichever step
// last wrote it.
func expect(dst []byte, seed, step, page uint64) {
	writePage(dst, seed, owner(seed, step, page), page)
}
