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

// What follows is not Zircon's: the aging a fault drives, and the walks the
// pager's victim loop makes until the evictor is ported.

func ids(pages []*queuedPage) []int {
	result := make([]int, len(pages))
	for i, p := range pages {
		result[i] = p.id
	}
	return result
}

func expectWalk(t *testing.T, what string, walk func(func(*queuedPage) bool), want ...int) {
	t.Helper()
	var got []*queuedPage
	walk(func(p *queuedPage) bool {
		got = append(got, p)
		return true
	})
	if !slices.Equal(ids(got), want) {
		t.Errorf("%s walks pages %v, want %v", what, ids(got), want)
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
// its reclaimable pages are walked in the order a fault last touched them,
// however many generations that spans.
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
		expectWalk(t, "the reclaim queues", pq.Reclaimable(), 2, 4, 6, 7, 8, 9, 11, 12, 5, 10, 3, 1)
		if got := pq.LastAgeReason(); got != AgeReasonAccess {
			t.Errorf("the last aging was for %v, want %v", got, AgeReasonAccess)
		}
		// The walk isolated every generation but the two active ones: the
		// last page touched, and the empty generation after it.
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{0, 1}, ReclaimIsolate: 11})
	})
}

// The walk visits a page accessed in a generation, which is still in the list
// of an older one, in the generation it was accessed in.
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
		expectWalk(t, "the reclaim queues", pq.Reclaimable(), 2, 1, 3)
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{2, 1}})
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

// The don't-need, zero-fork and whole-queue walks visit what each holds, the
// first put there first, and stop where their body says.
func TestTheWalksVisitTheirQueuesOldestFirst(t *testing.T) {
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
		expectWalk(t, "the don't-need queue", pq.DontNeed(), 4, 2)
		expectWalk(t, "the zero-fork queue", pq.AnonymousZeroFork(), 5, 1)
		expectWalk(t, "the reclaim queues", pq.Reclaimable(), 4, 2, 3, 6)
		var all []int
		for p := range pq.Pages() {
			all = append(all, p.id)
		}
		slices.Sort(all)
		if want := []int{1, 2, 3, 4, 5, 6}; !slices.Equal(all, want) {
			t.Errorf("every queue holds pages %v, want %v", all, want)
		}
		for name, walk := range map[string]func(func(*queuedPage) bool){
			"the don't-need queue": pq.DontNeed(), "the zero-fork queue": pq.AnonymousZeroFork(),
			"the reclaim queues": pq.Reclaimable(), "every queue": pq.Pages(),
		} {
			visited := 0
			walk(func(*queuedPage) bool { visited++; return false })
			if visited != 1 {
				t.Errorf("%s visited %d pages after its body stopped it, want 1", name, visited)
			}
		}
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{2}, ReclaimIsolate: 2, AnonymousZeroFork: 2})
	})
}
