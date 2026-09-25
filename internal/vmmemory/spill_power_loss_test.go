package vmmemory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// survivingSpill is a spill handle that outlives its device's power loss. The
// simulator invalidates every handle a power loss touched, which is a process
// that died with the machine; a pager whose host came back and found its
// scratch file still on the device would read through exactly this instead.
// Nothing but this test does that, and that is the point: without it the spill
// file's surviving bytes are unreachable and untested.
type survivingSpill struct {
	disk *sim.Disk
	name string
	mu   sync.Mutex
	file platform.File
}

func openSurvivingSpill(t *testing.T, disk *sim.Disk, name string) *survivingSpill {
	t.Helper()
	s := &survivingSpill{disk: disk, name: name}
	if err := s.reopen(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (s *survivingSpill) reopen(ctx context.Context) error {
	file, err := s.disk.Open(ctx, s.name, platform.OpenOptions{Create: true})
	if err != nil {
		return err
	}
	s.file = file
	return nil
}

// retry runs one operation, reopening the file and running it again when the
// power loss has made the handle stale.
func (s *survivingSpill) retry(ctx context.Context, operation func(platform.File) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := operation(s.file)
	if !errors.Is(err, platform.ErrStaleHandle) {
		return err
	}
	if err := s.reopen(ctx); err != nil {
		return err
	}
	return operation(s.file)
}

func (s *survivingSpill) ReadAt(ctx context.Context, dst []byte, offset int64) (int, error) {
	var n int
	err := s.retry(ctx, func(file platform.File) error {
		var err error
		n, err = file.ReadAt(ctx, dst, offset)
		return err
	})
	return n, err
}

func (s *survivingSpill) WriteAt(ctx context.Context, src []byte, offset int64) (int, error) {
	var n int
	err := s.retry(ctx, func(file platform.File) error {
		var err error
		n, err = file.WriteAt(ctx, src, offset)
		return err
	})
	return n, err
}

func (s *survivingSpill) Truncate(ctx context.Context, size int64) error {
	return s.retry(ctx, func(file platform.File) error { return file.Truncate(ctx, size) })
}

func (s *survivingSpill) Sync(ctx context.Context) error {
	return s.retry(ctx, func(file platform.File) error { return file.Sync(ctx) })
}

func (s *survivingSpill) Size(ctx context.Context) (int64, error) {
	var size int64
	err := s.retry(ctx, func(file platform.File) error {
		var err error
		size, err = file.Size(ctx)
		return err
	})
	return size, err
}

func (s *survivingSpill) PunchHole(ctx context.Context, offset, length int64) error {
	return s.retry(ctx, func(file platform.File) error {
		return file.(platform.SparseFile).PunchHole(ctx, offset, length)
	})
}

func (s *survivingSpill) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

var _ platform.SparseFile = (*survivingSpill)(nil)

// spillFixture is a one-memory-region pager whose scratch spill lives on a device that
// resolves unsynced writes at a power loss.
func spillFixture(t *testing.T, seed uint64) (*fixture, *vmmemory.MemoryRegion, *mapping, *survivingSpill) {
	t.Helper()
	disk := sim.New(sim.Config{Seed: seed}).NewDisk("pager", sim.DiskConfig{PowerLossFaults: true})
	spill := openSurvivingSpill(t, disk, "spill")
	a := newArena(pageSize, 2)
	cfg := vmmemory.Config{PageSize: uint64(pageSize), ResidentPages: 2, LogicalPages: 4, DirtyPages: 2}
	h, err := vmmemory.New(t.Context(), testresource.New(), cfg, a, spill)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// The pager never syncs its scratch file, so without this the power loss
	// would take the whole file rather than part of it. A host that has been
	// running for a while has a spill file the device knows about.
	if err := spill.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, h: h, a: a, disk: disk, pageSize: pageSize,
		source: control.Ref{VM: t.Name(), Sequence: 1}}
	r, m, _ := f.memoryRegion(3)
	return f, r, m, spill
}

// A page the guest stored into lives only in the spill file until a checkpoint
// publishes it. A power loss that drops or garbles the sector holding it must
// not turn into memory the guest never wrote: the page is refused instead.
//
// Without the reservation checksum this test hands the guest zeroes for the
// sectors the device lost and random bytes for the ones it garbled, under
// nearly every seed, with nothing reporting anything wrong.
func TestSpilledPageLostToAPowerLossIsRefusedRatherThanServed(t *testing.T) {
	refused, served := 0, 0
	for seed := uint64(1); seed <= 32; seed++ {
		synctest.Test(t, func(t *testing.T) {
			f, r, m, _ := spillFixture(t, seed)
			// Page zero is private and dirty; faulting the other two evicts it,
			// so its only copy is the reservation it was spilled to.
			access(t, r, m, 0, true)[0] = 77
			access(t, r, m, 1, false)
			access(t, r, m, 2, false)
			if stats, err := f.h.Stats(t.Context()); err != nil || stats.Spills == 0 {
				t.Fatalf("the dirty page was never spilled: %+v %v", stats, err)
			}
			if err := f.disk.PowerLoss(t.Context()); err != nil {
				t.Fatal(err)
			}
			err := r.Fault(t.Context(), 0, false)
			if err == nil {
				if got := m.arena.slots[m.pages[0].slot][0]; got != 77 {
					t.Fatalf("seed %d served byte %d as the guest's own store of 77", seed, got)
				}
				served++
				return
			}
			if !errors.Is(err, vmmemory.ErrSpillCorrupt) {
				t.Fatalf("seed %d refused the page with %v, want ErrSpillCorrupt", seed, err)
			}
			refused++
		})
	}
	if refused == 0 {
		t.Fatal("no seed lost a spilled page; the power loss resolved nothing")
	}
	t.Logf("%d of 32 seeds lost the spilled page and refused it; %d kept it", refused, served)
}

// The checksum must not refuse a page the device kept. A spill file that is
// synced before the power loss comes back whole, and every page reads back.
func TestSpilledPageThatSurvivesAPowerLossStillReadsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, spill := spillFixture(t, 11)
		access(t, r, m, 0, true)[0] = 77
		access(t, r, m, 1, false)
		access(t, r, m, 2, false)
		// The device is told about the spilled page before it loses power, so
		// nothing about it is still pending.
		if err := spill.Sync(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := f.disk.PowerLoss(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := r.Fault(t.Context(), 0, false); err != nil {
			t.Fatalf("a spilled page the device kept was refused: %v", err)
		}
		if got := m.arena.slots[m.pages[0].slot][0]; got != 77 {
			t.Fatalf("read back %d, want the guest's own store of 77", got)
		}
	})
}

// A reservation the pager reuses is checked against the bytes now in it, not
// against the page that used to be there.
func TestSpillChecksumFollowsTheReservationItIsReused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := spillFixture(t, 3)
		for _, value := range []byte{11, 22} {
			access(t, r, m, 0, true)[0] = value
			access(t, r, m, 1, false)
			access(t, r, m, 2, false)
			if err := r.Fault(t.Context(), 0, false); err != nil {
				t.Fatal(err)
			}
			if got := m.arena.slots[m.pages[0].slot][0]; got != value {
				t.Fatalf("read back %d after storing %d", got, value)
			}
		}
		if stats, err := f.h.Stats(t.Context()); err != nil || stats.Spills < 2 {
			t.Fatalf("the page was spilled %v times: %v", stats.Spills, err)
		}
	})
}
