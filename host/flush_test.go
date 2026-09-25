package host_test

import (
	"testing"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// flushBound is the bound every host here keeps, on a clock the test moves.
const flushBound = 30 * time.Second

// flushHost is one host running one VM with memory and a disk, its checkpoint
// loop on but an hour from its first turn, so every checkpoint a test sees is
// one a flush asked for. The host and its pagers read one simulated clock,
// because a flush compares the pager's dating of a write against the host's
// now.
func flushHost(t *testing.T) (*hostHarness, *sim.Clock, *volume.VM, *machine) {
	t.Helper()
	h := newSizedHostHarness(t, 1)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
	h.configs[0].Clock = clock
	h.configs[0].EpochInterval = -1
	h.configs[0].CheckpointInterval = time.Hour
	h.configs[0].FlushBound = flushBound
	pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) { cfg.Clock = clock })
	h.configs[0].Pagers = pagers.pagers
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", mixedVolumes)
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
	return h, clock, vm, guest
}

// flush has the guest flush its disk and returns where the answer arrives.
func flush(guest *machine) <-chan error {
	answered := make(chan error, 1)
	guest.memoryRegions["disk"].Flush(func(err error) { answered <- err })
	return answered
}

// A flush of disks holding nothing older than the bound completes at once, and
// takes no checkpoint: the interval is keeping up, and a guest that fsyncs often
// must not turn every fsync into an upload.
func TestAFlushOfFreshDisksCompletesAtOnce(t *testing.T) {
	_, clock, vm, guest := flushHost(t)
	guest.store("disk", 0, 7)
	clock.Advance(flushBound / 2)
	before := vm.Status().Checkpoint
	select {
	case err := <-flush(guest):
		if err != nil {
			t.Fatalf("a flush of fresh disks failed: %v", err)
		}
	default:
		t.Fatal("a flush of disks inside the bound waited")
	}
	if after := vm.Status().Checkpoint; after != before {
		t.Fatalf("a flush of fresh disks took checkpoint %s", after)
	}
	if guest.memoryRegions["disk"].OldestUnpublished().IsZero() {
		t.Fatal("a flush of fresh disks published them")
	}
}

// A flush of disks holding a write older than the bound waits, and the host
// takes the checkpoint it waits for out of the interval's turn: the interval
// here is an hour away, so the checkpoint that answers it is the flush's.
func TestAFlushOfStaleDisksWaitsForTheCheckpointItAsksFor(t *testing.T) {
	_, clock, vm, guest := flushHost(t)
	guest.store("disk", 0, 7)
	clock.Advance(2 * flushBound)
	before := vm.Status().Checkpoint
	select {
	case err := <-flush(guest):
		if err != nil {
			t.Fatalf("the flush failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a flush of stale disks was never answered")
	}
	if after := vm.Status().Checkpoint; after.Sequence <= before.Sequence {
		t.Fatalf("a flush of stale disks was answered at checkpoint %s, the one it found", after)
	}
	if !guest.memoryRegions["disk"].OldestUnpublished().IsZero() {
		t.Fatal("a flush was answered with the disk's write still unpublished")
	}
}

// A guest whose disks cannot be published stops making fsync progress: its
// flush waits through the failed checkpoint, and is answered by the one that
// lands once the store is back.
func TestAFlushWaitsWhileTheDisksCannotBePublished(t *testing.T) {
	h, clock, vm, guest := flushHost(t)
	guest.store("disk", 0, 7)
	clock.Advance(2 * flushBound)
	h.runtime.ObjectStore().Fail()
	answered := flush(guest)
	select {
	case err := <-answered:
		t.Fatalf("a flush was answered while nothing could be published: %v", err)
	default:
	}
	before := vm.Status().Checkpoint
	h.runtime.ObjectStore().Recover()
	// The attempt the flush asked for may have failed already, and then the
	// one that answers it is the interval's.
	clock.Advance(2 * time.Hour)
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the flush failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the flush was never answered once the store was back")
	}
	if after := vm.Status().Checkpoint; after.Sequence <= before.Sequence {
		t.Fatalf("the flush was answered at checkpoint %s, the one it found", after)
	}
}

// A flush is a disk's. RAM is never made durable by the interval, so a flush of
// it is not a request any host can answer.
func TestAFlushOfRAMIsRefused(t *testing.T) {
	_, _, _, guest := flushHost(t)
	answered := make(chan error, 1)
	guest.memoryRegions["ram0"].Flush(func(err error) { answered <- err })
	if err := <-answered; err == nil {
		t.Fatal("a flush of RAM succeeded")
	}
}

// With no bound configured, the bound is twice the checkpoint interval: a write
// one and a half intervals old does not hold a flush back, and one of two and a
// half intervals does.
func TestTheDefaultFlushBoundIsTwoIntervals(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
	h.configs[0].Clock = clock
	h.configs[0].EpochInterval = -1
	h.configs[0].CheckpointInterval = time.Hour
	pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) { cfg.Clock = clock })
	h.configs[0].Pagers = pagers.pagers
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", mixedVolumes)
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
	guest.store("disk", 0, 7)
	// The store fails, so the interval's own checkpoint at the hour lands
	// nothing and the write keeps its age.
	h.runtime.ObjectStore().Fail()
	clock.Advance(90 * time.Minute)
	select {
	case err := <-flush(guest):
		if err != nil {
			t.Fatalf("the flush failed: %v", err)
		}
	default:
		t.Fatal("a flush waited on a write 1.5 intervals old, inside the default bound of 2")
	}
	clock.Advance(time.Hour)
	select {
	case err := <-flush(guest):
		t.Fatalf("a flush of a write 2.5 intervals old was answered while nothing could be published: %v", err)
	default:
	}
	h.runtime.ObjectStore().Recover()
}

// With the bound turned off every flush completes at once, however stale.
func TestAFlushWithNoBoundCompletesAtOnce(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
	h.configs[0].Clock = clock
	h.configs[0].EpochInterval = -1
	h.configs[0].CheckpointInterval = time.Hour
	h.configs[0].FlushBound = -1
	pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) { cfg.Clock = clock })
	h.configs[0].Pagers = pagers.pagers
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", mixedVolumes)
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
	guest.store("disk", 0, 7)
	clock.Advance(10 * host.DefaultFlushBound)
	select {
	case err := <-flush(guest):
		if err != nil {
			t.Fatalf("the flush failed: %v", err)
		}
	default:
		t.Fatal("a flush with the bound turned off waited")
	}
}
