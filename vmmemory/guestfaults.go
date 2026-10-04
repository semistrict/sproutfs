package vmmemory

import (
	"sync/atomic"
	"time"
)

// GuestFaults is what the guest's faults on one memory region have cost since
// it attached. Only faults the VMM reported count: a populate, a prefetch and a
// post-copy stream load pages the guest did not wait for. Waited is from when
// the pager read the fault to when it was served, so it includes the fault's
// time in the queue.
type GuestFaults struct {
	Count  int64
	Waited time.Duration
	// First is when the pager read the first fault, and FirstWaited how long
	// the guest waited for it. First is zero until a fault has been served.
	First       time.Time
	FirstWaited time.Duration
}

// faultTally counts one memory region's guest faults. It is safe for
// concurrent use, because a session serves its faults on several workers.
type faultTally struct {
	count    atomic.Int64
	waitedNS atomic.Int64
	first    atomic.Pointer[firstFault]
}

type firstFault struct {
	at     time.Time
	waited time.Duration
}

// observe counts one served fault, read at at, which the guest waited for
// for waited. The first fault is the one read first, so of two served at once
// the earlier read is kept.
func (t *faultTally) observe(at time.Time, waited time.Duration) {
	t.count.Add(1)
	t.waitedNS.Add(int64(waited))
	fault := &firstFault{at: at, waited: waited}
	for {
		held := t.first.Load()
		if held != nil && !at.Before(held.at) {
			return
		}
		if t.first.CompareAndSwap(held, fault) {
			return
		}
	}
}

func (t *faultTally) report() GuestFaults {
	faults := GuestFaults{Count: t.count.Load(), Waited: time.Duration(t.waitedNS.Load())}
	if first := t.first.Load(); first != nil {
		faults.First, faults.FirstWaited = first.at, first.waited
	}
	return faults
}

// GuestFaults reports the guest's faults on this memory region so far.
func (r *MemoryRegion) GuestFaults() GuestFaults { return r.guestFaults.report() }
