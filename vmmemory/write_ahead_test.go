package vmmemory_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// holeMemoryRegion attaches a memory region whose every page is an explicit hole in its
// volume, which is what fresh guest memory is. Read first, a window of it is
// zero-mapped; stored into first, a page of it has never been mapped at all.
func holeMemoryRegion(t *testing.T, cfg vmmemory.Config, pages int) (*fixture, *vmmemory.MemoryRegion, *mapping, *backing) {
	t.Helper()
	f := newConfiguredFixture(t, cfg)
	b := f.newBacking(pages)
	clear(b.data)
	for page := range uint64(pages) {
		b.zero[page] = true
	}
	r, m := f.attach(b)
	return f, r, m, b
}

func hostStats(t *testing.T, f *fixture) vmmemory.Stats {
	t.Helper()
	s, err := f.h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// requireBytes reads every page the way the guest would, faulting where it
// must, and requires exactly the bytes the model holds.
func requireBytes(t *testing.T, r *vmmemory.MemoryRegion, m *mapping, want []byte, pageSize int) {
	t.Helper()
	for page := range len(want) / pageSize {
		got := access(t, r, m, uint64(page), false)
		if !bytes.Equal(got, want[page*pageSize:(page+1)*pageSize]) {
			t.Fatalf("page %d starts %v, want %v", page, got[:4], want[page*pageSize:page*pageSize+4])
		}
	}
}

// A zero page owns no memory and has nothing to fence, so a store into it is
// one mapping command that puts a fresh page where the zeros were, whether a
// read had zero-mapped it or it was never mapped at all. The fresh page already
// reads as zeros, so no byte is copied into it and the volume is not read.
func TestStoreIntoFreshZeroPageIsOneMappingCommand(t *testing.T) {
	for _, zeroMapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("zeroMapped=%t", zeroMapped), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, r, m, b := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 8,
					DirtyPages: 8, ReadAheadPages: 8, WriteAheadPages: 1}, 8)
				if zeroMapped {
					access(t, r, m, 0, false)
					if len(m.pages) != 8 || m.maps != 1 {
						t.Fatalf("one read zero-mapped %d pages with %d commands, want 8 with 1", len(m.pages), m.maps)
					}
				}
				maps, revokes, writes, zeroed := m.maps, m.revokes, f.a.writes, f.a.zeroed
				before := hostStats(t, f)
				access(t, r, m, 3, true)[0] = 42
				after := hostStats(t, f)
				if got := m.maps - maps; got != 1 {
					t.Fatalf("the store issued %d mapping commands, want 1", got)
				}
				if got := m.revokes - revokes; got != 0 {
					t.Fatalf("the store issued %d revokes, want none", got)
				}
				if got := after.Mapping.Count - before.Mapping.Count; got != 1 {
					t.Fatalf("the mapping histogram observed %d commands, want 1", got)
				}
				if got := after.Revoke.Count - before.Revoke.Count; got != 0 {
					t.Fatalf("the revoke histogram observed %d commands, want none", got)
				}
				if got, runs := f.a.writes-writes, f.a.zeroed-zeroed; got != 0 || runs != 1 {
					t.Fatalf("the store wrote %d slots and zeroed %d runs, want 0 and 1", got, runs)
				}
				if b.loads != 0 {
					t.Fatalf("the store read the volume %d times for a hole", b.loads)
				}
				if faults, copies := after.Faults-before.Faults, after.CopyOnWrites-before.CopyOnWrites; faults != 1 || copies != 1 {
					t.Fatalf("the store took %d faults and %d private pages, want 1 and 1", faults, copies)
				}
				if after.WriteAheadPages != 0 || after.DirtyPages != 1 || !m.pages[3].writable {
					t.Fatalf("write-ahead %d, dirty %d, writable %t; want 0, 1, true", after.WriteAheadPages, after.DirtyPages, m.pages[3].writable)
				}
				want := make([]byte, 8*pageSize)
				want[3*pageSize] = 42
				requireBytes(t, r, m, want, pageSize)
				f.mustCheckpoint(r, b)
				if !bytes.Equal(b.data, want) {
					t.Fatal("the checkpoint did not publish exactly the store")
				}
			})
		})
	}
}

