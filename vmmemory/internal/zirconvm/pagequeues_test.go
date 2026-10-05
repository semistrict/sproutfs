// Copyright 2026 The Fuchsia Authors
// Ported from zircon/kernel/vm/unittests/page_queues_unittest.cc
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"slices"
	"testing"
)

// The ten cases of page_queues_unittest.cc, each at the two page sizes a pager
// runs at and in a synctest bubble, as every ported case does. Zircon's cases
// start the queues' threads; there are none here (see page_queues.go), so a
// rotation's LRU processing happens only when it must, and the cases that
// accept either outcome of that race expect the one that happens. The cases
// after them test what is not Zircon's.

// queuedPage is a vm_page_t that InitializeTestPage has set up: in no queue,
// with no backlink.
type queuedPage struct {
	id   int
	node PageQueueNode[*queuedPage, *testCow]
}

func (p *queuedPage) QueueNode() *PageQueueNode[*queuedPage, *testCow] { return &p.node }

// testCow is the VmCowPages a test's VMO has: a pager's, from
// make_uncommitted_pager_vmo, or anonymous, from VmObjectPaged::Create.
type testCow struct{ pager bool }

func (c *testCow) CanEvict() bool { return c.pager }

var (
	anonymousVmo = &testCow{pager: false}
	pagerVmo     = &testCow{pager: true}
)

type testQueues = PageQueues[*queuedPage, *testCow]

func newTestQueues(ps uint64) *testQueues { return NewPageQueues[*queuedPage, *testCow](ps) }

func expectCounts(t *testing.T, pq *testQueues, want Counts) {
	t.Helper()
	if got := pq.QueueCounts(); got != want {
		t.Errorf("the queue counts are %+v, want %+v", got, want)
	}
}

func expectActiveInactive(t *testing.T, pq *testQueues, active, inactive int) {
	t.Helper()
	if got, want := pq.GetActiveInactiveCounts(), (ActiveInactiveCounts{Active: active, Inactive: inactive}); got != want {
		t.Errorf("the active and inactive counts are %+v, want %+v", got, want)
	}
}

func expectReclaimAge(t *testing.T, pq *testQueues, page *queuedPage, want uint64) {
	t.Helper()
	ok, age := pq.DebugPageIsReclaim(page)
	if !ok {
		t.Fatalf("page %d is not in a reclaim queue", page.id)
	}
	if age != want {
		t.Errorf("page %d is in the reclaim queue of age %d, want %d", page.id, age, want)
	}
}

func expectPeek(t *testing.T, pq *testQueues, want *queuedPage) {
	t.Helper()
	backlink, ok := pq.PeekIsolate(NumReclaim - 1)
	if !ok {
		t.Fatalf("peeking the isolate queue found nothing, want page %d", want.id)
	}
	if backlink.Page != want {
		t.Errorf("peeking the isolate queue found page %d, want page %d", backlink.Page.id, want.id)
	}
}

func expectTrue(t *testing.T, got bool, what string) {
	t.Helper()
	if !got {
		t.Errorf("%s is false, want true", what)
	}
}

func expectFalse(t *testing.T, got bool, what string) {
	t.Helper()
	if got {
		t.Errorf("%s is true, want false", what)
	}
}

// reclaimAt is the counts of one page in the reclaim queue of age.
func reclaimAt(age int) Counts {
	var counts Counts
	counts.Reclaim[age] = 1
	return counts
}

// anonymousCases are the two ways Zircon may run its cases: with only a
// pager's pages reclaimable, where an anonymous page has a queue of its own,
// and with anonymous pages reclaimable, where it is in the MRU queue.
var anonymousCases = []struct {
	name             string
	reclaimAnonymous bool
	anonymous        Counts
}{
	{"only a pager's pages are reclaimed", false, Counts{Anonymous: 1}},
	{"anonymous pages are reclaimed", true, reclaimAt(0)},
}

// A page shows up in each queue it is set in, and in none once it is removed.
// Zircon's pq_add_remove.
func TestAPageIsInEachQueueItIsSetIn(t *testing.T) {
	for _, c := range anonymousCases {
		t.Run(c.name, func(t *testing.T) {
			forEachPageSize(t, func(t *testing.T, ps uint64) {
				pq := newTestQueues(ps)
				if c.reclaimAnonymous {
					pq.EnableAnonymousReclaim(false)
				}
				page := &queuedPage{}

				pq.SetWired(page, anonymousVmo, 0)
				expectTrue(t, pq.DebugPageIsWired(page), "wired")
				expectCounts(t, pq, Counts{Wired: 1})

				pq.Remove(page)
				expectFalse(t, pq.DebugPageIsWired(page), "wired")
				expectFalse(t, pq.DebugPageIsAnonymous(page), "anonymous")
				expectCounts(t, pq, Counts{})

				pq.SetAnonymous(page, anonymousVmo, 0, false)
				expectTrue(t, pq.DebugPageIsAnonymous(page), "anonymous")
				expectCounts(t, pq, c.anonymous)

				pq.Remove(page)
				expectFalse(t, pq.DebugPageIsAnonymous(page), "anonymous")
				expectCounts(t, pq, Counts{})

				pq.SetReclaim(page, pagerVmo, 0)
				expectReclaimAge(t, pq, page, 0)
				expectCounts(t, pq, reclaimAt(0))

				pq.Remove(page)
				reclaim, _ := pq.DebugPageIsReclaim(page)
				expectFalse(t, reclaim, "reclaim")
				expectCounts(t, pq, Counts{})

				pq.SetPagerBackedDirty(page, pagerVmo, 0)
				expectTrue(t, pq.DebugPageIsPagerBackedDirty(page), "pager backed dirty")
				expectCounts(t, pq, Counts{PagerBackedDirty: 1})

				pq.Remove(page)
				expectFalse(t, pq.DebugPageIsPagerBackedDirty(page), "pager backed dirty")
				expectCounts(t, pq, Counts{})
			})
		})
	}
}

