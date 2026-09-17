package main

import (
	"bytes"
	"testing"
)

// TestThePatternIsAPureFunctionOfSeedStepAndPage: the whole point of the
// pattern is that the expectation lives outside the guest. A script that knows
// (seed, step) knows every byte the guest must hold, so nothing has to be
// carried across a fork, a migration or a stop for a check to mean something.
func TestThePatternIsAPureFunctionOfSeedStepAndPage(t *testing.T) {
	first := make([]byte, pageBytes)
	again := make([]byte, pageBytes)
	writePage(first, 7, 3, 11)
	writePage(again, 7, 3, 11)
	if !bytes.Equal(first, again) {
		t.Fatal("one page of the pattern came out differently twice")
	}
	for _, other := range []struct {
		name             string
		seed, step, page uint64
	}{
		{"another seed", 8, 3, 11},
		{"another step", 7, 4, 11},
		{"another page", 7, 3, 12},
	} {
		differs := make([]byte, pageBytes)
		writePage(differs, other.seed, other.step, other.page)
		if bytes.Equal(first, differs) {
			t.Fatalf("%s produced the same page: a check could not tell them apart", other.name)
		}
	}
}

// TestAPageHasNoRunsInCommonWithItsNeighbours. A pattern of repeated bytes
// would pass a check over memory that had one page swapped for the next, which
// is exactly the defect a fork or a migration could introduce.
func TestAPageHasNoRunsInCommonWithItsNeighbours(t *testing.T) {
	page := make([]byte, pageBytes)
	next := make([]byte, pageBytes)
	writePage(page, 1, 0, 100)
	writePage(next, 1, 0, 101)
	for offset := 0; offset+8 <= pageBytes; offset += 8 {
		if bytes.Equal(page[offset:offset+8], next[offset:offset+8]) {
			t.Fatalf("two pages share the word at offset %d", offset)
		}
	}
	// And no page is a run of one byte, which a memcmp of the wrong length
	// would not catch either.
	if bytes.Count(page, page[:1]) == pageBytes {
		t.Fatal("a page of the pattern is one byte repeated")
	}
}

// TestAStepScattersItsMutationAcrossTheWholeRange: a step that rewrote one
// contiguous run would leave the host publishing one run of dirty pages, which
// is the easy case. The pages a step touches are spread over the range and are
// a fraction of it, so a checkpoint has a scattered dirty set to seal.
func TestAStepScattersItsMutationAcrossTheWholeRange(t *testing.T) {
	const pages = 4096
	touchedIn := func(step uint64) (count int, firstQuarter, lastQuarter int) {
		for page := range uint64(pages) {
			if !touches(5, step, page) {
				continue
			}
			count++
			if page < pages/4 {
				firstQuarter++
			}
			if page >= 3*pages/4 {
				lastQuarter++
			}
		}
		return count, firstQuarter, lastQuarter
	}
	count, first, last := touchedIn(1)
	if count == 0 || count == pages {
		t.Fatalf("a step touched %d of %d pages, want a fraction of them", count, pages)
	}
	if first == 0 || last == 0 {
		t.Fatalf("a step touched %d pages in the first quarter and %d in the last: it is not scattered",
			first, last)
	}
	// A different step touches a different set, so successive steps do not
	// rewrite the same pages over and over.
	other, _, _ := touchedIn(2)
	same := 0
	for page := range uint64(pages) {
		if touches(5, 1, page) && touches(5, 2, page) {
			same++
		}
	}
	if same == count || same == other {
		t.Fatalf("two steps touch the same %d pages", same)
	}
}

// TestStepZeroWritesEveryPage, which is what makes every later step's
// expectation well defined: every page has a step that last wrote it.
func TestStepZeroWritesEveryPage(t *testing.T) {
	for page := range uint64(1024) {
		if !touches(3, 0, page) {
			t.Fatalf("the fill left page %d unwritten", page)
		}
	}
}

// TestOwnerIsTheLatestStepThatWroteAPage, which is the whole of what a check
// has to work out: a page a later step did not touch still holds what an
// earlier one wrote.
func TestOwnerIsTheLatestStepThatWroteAPage(t *testing.T) {
	for page := range uint64(512) {
		for step := range uint64(5) {
			at := owner(9, step, page)
			if at > step {
				t.Fatalf("page %d at step %d is owned by the later step %d", page, step, at)
			}
			if at != step && touches(9, step, page) {
				t.Fatalf("page %d was written by step %d and is owned by %d", page, step, at)
			}
			for later := at + 1; later <= step; later++ {
				if touches(9, later, page) {
					t.Fatalf("page %d is owned by step %d, and step %d wrote it after", page, at, later)
				}
			}
		}
	}
}
