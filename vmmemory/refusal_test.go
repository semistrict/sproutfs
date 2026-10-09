package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A client admits a mapping command against its own mapping budget before it
// touches anything and answers a refusal as an ordinary acknowledgement, so a
// refused command is the one failure that is known to have changed nothing. It
// is a failed fault, not a failed memory region: the pages it did not map are not
// recorded as mapped, the memory region goes on serving, and the same fault served
// again once the budget has been freed maps them and completes.
//
// A page recorded as mapped that the client never mapped is resolved with
// nothing behind it, which is why the record has to be taken back — while the
// opposite, a page recorded as mapped that the client did map, is what every
// ambiguous failure must leave behind, because a revocation skips an unmapped
// binding and would release the page the guest still reads through.
func TestARefusedMappingFailsTheFaultAndNotTheMemoryRegion(t *testing.T) {
	for _, mode := range []struct {
		name  string
		write bool
	}{{"read", false}, {"store", true}} {
		t.Run(mode.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, 8, 16, 8)
				r, m, b := f.memoryRegion(4)
				m.refuseMap = true
				maps := m.maps
				if err := r.Fault(t.Context(), 1, mode.write); !errors.Is(err, vmmemory.ErrMappingRefused) {
					t.Fatalf("a fault whose mapping command was refused = %v, want the refusal", err)
				}
				if m.maps != maps {
					t.Fatalf("the refused fault issued %d mapping commands, want none", m.maps-maps)
				}
				if _, ok := m.pages[1]; ok {
					t.Fatal("the refused command mapped the page")
				}
				// The memory region is not terminal: the same fault is served once the
				// budget the client ran out of has been freed.
				m.refuseMap = false
				if err := r.Fault(t.Context(), 1, mode.write); err != nil {
					t.Fatalf("the fault served again after the refusal: %v", err)
				}
				if m.maps == maps {
					t.Fatal("the fault served again issued no mapping command, so the refused pages were still recorded as mapped")
				}
				if got, err := memoryByte(t.Context(), r, m, 1, nil); err != nil || got != 2 {
					t.Fatalf("page 1 reads %d after the refusal, want its inherited 2: %v", got, err)
				}
				// A checkpoint of the memory region still works, which a terminal
				// memory region's would not.
				value := byte(71)
				if _, err := memoryByte(t.Context(), r, m, 1, &value); err != nil {
					t.Fatal(err)
				}
				f.mustCheckpoint(r, b)
				if b.data[pageSize] != 71 {
					t.Fatalf("the checkpoint after the refusal published %d, want 71", b.data[pageSize])
				}
			})
		})
	}
}

// A client refuses a mapping for want of budget, and the budget is its own
// region's mappings, so a session answers a refusal by taking those back
// itself (makeRoom) rather than waiting for the host to revoke something,
// which with no memory pressure never comes. Taking them back keeps every
// page: each one's mapping is revoked, its bytes stay in the arena, and the
// guest's next touch maps it again from its frame with no read, a store it
// made still there.
func TestTakingBackARegionsMappingsKeepsItsPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(4)
		value := byte(71)
		if _, err := memoryByte(t.Context(), r, m, 0, &value); err != nil {
			t.Fatal(err)
		}
		var before [4]byte
		for page := range uint64(4) {
			got, err := memoryByte(t.Context(), r, m, page, nil)
			if err != nil {
				t.Fatal(err)
			}
			before[page] = got
		}
		loads, revokes := b.loads, m.revokes
		taken, err := vmmemory.HarvestOwn(t.Context(), r)
		if err != nil {
			t.Fatalf("taking back the region's mappings: %v", err)
		}
		if taken != 4 {
			t.Fatalf("took back the mappings of %d pages, want the region's 4", taken)
		}
		if len(m.pages) != 0 {
			t.Fatalf("the client still maps %d pages after the region's mappings were taken back, want none", len(m.pages))
		}
		if m.revokes == revokes {
			t.Fatal("taking back the region's mappings sent no revocation")
		}
		for page := range uint64(4) {
			got, err := memoryByte(t.Context(), r, m, page, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != before[page] {
				t.Fatalf("page %d reads %d after its mapping was taken back, want %d", page, got, before[page])
			}
		}
		if before[0] != 71 {
			t.Fatalf("page 0 read %d before its mapping was taken back, want the store's 71", before[0])
		}
		if b.loads != loads {
			t.Fatalf("mapping the pages again read the backing %d times, want none", b.loads-loads)
		}
	})
}