// A page moves between queues, and a page moved to the don't-need queue is
// the first a peek finds. Zircon's pq_move_queues.
func TestAPageMovesBetweenQueues(t *testing.T) {
	for _, c := range anonymousCases {
		t.Run(c.name, func(t *testing.T) {
			forEachPageSize(t, func(t *testing.T, ps uint64) {
				pq := newTestQueues(ps)
				if c.reclaimAnonymous {
					pq.EnableAnonymousReclaim(false)
				}
				page := &queuedPage{}

				pq.SetWired(page, anonymousVmo, 0)
				expectTrue(t, pq.DebugPageIsWired(page), "wired")
				expectCounts(t, pq, Counts{Wired: 1})

				pq.MoveToAnonymous(page, false)
				expectFalse(t, pq.DebugPageIsWired(page), "wired")
				expectTrue(t, pq.DebugPageIsAnonymous(page), "anonymous")
				expectCounts(t, pq, c.anonymous)
				pq.Remove(page)

				// Now some of a pager's queues.
				pq.SetReclaim(page, pagerVmo, 0)
				expectReclaimAge(t, pq, page, 0)
				expectCounts(t, pq, reclaimAt(0))

				pq.MoveToPagerBackedDirty(page)
				expectTrue(t, pq.DebugPageIsPagerBackedDirty(page), "pager backed dirty")
				expectCounts(t, pq, Counts{PagerBackedDirty: 1})

				pq.MoveToReclaim(page)
				expectReclaimAge(t, pq, page, 0)
				expectCounts(t, pq, reclaimAt(0))

				pq.MoveToReclaimDontNeed(page)
				reclaim, _ := pq.DebugPageIsReclaim(page)
				expectFalse(t, reclaim, "reclaim")
				expectTrue(t, pq.DebugPageIsReclaimIsolate(page), "isolate")
				expectCounts(t, pq, Counts{ReclaimIsolate: 1})

				// The don't-need page is first in line for eviction.
				expectPeek(t, pq, page)

				pq.MoveToWired(page)
				expectFalse(t, pq.DebugPageIsReclaimIsolate(page), "isolate")
				reclaim, _ = pq.DebugPageIsReclaim(page)
				expectFalse(t, reclaim, "reclaim")
				expectTrue(t, pq.DebugPageIsWired(page), "wired")
				expectCounts(t, pq, Counts{Wired: 1})

				pq.Remove(page)
				expectCounts(t, pq, Counts{})
			})
		})
	}
}

// Moving a page into the queue it is already in leaves it there, counted
// once. Zircon's pq_move_self_queue.
func TestAPageMovedToItsOwnQueueStaysThere(t *testing.T) {
	for _, c := range anonymousCases {
		t.Run(c.name, func(t *testing.T) {
			forEachPageSize(t, func(t *testing.T, ps uint64) {
				pq := newTestQueues(ps)
				if c.reclaimAnonymous {
					pq.EnableAnonymousReclaim(false)
				}
				page := &queuedPage{}

				pq.SetWired(page, anonymousVmo, 0)
				expectTrue(t, pq.DebugPageIsWired(page), "wired")
				expectCounts(t, pq, Counts{Wired: 1})

				pq.MoveToWired(page)
				expectTrue(t, pq.DebugPageIsWired(page), "wired")
				expectCounts(t, pq, Counts{Wired: 1})

				pq.Remove(page)
				expectCounts(t, pq, Counts{})

				pq.SetAnonymous(page, anonymousVmo, 0, false)
				expectTrue(t, pq.DebugPageIsAnonymous(page), "anonymous")
				expectCounts(t, pq, c.anonymous)

				pq.MoveToAnonymous(page, false)
				expectTrue(t, pq.DebugPageIsAnonymous(page), "anonymous")
				expectCounts(t, pq, c.anonymous)

				pq.Remove(page)
				expectCounts(t, pq, Counts{})

				// Now some of a pager's queues.
				pq.SetReclaim(page, pagerVmo, 0)
				expectReclaimAge(t, pq, page, 0)
				expectCounts(t, pq, reclaimAt(0))

				pq.MoveToReclaim(page)
				expectReclaimAge(t, pq, page, 0)
				expectCounts(t, pq, reclaimAt(0))

				pq.Remove(page)
				expectCounts(t, pq, Counts{})

				pq.SetPagerBackedDirty(page, pagerVmo, 0)
				expectTrue(t, pq.DebugPageIsPagerBackedDirty(page), "pager backed dirty")
				expectCounts(t, pq, Counts{PagerBackedDirty: 1})

				pq.MoveToPagerBackedDirty(page)
				expectTrue(t, pq.DebugPageIsPagerBackedDirty(page), "pager backed dirty")
				expectCounts(t, pq, Counts{PagerBackedDirty: 1})

				pq.Remove(page)
				expectCounts(t, pq, Counts{})
			})
		})
	}
}

