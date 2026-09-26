package vmmemory

import (
	"sync"
	"time"
)

// A repeated fault is a fault on a page the memory region already maps for the
// access that trapped: zero-mapped or mapped for a read, or mapped writable for
// a store. Serving it changes nothing. The pager installed that mapping's page
// tables when it mapped the page, so all it can do is install them again.
// Something outside the pager took them away: the kernel moving the page, or the
// VMM dropping its own page tables.
//
// A guest meets a repeated fault now and then. A VMM can make them as fast as it
// can drop its page tables, and each one costs the pager a fault's work. Every
// other fault loads a page, maps it or copies it, and the resident, dirty and
// mapping budgets bound those. A guest under memory pressure faults again
// through loads, so its faults are not repeated.
//
// So each session's repeated faults are paced. A memory region may take
// repeatBurst of them at once, and one per repeatInterval after that. A fault
// past its budget waits in its session's fault worker before it is served. The
// wait costs the pager no work and holds up no other session.
const (
	// repeatBurst covers what a guest meets in one go: the pages the kernel
	// moves at once. Compaction moves a 2 MiB block at a time, which is 512
	// pages at 4 KiB.
	repeatBurst = 1024
	// repeatInterval holds a session to 1,024 repeated faults a second. At
	// the few tens of microseconds each one costs the pager, that is a few
	// percent of one processor.
	repeatInterval = time.Second / 1024
)

// repeatBudget paces one memory region's repeated faults.
type repeatBudget struct {
	mu sync.Mutex
	// spent is when the repeated faults given so far are paid for, at one per
	// repeatInterval. It is never more than repeatBurst intervals behind the
	// present, so an idle budget holds at most repeatBurst faults.
	spent time.Time
}

// spend takes one repeated fault from the budget at now. It reports how long
// that fault waits before it is served: zero while the budget holds one.
func (b *repeatBudget) spend(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if full := now.Add(-repeatBurst * repeatInterval); b.spent.Before(full) {
		b.spent = full
	}
	b.spent = b.spent.Add(repeatInterval)
	return max(b.spent.Sub(now), 0)
}

// paceRepeat charges one repeated fault to this memory region's budget and
// reports how long the fault waits before it is served.
func (r *MemoryRegion) paceRepeat() time.Duration {
	h := r.host
	wait := r.repeats.spend(h.clock.Now())
	h.mu.Lock()
	h.stats.RepeatedFaults++
	if wait > 0 {
		h.stats.PacedFaults++
	}
	h.mu.Unlock()
	return wait
}
