package vmmemory_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/vmmemory"
)

// The host lock comes before a memory region's binding map lock. A store that
// applies the rules holds the host lock while it reads a binding, so a scan of
// the bindings that took the map lock first and then waited for the host lock
// would wait for ever on that store, and the store on it. On GCE on 2026-09-26
// a host's status read and a guest's store did exactly that, and every fault,
// every verification and every status request of that pager waited behind
// them.
func TestAScanOfTheBindingsWaitingForTheHostLockHoldsNoBindingLock(t *testing.T) {
	f := newFixture(t, 16, 64, 16)
	r, _, _ := f.memoryRegion(8)
	if err := r.Fault(t.Context(), 0, false); err != nil {
		t.Fatal(err)
	}
	release := vmmemory.HoldHostLock(f.h)
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := r.Stats(t.Context())
		done <- err
	}()
	for !waitingInScan() {
		runtime.Gosched()
	}
	if vmmemory.BindingsHeld(r) {
		t.Fatal("a scan of the bindings waits for the host lock holding the binding map lock")
	}
	held = false
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// waitingInScan reports whether some goroutine waits for a lock inside a scan
// of a memory region's bindings.
func waitingInScan() bool {
	stacks := make([]byte, 1<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	for _, g := range strings.Split(string(stacks), "\n\n") {
		if strings.Contains(g, "[sync.Mutex.Lock") && (strings.Contains(g, "vmmemory.(*MemoryRegion).eachBinding") ||
			strings.Contains(g, "vmmemory.(*zirconRegion).eachBinding")) {
			return true
		}
	}
	return false
}