// Each rotation ages a reclaim page by one queue and leaves the wired and
// dirty pages where they are; the page is active for two rotations, and once
// past the last queue it is isolated. Zircon's pq_rotate_queue.
func TestRotatingTheQueuesAgesAReclaimPage(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pq.SetActiveRatioMultiplier(0)
		wired := &queuedPage{id: 1}
		clean := &queuedPage{id: 2}
		dirty := &queuedPage{id: 3}

		pq.SetWired(wired, pagerVmo, 0)
		pq.SetReclaim(clean, pagerVmo, 0)
		pq.SetPagerBackedDirty(dirty, pagerVmo, 0)
		expectTrue(t, pq.DebugPageIsWired(wired), "wired")
		expectTrue(t, pq.DebugPageIsPagerBackedDirty(dirty), "pager backed dirty")
		expectReclaimAge(t, pq, clean, 0)
		with := func(counts Counts) Counts {
			counts.PagerBackedDirty, counts.Wired = 1, 1
			return counts
		}
		expectCounts(t, pq, with(reclaimAt(0)))
		expectActiveInactive(t, pq, 1, 0)

		// Gradually rotate the queues.
		pq.RotateReclaimQueues()
		expectTrue(t, pq.DebugPageIsWired(wired), "wired")
		expectTrue(t, pq.DebugPageIsPagerBackedDirty(dirty), "pager backed dirty")
		expectReclaimAge(t, pq, clean, 1)
		expectCounts(t, pq, with(reclaimAt(1)))
		expectActiveInactive(t, pq, 1, 0)

		pq.RotateReclaimQueues()
		expectCounts(t, pq, with(reclaimAt(2)))
		expectActiveInactive(t, pq, 0, 1)
		pq.RotateReclaimQueues()
		expectCounts(t, pq, with(reclaimAt(3)))
		expectActiveInactive(t, pq, 0, 1)
		pq.RotateReclaimQueues()
		expectCounts(t, pq, with(reclaimAt(4)))
		pq.RotateReclaimQueues()
		expectCounts(t, pq, with(reclaimAt(5)))
		pq.RotateReclaimQueues()
		expectCounts(t, pq, with(reclaimAt(6)))
		// Zircon accepts the page in the last queue or isolated, depending on
		// whether its LRU thread has run ahead of the next aging. With no
		// thread it is in the last queue.
		pq.RotateReclaimQueues()
		expectCounts(t, pq, with(reclaimAt(7)))

		// The next rotation isolates the page.
		pq.RotateReclaimQueues()
		expectTrue(t, pq.DebugPageIsWired(wired), "wired")
		expectTrue(t, pq.DebugPageIsPagerBackedDirty(dirty), "pager backed dirty")
		expectTrue(t, pq.DebugPageIsReclaimIsolate(clean), "isolate")
		expectCounts(t, pq, with(Counts{ReclaimIsolate: 1}))
		expectActiveInactive(t, pq, 0, 1)

		// Moving the page brings it back to the first queue.
		pq.MoveToReclaim(clean)
		expectTrue(t, pq.DebugPageIsWired(wired), "wired")
		expectTrue(t, pq.DebugPageIsPagerBackedDirty(dirty), "pager backed dirty")
		expectReclaimAge(t, pq, clean, 0)
		expectCounts(t, pq, with(reclaimAt(0)))
		expectActiveInactive(t, pq, 1, 0)

		// Two more rotations.
		pq.RotateReclaimQueues()
		expectCounts(t, pq, with(reclaimAt(1)))
		expectActiveInactive(t, pq, 1, 0)
		pq.RotateReclaimQueues()
		expectCounts(t, pq, with(reclaimAt(2)))
		expectActiveInactive(t, pq, 0, 1)

		pq.Remove(wired)
		pq.Remove(clean)
		pq.Remove(dirty)
		expectCounts(t, pq, Counts{})
	})
}

