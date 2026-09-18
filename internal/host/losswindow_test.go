package host_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// A host reports, per VM it runs, how long that VM has held a write no
// checkpoint covers and whether its stores are waiting on the loss window. It is
// the one number that says what losing this host would cost that guest in time,
// and an operator has nowhere else to read it.
func TestAHostReportsEachVMsLossWindow(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = -1
	h.configs[0].LossWindow = 50 * time.Millisecond
	pager, arena := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1,
		LossWindow: 50 * time.Millisecond})
	h.configs[0].Pager = pager
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pager, arena, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	if age, waiting := h.hosts[0].LossWindow("vm-1"); age != 0 || waiting {
		t.Fatalf("a VM holding nothing unpublished reports %s and waiting=%t, want no window at all", age, waiting)
	}
	guest.store("ram0", 0, 7)
	age, waiting := h.hosts[0].LossWindow("vm-1")
	if age <= 0 {
		t.Fatal("a VM holding an unpublished write reports no loss window")
	}
	if waiting {
		t.Fatalf("a VM %s inside a %s window reports its stores waiting", age, h.configs[0].LossWindow)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, waiting := h.hosts[0].LossWindow("vm-1"); waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a VM past its loss window never reported its stores waiting")
		}
		time.Sleep(time.Millisecond)
	}
	// The checkpoint is what ends it: the pages are the store's now, so there is
	// no window left to be past.
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if age, waiting := h.hosts[0].LossWindow("vm-1"); age != 0 || waiting {
		t.Fatalf("a checkpointed VM reports %s and waiting=%t, want no window at all", age, waiting)
	}
}

// A publication that fails while its VM is already past the loss window is not
// retried at the next interval: the guest is held back for the whole of that
// wait, so an interval's patience is exactly what it must not spend. The loop
// tries again at an eighth of the interval and doubles from there, so a store
// that comes back publishes at once instead of an interval later.
func TestTheLoopRetriesPromptlyWhileTheLossWindowIsExceeded(t *testing.T) {
	const interval = time.Second
	h := newSizedHostHarness(t, 1)
	var unavailable atomic.Bool
	h.configs[0].ObjectStore = &gatedStore{ObjectStore: h.configs[0].ObjectStore,
		blocked: func() error {
			if unavailable.Load() {
				return platform.ErrUnavailable
			}
			return nil
		}}
	h.configs[0].CheckpointInterval = interval
	h.configs[0].LossWindow = time.Millisecond
	pager, arena := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
	h.configs[0].Pager = pager
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pager, arena, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	// One write no checkpoint covers, and then nothing this host does can
	// publish it. Nothing stores after this, so the loop's own schedule is the
	// only thing that decides when it tries again.
	guest.store("ram0", 0, 7)
	unavailable.Store(true)
	t.Cleanup(func() { unavailable.Store(false) })
	counting := newCountingMachine(guest)
	if err := h.hosts[0].AddMachine("vm-1", counting); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.hosts[0].RemoveMachine("vm-1") })
	counting.awaitTurns(t, 1)
	// An eighth of the interval, then a quarter: both attempts are well inside
	// the interval a loop that waited out its turn would still be waiting.
	began := time.Now()
	counting.awaitTurns(t, 2)
	if elapsed := time.Since(began); elapsed >= interval-interval/8 {
		t.Fatalf("two more attempts took %s, which is the interval's own wait rather than a backoff", elapsed)
	}
}

// A publication that fails while the VM is inside its window is retried at the
// next interval as it always was: nothing is being held back, and retrying
// eight times as often would only multiply what a store outage costs the
// deployment in requests.
func TestTheLoopKeepsItsIntervalWhileTheLossWindowIsNotExceeded(t *testing.T) {
	const interval = 200 * time.Millisecond
	h := newSizedHostHarness(t, 1)
	var unavailable atomic.Bool
	h.configs[0].ObjectStore = &gatedStore{ObjectStore: h.configs[0].ObjectStore,
		blocked: func() error {
			if unavailable.Load() {
				return platform.ErrUnavailable
			}
			return nil
		}}
	h.configs[0].CheckpointInterval = interval
	// Longer than this test: nothing it runs is ever past the window.
	h.configs[0].LossWindow = time.Hour
	pager, arena := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
	h.configs[0].Pager = pager
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pager, arena, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	unavailable.Store(true)
	t.Cleanup(func() { unavailable.Store(false) })
	counting := newCountingMachine(guest)
	if err := h.hosts[0].AddMachine("vm-1", counting); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.hosts[0].RemoveMachine("vm-1") })
	counting.awaitTurns(t, 1)
	began := time.Now()
	counting.awaitTurns(t, 2)
	if elapsed := time.Since(began); elapsed < 2*(interval-interval/8) {
		t.Fatalf("two more attempts took %s, want at least two jittered intervals of %s", elapsed, interval)
	}
}
