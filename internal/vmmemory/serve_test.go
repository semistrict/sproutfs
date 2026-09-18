package vmmemory_test

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// servingFixture keeps read-ahead to one page, so what a region holds is
// exactly what its guest touched and a listing can be asserted page by page.
func servingFixture(t *testing.T, resident, logical, dirty int) *fixture {
	t.Helper()
	return newConfiguredFixture(t, vmmemory.Config{ResidentPages: resident, LogicalPages: logical,
		DirtyPages: dirty, ReadAheadPages: 1})
}

// The page server hands a peer the bytes this host holds and tells it plainly
// when it holds none, so the peer reads that page from the volume instead. It
// never loads: a page server that could load would turn a destination's fault
// into a source-side volume read.
func TestReadResidentServesHeldPagesAndNeverLoads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := servingFixture(t, 8, 16, 8)
		r, m, b := f.region(8)
		access(t, r, m, 0, false)        // clean, shared with the volume's lineage
		access(t, r, m, 1, true)[0] = 71 // private, written by the guest
		if got, err := r.Resident(); err != nil || !slices.Equal(got, []uint64{0, 1}) {
			t.Fatalf("Resident lists %v, %v; want the two pages the guest touched", got, err)
		}
		loads := b.loads
		dst := make([]byte, pageSize)
		// The clean page is the checkpoint's own bytes; the private one is this
		// host's state, which no checkpoint holds and the destination must keep
		// dirty so its own next checkpoint publishes it.
		for _, want := range []struct {
			page        uint64
			value       byte
			unpublished bool
		}{{0, 1, false}, {1, 71, true}} {
			ok, unpublished, err := r.ReadResident(t.Context(), want.page, dst)
			if err != nil || !ok || dst[0] != want.value || unpublished != want.unpublished {
				t.Fatalf("ReadResident(%d) = %t, %t, %d, %v; want true, %t and %d",
					want.page, ok, unpublished, dst[0], err, want.unpublished, want.value)
			}
		}
		// A page the guest never touched is the destination's own read.
		for _, page := range []uint64{5, 7} {
			ok, unpublished, err := r.ReadResident(t.Context(), page, dst)
			if err != nil || ok || unpublished {
				t.Fatalf("ReadResident(%d) = %t, %t, %v; want false for a page this host does not hold", page, ok, unpublished, err)
			}
		}
		if b.loads != loads {
			t.Fatalf("the page server read the volume %d times, want none", b.loads-loads)
		}
		if _, _, err := r.ReadResident(t.Context(), 8, dst); !errors.Is(err, vmmemory.ErrRange) {
			t.Fatalf("ReadResident past the region = %v, want ErrRange", err)
		}
		if _, _, err := r.ReadResident(t.Context(), 0, dst[:pageSize-1]); !errors.Is(err, vmmemory.ErrRange) {
			t.Fatalf("ReadResident into a short buffer = %v, want ErrRange", err)
		}
	})
}

// A page this host published is served as the checkpoint's own: the destination
// could have read it from storage, so it need not be dirty there.
func TestReadResidentReportsAPublishedPageAsTheCheckpointsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := servingFixture(t, 8, 16, 8)
		r, m, b := f.region(4)
		access(t, r, m, 0, true)[0] = 71
		dst := make([]byte, pageSize)
		_, unpublished, err := r.ReadResident(t.Context(), 0, dst)
		if err != nil || !unpublished {
			t.Fatalf("a page written since the last checkpoint is reported published: %v", err)
		}
		f.mustCheckpoint(r, b)
		ok, unpublished, err := r.ReadResident(t.Context(), 0, dst)
		if err != nil || !ok || dst[0] != 71 || unpublished {
			t.Fatalf("ReadResident after the checkpoint = %t, %t, %d, %v; want true, false and 71", ok, unpublished, dst[0], err)
		}
	})
}

// A page of a checkpoint is served from the resident page the checkpoint and
// the guest share, which is what a peer must get after the final seal of a
// stopped guest. A guest that did store since the seal owns its own copy, and
// that is what it gets served.
func TestReadResidentServesTheCheckpointsPageAndTheGuestsCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := servingFixture(t, 8, 16, 8)
		r, m, b := f.region(4)
		access(t, r, m, 0, true)[0] = 60
		access(t, r, m, 1, true)[0] = 61
		seal(t, r)
		// Page 1 is stored into after the seal, so the guest copies away from
		// the checkpoint; page 0 still shares the checkpoint's copy.
		value := byte(91)
		if _, err := memoryByte(t.Context(), r, m, 1, &value); err != nil {
			t.Fatal(err)
		}
		dst := make([]byte, pageSize)
		for _, want := range []struct {
			page  uint64
			value byte
		}{{0, 60}, {1, 91}} {
			ok, unpublished, err := r.ReadResident(t.Context(), want.page, dst)
			if err != nil || !ok || dst[0] != want.value || !unpublished {
				t.Fatalf("ReadResident(%d) = %t, %t, %d, %v; want true, true and %d",
					want.page, ok, unpublished, dst[0], err, want.value)
			}
		}
		if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
			t.Fatal(err)
		}
		if err := r.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		// After the checkpoint lands, the page it retired is served from the memory
		// it left the guest, which is the same bytes the volume now holds.
		ok, unpublished, err := r.ReadResident(t.Context(), 0, dst)
		if err != nil || !ok || dst[0] != 60 || unpublished {
			t.Fatalf("ReadResident(0) after the checkpoint = %t, %t, %d, %v; want true, false and 60", ok, unpublished, dst[0], err)
		}
	})
}