// Pages in the don't-need queue stay there across a rotation, and one that is
// accessed leaves it for the MRU queue and ages from there. Zircon's
// pq_toggle_dont_need_queue.
func TestAnAccessedDontNeedPageAgesFromTheNewestQueue(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pq.SetActiveRatioMultiplier(0)
		page1 := &queuedPage{id: 1}
		page2 := &queuedPage{id: 2}

		pq.SetReclaim(page1, pagerVmo, 0)
		expectReclaimAge(t, pq, page1, 0)
		expectCounts(t, pq, reclaimAt(0))
		expectActiveInactive(t, pq, 1, 0)
		pq.SetReclaim(page2, pagerVmo, 0)
		expectReclaimAge(t, pq, page2, 0)
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{2}})
		expectActiveInactive(t, pq, 2, 0)

		// Move the pages to the don't-need queue.
		pq.MoveToReclaimDontNeed(page1)
		pq.MoveToReclaimDontNeed(page2)
		expectTrue(t, pq.DebugPageIsReclaimIsolate(page1), "page 1 isolated")
		expectTrue(t, pq.DebugPageIsReclaimIsolate(page2), "page 2 isolated")
		expectCounts(t, pq, Counts{ReclaimIsolate: 2})
		expectActiveInactive(t, pq, 0, 2)

		// A rotation leaves them there.
		pq.RotateReclaimQueues()
		expectTrue(t, pq.DebugPageIsReclaimIsolate(page1), "page 1 isolated")
		expectTrue(t, pq.DebugPageIsReclaimIsolate(page2), "page 2 isolated")
		expectCounts(t, pq, Counts{ReclaimIsolate: 2})
		expectActiveInactive(t, pq, 0, 2)

		// Access page1 and rotate: it is out of the don't-need queue, one
		// queue behind the newest.
		pq.MarkAccessed(page1)
		pq.RotateReclaimQueues()
		expectReclaimAge(t, pq, page1, 1)
		expectTrue(t, pq.DebugPageIsReclaimIsolate(page2), "page 2 isolated")
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{0, 1}, ReclaimIsolate: 1})
		// Two queues are active, so page1 still is.
		expectActiveInactive(t, pq, 1, 1)

		// Another rotation ages it past the active queues.
		pq.RotateReclaimQueues()
		expectReclaimAge(t, pq, page1, 2)
		expectTrue(t, pq.DebugPageIsReclaimIsolate(page2), "page 2 isolated")
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{0, 0, 1}, ReclaimIsolate: 1})
		expectActiveInactive(t, pq, 0, 2)

		pq.Remove(page1)
		pq.Remove(page2)
		expectCounts(t, pq, Counts{})
	})
}

// Of two pages one queue apart, the older is isolated first and peeked
// first. Zircon's pq_multiple_queues_fifo_order.
func TestTheOlderOfTwoQueuesIsPeekedFirst(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pq.SetActiveRatioMultiplier(0)
		oldPage := &queuedPage{id: 1}
		newPage := &queuedPage{id: 2}

		// oldPage is a rotation older than newPage.
		pq.SetReclaim(oldPage, pagerVmo, 0)
		expectReclaimAge(t, pq, oldPage, 0)
		pq.RotateReclaimQueues()
		pq.SetReclaim(newPage, pagerVmo, ps)
		expectReclaimAge(t, pq, newPage, 0)

		// Rotate until newPage is isolated, which isolates oldPage before it.
		rotations := 0
		for !pq.DebugPageIsReclaimIsolate(newPage) && rotations < 20 {
			pq.RotateReclaimQueues()
			rotations++
		}
		if rotations != NumReclaim {
			t.Errorf("newPage was isolated after %d rotations, want %d", rotations, NumReclaim)
		}
		expectTrue(t, pq.DebugPageIsReclaimIsolate(oldPage), "oldPage isolated")
		expectTrue(t, pq.DebugPageIsReclaimIsolate(newPage), "newPage isolated")

		expectPeek(t, pq, oldPage)
		pq.Remove(oldPage)
		expectPeek(t, pq, newPage)
		pq.Remove(newPage)
		expectCounts(t, pq, Counts{})
	})
}

// Of two pages in one queue, the one put there first is peeked first.
// Zircon's pq_single_queue_fifo_order.
func TestTheFirstPageOfAQueueIsPeekedFirst(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		oldPage := &queuedPage{id: 1}
		newPage := &queuedPage{id: 2}

		pq.SetReclaim(oldPage, pagerVmo, 0)
		pq.SetReclaim(newPage, pagerVmo, ps)
		expectReclaimAge(t, pq, oldPage, 0)
		expectReclaimAge(t, pq, newPage, 0)

		// Age the pages until they are in the LRU queue.
		for range NumReclaim - 1 {
			pq.RotateReclaimQueues()
		}
		expectReclaimAge(t, pq, oldPage, NumReclaim-1)
		expectReclaimAge(t, pq, newPage, NumReclaim-1)

		// They are isolated in the order they were queued, so the older is at
		// the head.
		expectPeek(t, pq, oldPage)
		pq.Remove(oldPage)
		expectPeek(t, pq, newPage)
		pq.Remove(newPage)
		expectCounts(t, pq, Counts{})
	})
}

