package vmmemory

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