// A revocation the client refused is not the refusal a fault is served again
// for: what such a fault waits for is a revocation, and this is one that could
// not happen. It is terminal like every other failed revocation — the pages
// stay recorded as mapped, which is what keeps the page the guest may still
// read through reachable — and it must not reach the fault worker as a command
// to try again, which would leave the guest waiting on a memory region that is over.
func TestARefusedRevocationIsTerminalRatherThanServedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 1, 8, 4)
		r, m, _ := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 41
		m.refuseRevoke = true
		// The only page is page 0's, so this fault reclaims it, which revokes
		// the guest's mapping of it first.
		err := r.Fault(t.Context(), 1, false)
		if err == nil {
			t.Fatal("the reclaim whose revocation was refused reported success")
		}
		if errors.Is(err, vmmemory.ErrMappingRefused) {
			t.Fatalf("a refused revocation reached the fault as a command to serve again: %v", err)
		}
		if _, ok := m.pages[0]; !ok {
			t.Fatal("the refused revocation took the guest's mapping away anyway")
		}
	})
}

// Write-ahead maps a whole run of fresh zero pages with one command, so a
// refusal leaves a run of pages to take the record back for. They keep their
// pages and their reservations — a store that has a private page and no
// mapping is a page waiting to be mapped, not a page that lost anything — and
// the store that faults again maps the run and completes.
func TestARefusedWriteAheadRunKeepsItsPagesUnmapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, b := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 8,
			DirtyPages: 8, ReadAheadPages: 8, WriteAheadPages: 4}, 8)
		m.refuseMap = true
		if err := r.Fault(t.Context(), 0, true); !errors.Is(err, vmmemory.ErrMappingRefused) {
			t.Fatalf("a store whose write-ahead run was refused = %v, want the refusal", err)
		}
		if len(m.pages) != 0 {
			t.Fatalf("the refused command mapped %d pages, want none", len(m.pages))
		}
		m.refuseMap = false
		value := byte(42)
		if _, err := memoryByte(t.Context(), r, m, 0, &value); err != nil {
			t.Fatalf("the store served again after the refusal: %v", err)
		}
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != 42 {
			t.Fatalf("page 0 reads %d after the refused write-ahead run, want 42: %v", got, err)
		}
		f.mustCheckpoint(r, b)
		if b.data[0] != 42 {
			t.Fatalf("the checkpoint after the refusal published %d, want 42", b.data[0])
		}
	})
}

// A read of a hole maps its window to zero with one command, and the pager
// records the run as mapped before the command is sent: an ambiguous
// acknowledgement must leave it recorded. A refusal changed nothing, so the
// whole run comes out of the record again. That is the faulting page, which
// the fault bound before it planned, and the pages beside it, which have no
// binding. The fault served again maps the run.
func TestARefusedZeroRunIsNotRecordedAsMapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, r, m, _ := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 8,
			DirtyPages: 8, ReadAheadPages: 8}, 8)
		m.refuseMap = true
		maps := m.maps
		if err := r.Fault(t.Context(), 3, false); !errors.Is(err, vmmemory.ErrMappingRefused) {
			t.Fatalf("a read whose zero run was refused = %v, want the refusal", err)
		}
		for _, page := range []uint64{3, 5} {
			if vmmemory.Repeated(r, page, false) {
				t.Fatalf("page %d is recorded as mapped after its zero run was refused", page)
			}
		}
		m.refuseMap = false
		if err := r.Fault(t.Context(), 3, false); err != nil {
			t.Fatalf("the read served again after the refusal: %v", err)
		}
		if m.maps-maps != 1 || len(m.pages) != 8 {
			t.Fatalf("the read served again issued %d commands mapping %d pages, want 1 mapping the window's 8",
				m.maps-maps, len(m.pages))
		}
		for _, page := range []uint64{3, 5} {
			if !vmmemory.Repeated(r, page, false) {
				t.Fatalf("page %d is not recorded as mapped after its zero run was", page)
			}
		}
	})
}