// Of two pages moved to the don't-need queue, the first moved is peeked
// first. Zircon's pq_isolate_dont_need_fifo_order.
func TestTheFirstDontNeedPageIsPeekedFirst(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		oldPage := &queuedPage{id: 1}
		newPage := &queuedPage{id: 2}

		pq.SetReclaim(oldPage, pagerVmo, 0)
		pq.MoveToReclaimDontNeed(oldPage)
		expectTrue(t, pq.DebugPageIsReclaimIsolate(oldPage), "oldPage isolated")
		pq.SetReclaim(newPage, pagerVmo, ps)
		pq.MoveToReclaimDontNeed(newPage)
		expectTrue(t, pq.DebugPageIsReclaimIsolate(newPage), "newPage isolated")

		expectPeek(t, pq, oldPage)
		pq.Remove(oldPage)
		expectPeek(t, pq, newPage)
		pq.Remove(newPage)
		expectCounts(t, pq, Counts{})
	})
}

// A don't-need page is peeked before an aged one, whichever was isolated
// first. Zircon's pq_isolate_queues_priority.
func TestADontNeedPageIsPeekedBeforeAnAgedOne(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pq.SetActiveRatioMultiplier(0)
		aged := &queuedPage{id: 1}
		dontNeed := &queuedPage{id: 2}

		// Rotate until the first page is isolated, in the standard queue.
		pq.SetReclaim(aged, pagerVmo, 0)
		expectReclaimAge(t, pq, aged, 0)
		rotations := 0
		for !pq.DebugPageIsReclaimIsolate(aged) && rotations < 20 {
			pq.RotateReclaimQueues()
			rotations++
		}
		if rotations != NumReclaim {
			t.Errorf("the page was isolated after %d rotations, want %d", rotations, NumReclaim)
		}
		expectTrue(t, pq.DebugPageIsReclaimIsolate(aged), "aged isolated")

		// The second goes to the don't-need queue.
		pq.SetReclaim(dontNeed, pagerVmo, 0)
		pq.MoveToReclaimDontNeed(dontNeed)
		expectTrue(t, pq.DebugPageIsReclaimIsolate(dontNeed), "don't-need isolated")

		// The don't-need queue is peeked before the standard one.
		expectPeek(t, pq, dontNeed)
		pq.Remove(dontNeed)
		expectPeek(t, pq, aged)
		pq.Remove(aged)
		expectCounts(t, pq, Counts{})
	})
}

// Only a page in an isolate queue is reclaimable. Zircon's
// pq_is_page_reclaimable.
func TestOnlyAnIsolatedPageIsReclaimable(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		page := &queuedPage{}

		pq.SetReclaim(page, pagerVmo, 0)
		expectFalse(t, pq.IsPageReclaimable(page), "reclaimable in the MRU queue")

		pq.MoveToReclaimDontNeed(page)
		expectTrue(t, pq.IsPageReclaimable(page), "reclaimable in the don't-need queue")

		pq.MoveToReclaim(page)
		expectFalse(t, pq.IsPageReclaimable(page), "reclaimable back in the MRU queue")

		pq.Remove(page)
		expectCounts(t, pq, Counts{})
	})
}

// What follows is not Zircon's: the aging a fault drives, and the peeks
// that pass over pages the pager's evictor may not take.

func ids(pages []*queuedPage) []int {
	result := make([]int, len(pages))
	for i, p := range pages {
		result[i] = p.id
	}
	return result
}

// peek is one of the filtered peeks, over the test's queues.
type peek func(accept func(*queuedPage) bool) (VmoBacklink[*queuedPage, *testCow], bool)

// expectPeeks peeks again and again, each time refusing every page an earlier
// peek took, and checks the order the pages came in. The page stays where it
// is each time, so this is the order an evictor that passes over each page it
// has tried takes them in.
func expectPeeks(t *testing.T, what string, peek peek, want ...int) {
	t.Helper()
	var got []*queuedPage
	taken := map[*queuedPage]bool{}
	for {
		backlink, ok := peek(func(p *queuedPage) bool { return !taken[p] })
		if !ok {
			break
		}
		if backlink.Cow != pagerVmo {
			t.Errorf("%s gave page %d a backlink to %v, want the pager's object", what, backlink.Page.id, backlink.Cow)
		}
		taken[backlink.Page] = true
		got = append(got, backlink.Page)
	}
	if !slices.Equal(ids(got), want) {
		t.Errorf("%s peeks pages %v, want %v", what, ids(got), want)
	}
}

// reclaimable peeks the isolate queues and every reclaim queue, the active
// ones too, as the pager's evictor does.
func reclaimable(pq *testQueues) peek {
	return func(accept func(*queuedPage) bool) (VmoBacklink[*queuedPage, *testCow], bool) {
		return pq.PeekIsolateWhere(0, accept)
	}
}

func makePages(n int) []*queuedPage {
	pages := make([]*queuedPage, n)
	for i := range pages {
		pages[i] = &queuedPage{id: i + 1}
	}
	return pages
}

