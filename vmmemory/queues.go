package vmmemory

import "github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"

// The resident pages are ordered for reclaim by Zircon's page queues
// (internal/zirconvm/pagequeues.go), which take the place of the recency list,
// the idle list and the pins of cold copies the pager kept itself:
//
//   - Every page a memory region may map is in a reclaim queue, or in the
//     standard isolate queue once it has aged out of them. The queues age one
//     generation for each page a fault creates or touches (AgeOnAccess), so an
//     eviction walks them in the order a fault last touched each page, as it
//     walked the recency list. Fault order is the only recency the pager has:
//     it sees no access through a page table it has filled.
//   - An idle page, which no memory region maps, is in the don't-need queue,
//     which Zircon's peek takes first, as the pager took the idle list first.
//     The evictor (evictor.go) takes its victims by peeking these queues.
//   - A page a cold copy will be compared with is in the zero-fork queue. In
//     Zircon that queue holds the pages a write fault copied from the zero page,
//     outside the reclaim queues until the scanner has compared them with zero.
//     Here the comparison's other side is a page of the arena rather than the
//     zero page, and it is that page which must wait outside the reclaim
//     queues, so it is the page queued there; an eviction takes it only when
//     nothing else can go. See cold.go.
//
// A page's queue is decided by whether it is idle and whether it is pinned,
// which are both changed under Host.pinMu, so its moves between the queues are
// made under it too. Host.mu is taken before pinMu, and the queues' own locks
// after both.

// pageQueues are the queues of this pager's resident pages. The object a
// page's backlink names is its file, and its offset is its slot's.
type pageQueues = zirconvm.PageQueues[*resident, *arenaFile]

// newPageQueues are the queues of a pager whose pages are pageSize bytes.
// Every page goes to the reclaim queues, an overlay page as much as a named
// one, so anonymous pages are reclaimable; a page in the zero-fork queue is
// not.
func newPageQueues(pageSize uint64) *pageQueues {
	queues := zirconvm.NewPageQueues[*resident, *arenaFile](pageSize)
	queues.EnableAnonymousReclaim(false)
	return queues
}

// CanEvict reports that every page of the arena can be evicted: a named page
// is dropped and read again, and an overlay page is spilled (departure D2 of
// plans/zircon-pager-port-2026-10-05.md).
func (f *arenaFile) CanEvict() bool { return true }

// dequeueLocked takes a page whose memory is going back out of the queues.
// Caller holds h.mu.
func (h *Host) dequeueLocked(pg *resident) {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	if pg.idle {
		pg.idle = false
		h.idlePages--
	}
	if pg.queued {
		h.queues.Remove(pg)
		pg.queued = false
	}
}

// touch tells the queues a fault touched pg, which makes it the newest page.
// An idle page stays in the don't-need queue: being idle is that no memory
// region maps it, which a touch does not change.
func (h *Host) touch(pg *resident) {
	h.pinMu.Lock()
	marked := pg.queued && !pg.idle
	if marked {
		h.queues.MarkAccessed(pg)
	}
	h.pinMu.Unlock()
	if marked {
		h.queues.AgeOnAccess()
	}
}

// idleLocked makes a page no memory region maps any more idle, at the end of
// the don't-need queue, where it waits for a memory region that inherits its
// identity or for an allocation that needs its slot. Caller holds h.mu.
func (h *Host) idleLocked(pg *resident) {
	// idle is read here under h.mu alone, which its writers also hold.
	if pg.idle || pg.aliases.len() > 0 || pg.slot < 0 {
		return
	}
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	pg.idle = true
	h.idlePages++
	if len(pg.coldCopies) == 0 {
		h.queues.MoveToReclaimDontNeed(pg)
	}
}

// mappedLocked ends a page being idle, which a memory region mapping it again
// does: it goes to the newest reclaim queue. Caller holds h.mu.
func (h *Host) mappedLocked(pg *resident) {
	// idle is read here under h.mu alone, which its writers also hold.
	if !pg.idle {
		return
	}
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	pg.idle = false
	h.idlePages--
	if len(pg.coldCopies) == 0 {
		h.queues.MoveToReclaim(pg)
	}
}

// unpinnedLocked moves a page the last cold copy compared with it has let go
// of back where it belongs: the don't-need queue if it is idle, and the newest
// reclaim queue if not. Caller holds h.pinMu.
func (h *Host) unpinnedLocked(pg *resident) {
	if !pg.queued {
		return
	}
	if pg.idle {
		h.queues.MoveToReclaimDontNeed(pg)
		return
	}
	h.queues.MoveToReclaim(pg)
}
