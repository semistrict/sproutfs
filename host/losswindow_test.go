package host_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A host reports, per VM it runs, how long that VM has held a write no
// checkpoint covers and whether its stores are waiting on the loss window. It is
// the one number that says what losing this host would cost that guest in time,
// and an operator has nowhere else to read it.
func TestAHostReportsEachVMsLossWindow(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = -1
	h.configs[0].LossWindow = 50 * time.Millisecond
	pagers := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1,
		LossWindow: 50 * time.Millisecond})
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	if age, waiting := h.hosts[0].LossWindow("vm-1"); age != 0 || waiting {
		t.Fatalf("a VM holding nothing unpublished reports %s and waiting=%t, want no window at all", age, waiting)
	}
	guest.store("disk", 0, 7)
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

// A fork hands a child the pages its parent has written since the parent's last
// checkpoint, and their age goes with them: the child inherits the parent's loss
// window rather than starting one of its own. Without it a VM forked again every
// few minutes, child after child, would carry the same writes forward for ever
// with none of them ever becoming durable.
func TestAForkHandsTheChildTheParentsLossWindow(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	h.configs[0].CheckpointInterval = -1
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	handoffs, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 || len(handoffs[0].MemoryRegions) != 1 {
		t.Fatalf("the fork handed over %+v, want one memory region of one child", handoffs)
	}
	memoryRegion := handoffs[0].MemoryRegions[0]
	if len(memoryRegion.Unpublished) == 0 {
		t.Fatal("the fork handed the child no unpublished pages to inherit")
	}
	if memoryRegion.UnpublishedAge <= 0 {
		t.Fatal("the fork handed the child unpublished pages with no age, so its window starts again")
	}
}

// A publication that fails while its VM is already past the loss window is not
// given up and taken again at the next interval. The pager is holding the
// guest's stores behind it, and a held store holds its vCPU, so a new pause
// could not be taken at all. The sealed checkpoint is published again instead,
// with no pause, at an eighth of the interval and doubling from there, and it
// lands as soon as the store answers.
func TestTheLoopRepublishesPromptlyWhileTheLossWindowIsExceeded(t *testing.T) {
	const interval = time.Second
	h := newSizedHostHarness(t, 1)
	var unavailable atomic.Bool
	var refused atomic.Int64
	h.configs[0].ObjectStore = &gatedStore{ObjectStore: h.configs[0].ObjectStore,
		blocked: func() error {
			if unavailable.Load() {
				refused.Add(1)
				return platform.ErrUnavailable
			}
			return nil
		}}
	h.configs[0].CheckpointInterval = interval
	h.configs[0].LossWindow = time.Millisecond
	pagers := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	// One write no checkpoint covers, and then nothing this host does can
	// publish it. Nothing stores after this, so the loop's own schedule is the
	// only thing that decides when it tries again.
	guest.store("disk", 0, 7)
	before := vm.Status().Checkpoint
	unavailable.Store(true)
	t.Cleanup(func() { unavailable.Store(false) })
	counting := newCountingMachine(guest)
	if err := h.hosts[0].AddMachine("vm-1", counting); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.hosts[0].RemoveMachine("vm-1") })
	counting.awaitTurns(t, 1)
	// The first attempt, then an eighth of the interval and a quarter: all
	// well inside the interval a loop that waited out its turn would still be
	// waiting.
	began := time.Now()
	deadline := began.Add(10 * time.Second)
	for refused.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the store refused %d requests, want at least three attempts", refused.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(began); elapsed >= interval-interval/8 {
		t.Fatalf("three attempts took %s, which is the interval's own wait rather than a backoff", elapsed)
	}
	unavailable.Store(false)
	for vm.Status().Checkpoint == before {
		if time.Now().After(deadline) {
			t.Fatalf("the held checkpoint never landed once the store answered: %+v", vm.Status())
		}
		time.Sleep(time.Millisecond)
	}
	counting.mu.Lock()
	captures := counting.captures
	counting.mu.Unlock()
	if captures != 1 {
		t.Fatalf("the loop paused the guest %d times, want once: the retries publish what it sealed", captures)
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
	pagers := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("disk", 0, 7)
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

// A guest that dirties a page and then only rewrites it needs no new page, so
// no store of it reaches the pager's window check and nothing asks for a
// checkpoint. The loop's own clock takes one at three quarters of the window
// anyway, long before the interval.
func TestTheLoopCheckpointsAVMNearItsLossWindowOnItsOwnClock(t *testing.T) {
	const window = 400 * time.Millisecond
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = time.Hour
	h.configs[0].LossWindow = window
	pagers := newPagerWithConfig(t, h.configs[0].Resources, vmmemory.Config{
		ResidentPages: 16, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("disk", 0, 7)
	before := vm.Status().Checkpoint
	began := time.Now()
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.hosts[0].RemoveMachine("vm-1") })
	deadline := began.Add(10 * time.Second)
	for vm.Status().Checkpoint == before {
		if time.Now().After(deadline) {
			t.Fatalf("no checkpoint of a VM near its window landed: %+v", vm.Status())
		}
		guest.store("disk", 0, 8)
		time.Sleep(time.Millisecond)
	}
	// The interval is an hour away, so a checkpoint that landed at all was the
	// loop's clock or a request; the pager made none. It came no sooner than
	// half the window, which is not a turn taken at once.
	if elapsed := time.Since(began); elapsed < window/2 {
		t.Fatalf("the checkpoint landed after %s, want no sooner than half the %s window", elapsed, window)
	}
	stats, err := pagers.pagers.Pmem.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.CheckpointRequests != 0 {
		t.Fatalf("the pager asked for %d checkpoints, want the loop's clock alone to take it", stats.CheckpointRequests)
	}
}
