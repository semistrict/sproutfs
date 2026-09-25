package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

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