// A pager ages the queues on every page a fault makes or marks accessed, so
// its reclaimable pages are peeked in the order a fault last touched them,
// however many generations that spans, the active ones too.
func TestAgingOnEachAccessWalksPagesInTheOrderTheyWereTouched(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pages := makePages(12)
		for i, p := range pages {
			pq.SetReclaim(p, pagerVmo, uint64(i)*ps)
			pq.AgeOnAccess()
		}
		// Touch some of them again, the oldest last.
		for _, i := range []int{4, 9, 2, 0} {
			pq.MarkAccessed(pages[i])
			pq.AgeOnAccess()
		}
		if got := pq.LastAgeReason(); got != AgeReasonAccess {
			t.Errorf("the last aging was for %v, want %v", got, AgeReasonAccess)
		}
		expectPeeks(t, "the reclaim queues", reclaimable(pq), 2, 4, 6, 7, 8, 9, 11, 12, 5, 10, 3, 1)
		// The last page touched was active, so the peeks aged the queues
		// until it was not, and isolated every page.
		if got := pq.LastAgeReason(); got != AgeReasonManual {
			t.Errorf("the last aging was for %v, want %v", got, AgeReasonManual)
		}
		expectCounts(t, pq, Counts{ReclaimIsolate: 12})
	})
}

// A peek takes a page accessed in a generation, which is still in the list of
// an older one, in the generation it was accessed in. Zircon's LRU processing
// moves it to the head of that generation's list, so it comes after a page
// set in that generation before the processing.
func TestTheWalkFindsALazilyMarkedPageInItsGeneration(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pages := makePages(3)
		// Pages 1 and 2 are in one generation; 1 is marked accessed in the
		// next, in which 3 is queued after it.
		pq.SetReclaim(pages[0], pagerVmo, 0)
		pq.SetReclaim(pages[1], pagerVmo, ps)
		pq.RotateReclaimQueues()
		pq.MarkAccessed(pages[0])
		pq.SetReclaim(pages[2], pagerVmo, 2*ps)
		expectPeeks(t, "the reclaim queues", reclaimable(pq), 2, 3, 1)
		expectCounts(t, pq, Counts{ReclaimIsolate: 3})
	})
}

// Aging on access stops while aging is disabled, and a manual rotation does
// not.
func TestDisablingAgingStopsAgingOnAccess(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		page := &queuedPage{}
		pq.SetReclaim(page, pagerVmo, 0)
		pq.DisableAging()
		pq.AgeOnAccess()
		expectReclaimAge(t, pq, page, 0)
		pq.RotateReclaimQueues()
		expectReclaimAge(t, pq, page, 1)
		pq.EnableAging()
		pq.AgeOnAccess()
		expectReclaimAge(t, pq, page, 2)
		if got := pq.LastAgeReason(); got != AgeReasonAccess {
			t.Errorf("the last aging was for %v, want %v", got, AgeReasonAccess)
		}
	})
}

// Disabling or enabling aging twice is a mismatched pair, which panics.
func TestAMismatchedAgingPairPanics(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		for name, mismatch := range map[string]func(*testQueues){
			"enable":  func(pq *testQueues) { pq.EnableAging() },
			"disable": func(pq *testQueues) { pq.DisableAging(); pq.DisableAging() },
		} {
			func() {
				defer func() {
					if got := recover(); got != "zirconvm: mismatched disable/enable pair" {
						t.Errorf("a mismatched %s panicked with %v, want the mismatch", name, got)
					}
				}()
				mismatch(newTestQueues(ps))
			}()
		}
	})
}

// The don't-need, zero-fork and reclaim peeks take what each queue holds, the
// first put there first, and stop at the first page they are let take; the
// walk of every queue visits every page and stops where its body says.
func TestThePeeksTakeTheirQueuesOldestFirst(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pq.EnableAnonymousReclaim(false)
		pages := makePages(6)
		for i, p := range pages {
			pq.SetReclaim(p, pagerVmo, uint64(i)*ps)
		}
		pq.MoveToReclaimDontNeed(pages[3])
		pq.MoveToReclaimDontNeed(pages[1])
		pq.MoveAnonymousToAnonymousZeroFork(pages[4])
		pq.MoveAnonymousToAnonymousZeroFork(pages[0])
		peeks := map[string]peek{
			"the don't-need queue": pq.PeekDontNeedWhere,
			"the zero-fork queue":  pq.PeekAnonymousZeroForkWhere,
			"the reclaim queues":   reclaimable(pq),
		}
		expectPeeks(t, "the don't-need queue", peeks["the don't-need queue"], 4, 2)
		expectPeeks(t, "the zero-fork queue", peeks["the zero-fork queue"], 5, 1)
		expectPeeks(t, "the reclaim queues", peeks["the reclaim queues"], 4, 2, 3, 6)
		var all []int
		for p := range pq.Pages() {
			all = append(all, p.id)
		}
		slices.Sort(all)
		if want := []int{1, 2, 3, 4, 5, 6}; !slices.Equal(all, want) {
			t.Errorf("every queue holds pages %v, want %v", all, want)
		}
		for name, peek := range peeks {
			asked := 0
			if _, ok := peek(func(*queuedPage) bool { asked++; return true }); !ok || asked != 1 {
				t.Errorf("%s asked about %d pages before it took the first, and took one %t; want 1 and true", name, asked, ok)
			}
		}
		visited := 0
		pq.Pages()(func(*queuedPage) bool { visited++; return false })
		if visited != 1 {
			t.Errorf("every queue visited %d pages after its body stopped it, want 1", visited)
		}
		expectCounts(t, pq, Counts{ReclaimIsolate: 4, AnonymousZeroFork: 2})
	})
}