// While a store's mapping command is in flight, the page it stores into and
// every page of its run are still zero-mapped: nothing was revoked, so the
// guest reads zeros through them without faulting and without waiting for the
// store. Its store lands on the new page once the command completes.
func TestZeroMappedPageStaysReadableWhileAStoreMapsItsOwnCopy(t *testing.T) {
	for _, ahead := range []int{1, 4} {
		t.Run(fmt.Sprintf("writeAhead=%d", ahead), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, r, m, _ := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 8,
					DirtyPages: 8, ReadAheadPages: 8, WriteAheadPages: ahead}, 8)
				access(t, r, m, 0, false)
				entered, release := make(chan struct{}), make(chan struct{})
				open := sync.OnceFunc(func() { close(release) })
				defer open()
				m.onMap = func(uint64, int) { close(entered); <-release }
				stored := make(chan error, 1)
				go func() { stored <- r.Fault(t.Context(), 3, true) }()
				<-entered
				m.onMap = nil
				read := make(chan error, 1)
				go func() {
					for page := range uint64(8) {
						got, err := memoryByte(t.Context(), r, m, page, nil)
						if err == nil && got != 0 {
							err = fmt.Errorf("page %d read %d while the store was mapping", page, got)
						}
						if err != nil {
							read <- err
							return
						}
					}
					read <- nil
				}()
				synctest.Wait()
				select {
				case err := <-read:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("a read of the zero-mapped run waited for the store's mapping command")
				}
				open()
				if err := <-stored; err != nil {
					t.Fatal(err)
				}
				if m.revokes != 0 {
					t.Fatalf("the store issued %d revokes, want none", m.revokes)
				}
				value := byte(7)
				if _, err := memoryByte(t.Context(), r, m, 3, &value); err != nil {
					t.Fatal(err)
				}
				for page := range uint64(8) {
					want := byte(0)
					if page == 3 {
						want = 7
					}
					if got, err := memoryByte(t.Context(), r, m, page, nil); err != nil || got != want {
						t.Fatalf("page %d reads %d, want %d: %v", page, got, want, err)
					}
				}
			})
		})
	}
}

// A guest writing fresh memory in order, which is what a kernel building page
// metadata does, takes one fault per write-ahead run rather than one per page:
// each fault maps its whole run writable in one command, in consecutive slots,
// and the stores into the rest of the run never fault.
func TestSequentialStoresIntoFreshMemoryTakeOneFaultPerRun(t *testing.T) {
	for _, zeroMapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("zeroMapped=%t", zeroMapped), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const pages, ahead = 8, 2
				f, r, m, b := holeMemoryRegion(t, vmmemory.Config{ResidentPages: pages, LogicalPages: pages,
					DirtyPages: pages, ReadAheadPages: pages, WriteAheadPages: ahead}, pages)
				if zeroMapped {
					access(t, r, m, 0, false)
				}
				maps, revokes := m.maps, m.revokes
				before := hostStats(t, f)
				want := make([]byte, pages*uint64(pageSize))
				for page := range uint64(pages) {
					value := byte(page + 1)
					if _, err := memoryByte(t.Context(), r, m, page, &value); err != nil {
						t.Fatal(err)
					}
					want[int(page)*pageSize] = value
				}
				after := hostStats(t, f)
				const runs = pages / ahead
				if got := after.Faults - before.Faults; got != runs {
					t.Fatalf("%d sequential stores took %d faults, want %d", pages, got, runs)
				}
				if got := m.maps - maps; got != runs {
					t.Fatalf("the stores issued %d mapping commands, want %d", got, runs)
				}
				if m.revokes != revokes {
					t.Fatalf("the stores issued %d revokes, want none", m.revokes-revokes)
				}
				if copies := after.CopyOnWrites - before.CopyOnWrites; copies != runs || after.WriteAheadPages != pages-runs || after.DirtyPages != pages {
					t.Fatalf("private pages %d, write-ahead %d, dirty %d; want %d, %d, %d",
						copies, after.WriteAheadPages, after.DirtyPages, runs, pages-runs, pages)
				}
				if b.loads != 0 {
					t.Fatalf("the stores read the volume %d times for holes", b.loads)
				}
				for page := range uint64(pages) {
					if p := m.pages[page]; p.slot != int(page) || !p.writable {
						t.Fatalf("page %d maps slot %d writable %t, want slot %d writable", page, p.slot, p.writable, page)
					}
				}
				requireBytes(t, r, m, want, pageSize)
				f.mustCheckpoint(r, b)
				if !bytes.Equal(b.data, want) {
					t.Fatal("the checkpoint did not publish the stores")
				}
				if s := hostStats(t, f); s.WriteAheadZeroPages != 0 {
					t.Fatalf("%d write-ahead pages were written back as zeros, want none: every page was stored into", s.WriteAheadZeroPages)
				}
			})
		})
	}
}