// A fault records every run of its window mapped before it sends their
// commands: the pages it maps read-only and the region's own dirty pages it
// maps writable. A refusal of the read-only command takes back the record of
// the writable runs too, which were never sent. It used to take back only the
// read-only ones, which left a dirty page recorded mapped with nothing behind
// it, and the guest's next store into the page resolved writable over nothing:
// at 4 KiB, where a client refuses for want of VMAs, that was UFFDIO_CONTINUE:
// invalid argument, and the VM was gone.
func TestARefusedReadOnlyRunTakesBackTheWritableRunsOfItsFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB, ResidentPages: 48,
			LogicalPages: 64, DirtyPages: 64, ReadAheadPages: 16})
		r, m, b := f.memoryRegion(64)
		// Page 2 is the region's own dirty page, made from zeros, resident and
		// not mapped, and page 3 is zeros, which a fault maps without reading:
		// one fault on page 2 maps page 3 to zero and page 2 writable, by two
		// commands, the read-only one first.
		b.zero[2], b.zero[3] = true, true
		access(t, r, m, 2, true)[0] = 7
		if harvested, err := vmmemory.HarvestPage(f.ctx, r, 2); err != nil || !harvested {
			t.Fatalf("harvesting page 2 = %t, %v; want it harvested", harvested, err)
		}
		if _, mapped := m.mappedPage(3); mapped {
			t.Fatal("page 3 is mapped before the fault, so the fault would not map it")
		}
		m.refuseMap = true
		if err := r.Fault(f.ctx, 2, false); !errors.Is(err, vmmemory.ErrMappingRefused) {
			t.Fatalf("a fault whose first mapping command was refused = %v, want the refusal", err)
		}
		m.refuseMap = false
		access(t, r, m, 2, true)[0] = 8
		if got := access(t, r, m, 2, false)[0]; got != 8 {
			t.Fatalf("page 2 reads %d after the store, want 8", got)
		}
	})
}

// A store that makes a page the region's own before its writable mapping lands
// takes the old read-only mapping away when the client refuses the new one: a
// protect trap's page, here, which a capture write-protected. The binding is
// writable from the trap on, and a read-only mapping left under it is one the
// next fault resolves writable over, or one an eviction skips if it is
// recorded gone while still there.
func TestARefusedStoreTakesTheOldMappingAway(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, _ := f.pmemRegion(2)
		f.storeAt(r, m, 0, 0, 0xee)
		if _, err := r.Capture(f.ctx, r.Unjournaled()); err != nil {
			t.Fatal(err)
		}
		if p, mapped := m.mappedPage(0); !mapped || p.writable {
			t.Fatalf("page 0 after its capture is %+v (mapped %t), want it mapped read-only", p, mapped)
		}
		m.refuseMap = true
		if err := r.Fault(f.ctx, 0, true); !errors.Is(err, vmmemory.ErrMappingRefused) {
			t.Fatalf("a store whose writable mapping was refused = %v, want the refusal", err)
		}
		m.refuseMap = false
		if p, mapped := m.mappedPage(0); mapped {
			t.Fatalf("page 0 is still mapped as %+v after its store was refused, want the old mapping gone", p)
		}
		f.storeAt(r, m, 0, 1, 0xef)
		if got := access(t, r, m, 0, false)[:2]; got[0] != 0xee || got[1] != 0xef {
			t.Fatalf("page 0 reads %#x, want the 0xee and 0xef the guest stored", got)
		}
	})
}