// BenchmarkAPageTouchedAndAged is what the pager pays the queues for each page
// a fault touches: a mark and an aging, over pages that have aged into the
// isolate queue.
func BenchmarkAPageTouchedAndAged(b *testing.B) {
	pq := newTestQueues(4 << 10)
	pages := makePages(1024)
	for i, p := range pages {
		pq.SetReclaim(p, pagerVmo, uint64(i)<<12)
		pq.AgeOnAccess()
	}
	b.ResetTimer()
	for i := range b.N {
		pq.MarkAccessed(pages[i%len(pages)])
		pq.AgeOnAccess()
	}
}

// What follows tests what Gremlins found Zircon's ten cases leave untested.

// isolateAll processes every generation older than the active ones, so the
// LRU generation is one behind the MRU one.
func isolateAll(pq *testQueues) {
	pq.PeekIsolateWhere(NumActiveQueues, func(*queuedPage) bool { return false })
}

// activeQueues are queues with a multiplier of one whose LRU generation is
// one behind the MRU one, and pages set in them, n of them queued.
func activeQueues(ps uint64, n int) (*testQueues, []*queuedPage) {
	pq := newTestQueues(ps)
	pq.SetActiveRatioMultiplier(1)
	isolateAll(pq)
	pages := makePages(n)
	for i, p := range pages {
		pq.SetReclaim(p, pagerVmo, uint64(i)*ps)
	}
	return pq, pages
}

// A peek ages the queues once the active ratio calls for it, which is checked
// once a margin's worth of pages, 2 MiB of them, may have changed queue.
func TestAPeekAgesTheQueuesWhenTheActiveRatioIsTripped(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		margin := int((2 << 20) / ps)
		pq, pages := activeQueues(ps, margin)
		backlink, ok := pq.PeekIsolate(NumActiveQueues)
		expectTrue(t, ok, "a peek found a page")
		if backlink.Page != pages[0] {
			t.Errorf("a peek found %+v, want page 1", backlink)
		}
		if got := pq.LastAgeReason(); got != AgeReasonActiveRatio {
			t.Errorf("the queues last aged for %v, want %v", got, AgeReasonActiveRatio)
		}
		want := Counts{ReclaimIsolate: min(margin, peekIsolateBatch)}
		want.Reclaim[2] = margin - want.ReclaimIsolate
		expectCounts(t, pq, want)
	})
}

// One page short of the margin, the active ratio is not checked, so a peek
// ages nothing and finds nothing.
func TestTheActiveRatioIsNotCheckedShortOfItsMargin(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		margin := int((2 << 20) / ps)
		pq, _ := activeQueues(ps, margin-1)
		backlink, ok := pq.PeekIsolate(NumActiveQueues)
		expectFalse(t, ok, "a peek found a page")
		if backlink.Page != nil {
			t.Errorf("a peek found page %d, want none", backlink.Page.id)
		}
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{margin - 1}})
	})
}

// A peek isolates sixteen pages at a time, not the whole queue.
func TestAPeekIsolatesSixteenPagesAtATime(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pages := makePages(20)
		for i, p := range pages {
			pq.SetReclaim(p, pagerVmo, uint64(i)*ps)
		}
		for range NumReclaim - 1 {
			pq.RotateReclaimQueues()
		}
		expectPeek(t, pq, pages[0])
		want := Counts{ReclaimIsolate: 16}
		want.Reclaim[NumReclaim-1] = 4
		expectCounts(t, pq, want)
	})
}

// The reclaim counts put the two newest generations in newest, the two
// oldest and the isolated pages in oldest, and every page in total.
func TestTheReclaimCountsSplitThePagesByAge(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pages := makePages(9)
		// One page a generation, and then one isolated.
		for i, p := range pages {
			pq.SetReclaim(p, pagerVmo, uint64(i)*ps)
			pq.RotateReclaimQueues()
		}
		// And one in the newest.
		pq.SetReclaim(&queuedPage{id: 10}, pagerVmo, 9*ps)
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{1, 1, 1, 1, 1, 1, 1, 1}, ReclaimIsolate: 2})
		if got, want := pq.GetReclaimQueueCounts(), (ReclaimCounts{Total: 10, Newest: 2, Oldest: 4}); got != want {
			t.Errorf("the reclaim counts are %+v, want %+v", got, want)
		}
		if got := pq.QueueCounts().Total(); got != 10 {
			t.Errorf("the queues hold %d pages, want 10", got)
		}
	})
}