// A run goes forward to the end of the faulting page's read-ahead window, then
// back towards its start, and never past either end.
func TestWriteAheadStaysInsideTheReadAheadWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 16,
			DirtyPages: 16, ReadAheadPages: 4, WriteAheadPages: 16}, 16)
		access(t, r, m, 1, true)[0] = 1
		if len(m.pages) != 4 {
			t.Fatalf("a store into page 1 mapped %d pages, want its window of 4", len(m.pages))
		}
		for page := range uint64(4) {
			if !m.pages[page].writable {
				t.Fatalf("page %d of the run is not writable", page)
			}
		}
		if s := hostStats(t, f); s.Faults != 1 || s.WriteAheadPages != 3 {
			t.Fatalf("faults %d, write-ahead %d; want 1 and 3", s.Faults, s.WriteAheadPages)
		}
		access(t, r, m, 6, true)[0] = 6
		if len(m.pages) != 8 {
			t.Fatalf("a store into page 6 left %d pages mapped, want 8", len(m.pages))
		}
		if s := hostStats(t, f); s.Faults != 2 || s.WriteAheadPages != 6 {
			t.Fatalf("faults %d, write-ahead %d; want 2 and 6", s.Faults, s.WriteAheadPages)
		}
	})
}

// Write-ahead takes only free arena slots, exactly as read-ahead does. With the
// arena full, the store evicts for its own page alone.
func TestWriteAheadTakesOnlyFreeArenaSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 16
		f, r, m, b := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 4, LogicalPages: pages,
			DirtyPages: pages, ReadAheadPages: 8, WriteAheadPages: 8}, pages)
		want := make([]byte, pages*uint64(pageSize))
		access(t, r, m, 0, true)[0] = 10
		want[0] = 10
		if s := hostStats(t, f); s.WriteAheadPages != 3 || s.ResidentPages != 4 || s.DirtyPages != 4 || s.Evictions != 0 {
			t.Fatalf("write-ahead %d, resident %d, dirty %d, evictions %d; want 3, 4, 4, 0",
				s.WriteAheadPages, s.ResidentPages, s.DirtyPages, s.Evictions)
		}
		access(t, r, m, 8, true)[0] = 80
		want[8*pageSize] = 80
		// A page is 2 MiB, which is the whole scratch budget of one reclaim, so
		// the store evicts exactly the one victim it needs a slot for.
		s := hostStats(t, f)
		if s.WriteAheadPages != 3 || s.Evictions != 1 || s.Spills != 1 || s.ResidentPages != 4 || s.DirtyPages != 5 || s.PeakResidentPages != 4 {
			t.Fatalf("write-ahead %d, evictions %d, spills %d, resident %d, dirty %d, peak resident %d; want 3, 1, 1, 4, 5, 4",
				s.WriteAheadPages, s.Evictions, s.Spills, s.ResidentPages, s.DirtyPages, s.PeakResidentPages)
		}
		requireBytes(t, r, m, want, pageSize)
		f.mustCheckpoint(r, b)
		if !bytes.Equal(b.data, want) {
			t.Fatal("the checkpoint did not publish the stores and the spilled write-ahead pages")
		}
	})
}

// Write-ahead takes only dirty reservations that are free, never waiting for
// one: a run is as long as the spare budget allows.
func TestWriteAheadTakesOnlySpareDirtyReservations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 16
		f, r, m, b := holeMemoryRegion(t, vmmemory.Config{ResidentPages: pages, LogicalPages: pages,
			DirtyPages: 3, ReadAheadPages: 8, WriteAheadPages: 8}, pages)
		want := make([]byte, pages*uint64(pageSize))
		access(t, r, m, 0, true)[0] = 10
		want[0] = 10
		if s := hostStats(t, f); len(m.pages) != 3 || s.WriteAheadPages != 2 || s.DirtyPages != 3 || s.PeakDirtyPages != 3 {
			t.Fatalf("mapped %d, write-ahead %d, dirty %d, peak dirty %d; want 3, 2, 3, 3",
				len(m.pages), s.WriteAheadPages, s.DirtyPages, s.PeakDirtyPages)
		}
		f.mustCheckpoint(r, b)
		access(t, r, m, 4, true)[0] = 40
		want[4*pageSize] = 40
		for page := uint64(4); page < 7; page++ {
			if !m.pages[page].writable {
				t.Fatalf("page %d of the second run is not writable", page)
			}
		}
		if s := hostStats(t, f); s.WriteAheadPages != 4 || s.DirtyPages != 3 || s.PeakDirtyPages != 3 {
			t.Fatalf("write-ahead %d, dirty %d, peak dirty %d; want 4, 3, 3", s.WriteAheadPages, s.DirtyPages, s.PeakDirtyPages)
		}
		requireBytes(t, r, m, want, pageSize)
		f.mustCheckpoint(r, b)
		if !bytes.Equal(b.data, want) {
			t.Fatal("the checkpoint did not publish the stores")
		}
	})
}

