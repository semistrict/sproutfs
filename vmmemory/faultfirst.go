package vmmemory

import (
	"context"
	"errors"

	"github.com/semistrict/sproutfs/platform/sim"
)

// A fault reads its window one of three ways, decided before it plans
// anything (planFault), and plans only what that way maps or reads:
//
//   - Most faults read their page first (readFirst). A fault locates its page
//     alone, takes it — bound to a resident page under its identity, or a slot
//     to read it into — and starts its read on a task of its own. Only then
//     does it locate the rest of what it plans, in one lookup: the
//     faultAround pages from its own on, which it maps where they are
//     resident and never reads, as Zircon's fault does, and the pages its
//     stream has earned to read ahead (readahead.go), which it prefetches
//     behind its page. The read is under way while it plans, so planning
//     costs the fault nothing while it takes less than the read.
//   - A fault with nothing around it in its window and nothing to read ahead,
//     on its window's last page, reads its page alone (readAlone).
//   - A post-copy stream's fault reads its whole window at once with its page
//     (readRun), so it locates the whole window first. Nothing waits on it.
//
// A fault used to plan its whole window, and at 4 KiB a window is 2,048
// pages. Before it read anything, on GCE on 2026-10-04 that planning took a
// dependent 4 KiB fault from the cluster to 3.2 ms against 0.67 ms for the
// page's read alone (docs/measurements/gce-fault-first-2026-10-04.md). Planned
// behind the read, it still took 1.05 ms: locating 2,048 pages and looking
// each up among the resident pages took about 0.75 ms of processor a fault, as
// long as the read (docs/measurements/gce-fault-planning-2026-10-04.md).
// Planning its page alone took the median hop to 0.83 ms, and the faults'
// processor time from 0.41 s to 0.05 s of a chain of 400
// (docs/measurements/gce-random-fault-planning-2026-10-04.md). Zircon's bound
// of 16 pages around a fault is a lookup a 128th of that.

// WorkPlan is the work of planning a window, one unit a page located, which a
// simulation prices (sim.Config.Compute) so that a test sees a fault's
// planning take time.
const WorkPlan = "vmmemory/plan"

// reading is how a fault reads its window.
type reading int

const (
	// readAlone reads the faulting page alone and plans nothing else: a
	// fault with nothing around it and nothing to read ahead.
	readAlone reading = iota
	// readFirst reads the faulting page first, maps what is resident around
	// it and prefetches behind it what its stream has earned.
	readFirst
	// readRun reads the whole window at once with the faulting page: a
	// post-copy stream's fault.
	readRun
)

// runFirst reports a fault that reads its whole run before its page is
// installed: a post-copy stream's, and every fault under the in-tree bug that
// puts the run back in front of the faulting page.
func runFirst(ctx context.Context) bool {
	return streaming(ctx) || sim.Bug(ctx, "pager-read-the-run-first")
}

// provisionalRun is the run of free slots a fault that prefetches took for its
// window before it located it: count slots from at, for the pages from first.
// The faulting page's is reserved; the rest wait for keepProvisional, and a
// plan unlocked before they are settled gives them back.
type provisionalRun struct {
	first, faulting uint64
	at              fileSlot
	count           int
}

// slots is every slot of the run but the faulting page's, with its page.
func (run provisionalRun) slots(yield func(uint64, fileSlot) bool) {
	for k := range run.count {
		if page := run.first + uint64(k); page != run.faulting && !yield(page, run.at.plus(k)) {
			return
		}
	}
}

// errReadAbandoned is what a faulting page's read is cancelled with when the
// fault failed to plan the rest of its window.
var errReadAbandoned = errors.New("vmmemory: the fault reading this page failed before its read landed")