// A listing is what a migration acts on: the destination fetches the
// unpublished pages and reads the rest from the volume. A region that cannot
// answer must say so, because the answer it would otherwise give — this host
// holds nothing — is the answer that sends the destination to the volume for
// every page and rewinds the guest to the last checkpoint.
func TestListingARegionThatCannotAnswerReportsTheFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := servingFixture(t, 8, 16, 8)
		r, m, _ := f.region(4)
		access(t, r, m, 0, true)[0] = 71
		pages, err := r.Unpublished()
		if err != nil || !slices.Equal(pages, []uint64{0}) {
			t.Fatalf("Unpublished lists %v, %v; want the one page the guest stored into", pages, err)
		}
		// A failed write protection is a region nothing can reason about any
		// more, which is exactly when what it holds must not read as nothing.
		m.failProtect = true
		if err := r.Seal(t.Context()); !errors.Is(err, errInjected) {
			t.Fatalf("the seal whose protection failed = %v, want the injected failure", err)
		}
		if pages, err := r.Unpublished(); err == nil {
			t.Fatalf("the unpublished set of a failed region = %v, nil; want the failure", pages)
		}
		if pages, err := r.Resident(); err == nil {
			t.Fatalf("the resident set of a failed region = %v, nil; want the failure", pages)
		}
	})
}

// Residency is a snapshot: a page listed for a bulk stream can be reclaimed
// before the stream asks for it, and the answer then is that this host no
// longer holds it, never a read of the volume on the peer's behalf.
func TestReadResidentReportsAPageEvictedSinceItWasListed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := servingFixture(t, 1, 8, 4)
		r, m, b := f.region(2)
		// An unrelated lineage, so this volume's fault takes the only slot
		// rather than sharing the resident page it already holds.
		other, om := f.attach(f.newUnrelatedBacking(2))
		access(t, r, m, 0, false)
		if got, err := r.Resident(); err != nil || !slices.Equal(got, []uint64{0}) {
			t.Fatalf("Resident lists %v, %v; want the one page the guest touched", got, err)
		}
		// The only slot goes to another volume, which evicts that page.
		access(t, other, om, 0, false)
		if got, err := r.Resident(); err != nil || len(got) != 0 {
			t.Fatalf("Resident still lists %v, %v after the page was reclaimed", got, err)
		}
		loads := b.loads
		dst := make([]byte, pageSize)
		ok, _, err := r.ReadResident(t.Context(), 0, dst)
		if err != nil || ok {
			t.Fatalf("ReadResident of an evicted page = %t, %v; want false", ok, err)
		}
		if b.loads != loads {
			t.Fatalf("the page server loaded the evicted page %d times, want none", b.loads-loads)
		}
	})
}

// A migration's stop seals nothing: the volume is handed to another host while
// this region still holds the guest's own dirty pages, and it keeps serving
// them without touching that volume again. Detaching then releases everything it
// kept.
func TestHandedOffRegionKeepsServingItsPagesWithoutItsVolume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := servingFixture(t, 8, 16, 8)
		r, m, b := f.region(4)
		for page := range uint64(2) {
			access(t, r, m, page, true)[0] = byte(60 + page)
		}
		if _, err := r.Handoff(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The volume handle is gone: every use of it now fails, and nothing the
		// region still does may be a use of it.
		b.failRead, b.failVerify = true, true
		if err := r.Verify(t.Context()); err != nil {
			t.Fatalf("verification used a volume this host handed off: %v", err)
		}
		if got, err := r.Resident(); err != nil || !slices.Equal(got, []uint64{0, 1}) {
			t.Fatalf("a handed-off region lists %v, %v; want the pages it still holds", got, err)
		}
		dst := make([]byte, pageSize)
		for page := range uint64(2) {
			ok, unpublished, err := r.ReadResident(t.Context(), page, dst)
			if err != nil || !ok || dst[0] != byte(60+page) || !unpublished {
				t.Fatalf("ReadResident(%d) = %t, %t, %d, %v; want true, true and %d", page, ok, unpublished, dst[0], err, 60+page)
			}
		}
		for name, err := range map[string]error{
			"Seal":  r.Seal(t.Context()),
			"Fault": r.Fault(t.Context(), 3, false),
		} {
			if !errors.Is(err, vmmemory.ErrHandedOff) {
				t.Fatalf("%s on a handed-off region = %v, want ErrHandedOff", name, err)
			}
		}
		clear(m.pages) // the VMM process has exited
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		s, err := f.h.Stats(t.Context())
		if err != nil || s.ResidentPages != 0 || s.DirtyPages != 0 || s.LogicalPages != 0 {
			t.Fatalf("detaching a handed-off region left %+v: %v", s, err)
		}
	})
}

// A region a checkpoint still has sealed is not something to hand off: that
// publication is reading its pages under a volume handle the handoff would give
// away. The region keeps its volume and says why.
func TestHandoffRefusesASealedRegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := servingFixture(t, 8, 16, 8)
		r, m, b := f.region(4)
		access(t, r, m, 0, true)[0] = 60
		seal(t, r)
		if _, err := r.Handoff(t.Context()); !errors.Is(err, vmmemory.ErrSealed) {
			t.Fatalf("Handoff of a sealed region = %v, want ErrSealed", err)
		}
		if err := r.Unseal(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.mustCheckpoint(r, b)
		if b.data[0] != 60 {
			t.Fatalf("the region that kept its volume published %d, want 60", b.data[0])
		}
		if _, err := r.Handoff(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