// The pager cannot tell which pages of a run the guest stored into, so a flush
// and a capture's checkpoint write back every one of them, zeros included, and
// the pages whose read bytes were still all zero are counted.
func TestACheckpointPublishesEveryWriteAheadPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 8
		f, r, m, b := holeMemoryRegion(t, vmmemory.Config{ResidentPages: pages, LogicalPages: pages,
			DirtyPages: pages, ReadAheadPages: pages, WriteAheadPages: 4}, pages)
		access(t, r, m, 0, true)[0] = 5
		seal(t, r)
		if got := r.Checkpoint().DirtyPages(); !slices.Equal(got, []uint64{0, 1, 2, 3}) {
			t.Fatalf("the checkpoint holds pages %v, want the whole run 0-3", got)
		}
		if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
			t.Fatal(err)
		}
		if err := r.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		s := hostStats(t, f)
		if s.CheckpointPages != 4 || s.WriteAheadPages != 3 || s.WriteAheadZeroPages != 3 || s.DirtyPages != 0 {
			t.Fatalf("checkpoint %d pages, write-ahead %d, zero write-ahead %d, dirty %d; want 4, 3, 3, 0",
				s.CheckpointPages, s.WriteAheadPages, s.WriteAheadZeroPages, s.DirtyPages)
		}
		access(t, r, m, 4, true)[0] = 6
		access(t, r, m, 5, true)[0] = 7 // stored into through the run's mapping, without a fault
		if s := hostStats(t, f); s.Faults != 2 {
			t.Fatalf("two runs took %d faults, want 2", s.Faults)
		}
		seal(t, r)
		if got := r.Checkpoint().DirtyPages(); !slices.Equal(got, []uint64{4, 5, 6, 7}) {
			t.Fatalf("the checkpoint holds pages %v, want the whole run 4-7", got)
		}
		if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
			t.Fatal(err)
		}
		if err := r.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		s = hostStats(t, f)
		if s.CheckpointPages != 8 || s.WriteAheadPages != 6 || s.WriteAheadZeroPages != 5 {
			t.Fatalf("checkpoint %d pages, write-ahead %d, zero write-ahead %d; want 8, 6, 5",
				s.CheckpointPages, s.WriteAheadPages, s.WriteAheadZeroPages)
		}
		want := make([]byte, pages*uint64(pageSize))
		want[0], want[4*pageSize], want[5*pageSize] = 5, 6, 7
		if !bytes.Equal(b.data, want) {
			t.Fatal("the volume does not hold exactly the stores")
		}
		requireBytes(t, r, m, want, pageSize)
	})
}

// Random stores and reads over a memory region half holes and half data, through an
// arena too small to hold it and flushed now and then, must always read back
// what an independent byte model holds and never exceed a budget.
func TestWriteAheadAgainstIndependentByteModel(t *testing.T) {
	for seed := uint64(0); seed < 4; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const pages, resident, dirty = 16, 4, 16
				f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: resident, LogicalPages: pages,
					DirtyPages: dirty, ReadAheadPages: 8, WriteAheadPages: 4})
				b := f.newBacking(pages)
				for page := range pages / 2 {
					b.zero[uint64(page)] = true
					clear(b.data[page*pageSize : (page+1)*pageSize])
				}
				r, m := f.attach(b)
				expected := bytes.Clone(b.data)
				rng := rand.New(rand.NewPCG(seed, seed+11))
				for step := range 400 {
					page := rng.IntN(pages)
					write := rng.IntN(2) == 0
					data := access(t, r, m, uint64(page), write)
					if write {
						offset, value := rng.IntN(pageSize), byte(rng.Uint32())
						data[offset] = value
						expected[page*pageSize+offset] = value
					}
					if !bytes.Equal(data, expected[page*pageSize:(page+1)*pageSize]) {
						t.Fatalf("step %d: page %d does not match the model", step, page)
					}
					if s := hostStats(t, f); s.DirtyPages > dirty || s.PeakDirtyPages > dirty || s.PeakResidentPages > resident {
						t.Fatalf("step %d: dirty %d, peak dirty %d, peak resident %d over budgets %d and %d",
							step, s.DirtyPages, s.PeakDirtyPages, s.PeakResidentPages, dirty, resident)
					}
					if step%37 == 0 {
						f.mustCheckpoint(r, b)
						if !bytes.Equal(b.data, expected) {
							t.Fatalf("step %d: the published volume does not match the model", step)
						}
					}
				}
			})
		})
	}
}