// A count's total is the sum of every queue's.
func TestACountsTotalIsEveryQueue(t *testing.T) {
	counts := Counts{Reclaim: [NumReclaim]int{1, 2, 3, 4, 5, 6, 7, 8}, ReclaimIsolate: 10, PagerBackedDirty: 20,
		Anonymous: 40, Wired: 80, AnonymousZeroFork: 160, FailedReclaim: 320, HighPriority: 640}
	if got := counts.Total(); got != 1306 {
		t.Errorf("the total is %d, want 1306", got)
	}
}

// A page's backlink can be changed, one page or many at a time, and pages can
// be removed many at a time into a list.
func TestBacklinksChangeAndPagesLeaveInBatches(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		other := &testCow{pager: true}
		pages := makePages(3)
		for i, p := range pages {
			pq.SetReclaim(p, pagerVmo, uint64(i)*ps)
			pq.MoveToReclaimDontNeed(p)
		}
		pq.ChangeObjectOffset(pages[0], other, 7*ps)
		pq.ChangeObjectOffsetArray(pages[1:], other, []uint64{8 * ps, 9 * ps})
		for i, p := range pages {
			backlink, ok := pq.PeekIsolate(NumReclaim - 1)
			if !ok || backlink != (VmoBacklink[*queuedPage, *testCow]{Cow: other, Page: p, Offset: uint64(7+i) * ps}) {
				t.Errorf("peek %d found %+v, want page %d of the other object at page %d", i, backlink, p.id, 7+i)
			}
			pq.Remove(p)
			pq.SetReclaim(p, pagerVmo, uint64(i)*ps)
		}
		var out []*queuedPage
		pq.RemoveArrayIntoList(pages[:2], &out)
		if !slices.Equal(ids(out), []int{1, 2}) {
			t.Errorf("the removed pages are %v, want [1 2]", ids(out))
		}
		expectCounts(t, pq, reclaimAt(0))
	})
}

// Where only a pager's pages are reclaimed, an anonymous page moves to the
// zero-fork queue and a reclaim page does not, and a pop takes the zero fork
// back to the anonymous queue. A high priority page is in its own queue.
func TestAZeroForkLeavesAndReturnsToTheAnonymousQueue(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		anonymous, reclaim, high := &queuedPage{id: 1}, &queuedPage{id: 2}, &queuedPage{id: 3}
		pq.SetAnonymous(anonymous, anonymousVmo, 0, false)
		pq.SetReclaim(reclaim, pagerVmo, ps)
		pq.SetHighPriority(high, pagerVmo, 2*ps)
		pq.MoveAnonymousToAnonymousZeroFork(anonymous)
		pq.MoveAnonymousToAnonymousZeroFork(reclaim)
		expectTrue(t, pq.DebugPageIsAnonymousZeroFork(anonymous), "the anonymous page is a zero fork")
		expectFalse(t, pq.DebugPageIsAnonymousZeroFork(reclaim), "the reclaim page is a zero fork")
		expectTrue(t, pq.DebugPageIsHighPriority(high), "high priority")
		expectFalse(t, pq.DebugPageIsHighPriority(reclaim), "the reclaim page is high priority")
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{1}, AnonymousZeroFork: 1, HighPriority: 1})
		backlink, ok := pq.PopAnonymousZeroFork()
		if !ok || backlink.Page != anonymous || backlink.Cow != anonymousVmo || backlink.Offset != 0 {
			t.Errorf("the pop took %+v, want the anonymous page at 0", backlink)
		}
		_, ok = pq.PopAnonymousZeroFork()
		expectFalse(t, ok, "a second pop found a page")
		pq.MoveToHighPriority(reclaim)
		expectCounts(t, pq, Counts{Anonymous: 1, HighPriority: 2})
	})
}

// queueIsValid holds a queue between the LRU and MRU queues, on either side
// of the ring's wrap.
func TestAQueueIsValidBetweenTheLRUAndMRUQueuesAcrossTheWrap(t *testing.T) {
	base := pageQueueReclaimBase
	for _, c := range []struct {
		queue, lru, mru uint8
		want            bool
	}{
		{base + 2, base + 2, base + 5, true},
		{base + 5, base + 2, base + 5, true},
		{base + 1, base + 2, base + 5, false},
		{base + 6, base + 2, base + 5, false},
		{base + 7, base + 6, base + 1, true},
		{base + 1, base + 6, base + 1, true},
		{base + 0, base + 6, base + 1, true},
		{base + 2, base + 6, base + 1, false},
		{base + 5, base + 6, base + 1, false},
	} {
		if got := queueIsValid(c.queue, c.lru, c.mru); got != c.want {
			t.Errorf("queue %d between %d and %d is valid %t, want %t", c.queue-base, c.lru-base, c.mru-base, got, c.want)
		}
	}
}
