package vmmemory_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// peerBacking stands in for a migration destination's peer backing: some pages
// are served by the source out of pages no checkpoint has, and the volume's own
// bytes are wrong for exactly those pages.
type peerBacking struct {
	*backing
	// unpublished names the pages the source holds, and served their bytes.
	unpublished map[uint64]bool
	served      map[uint64]byte
	// selected is the sequence this backing's handoff was taken against: this
	// VM's own checkpoints up to it predate the pages the handoff named. Zero is
	// a fork's child, which has published nothing of its own.
	selected uint64
	// hidden names pages the source serves out of its own dirty pages that the
	// handoff's set did not list, and the bytes it serves for them. The two
	// answers a real peer backing gives come from different places — Locate
	// reports the set the handoff fixed, while a load reports what the source
	// says at the moment it answers — so a page can be located as the volume's
	// and loaded as the source's own, which is what these pages are.
	hidden map[uint64]byte
	loads  int
	// installedMu guards installed, which is every page this backing was told
	// the region went on to hold. A real peer backing stops asking its source
	// for those and lets it stop serving, so a page missing from here is a page
	// that host goes on holding for a destination that already has it.
	installedMu sync.Mutex
	installed   map[uint64]bool
}

var _ vmmemory.UnpublishedInstaller = (*peerBacking)(nil)

func (b *peerBacking) InstalledUnpublished(offset uint64, installed []bool) {
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	if b.installed == nil {
		b.installed = make(map[uint64]bool)
	}
	for index, held := range installed {
		if held {
			b.installed[offset/uint64(b.pageSize)+uint64(index)] = true
		}
	}
}

// holds reports whether this backing was told the region took the page, which
// is what decides whether the source may stop serving it.
func (b *peerBacking) holds(page uint64) bool {
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	return b.installed[page]
}

func (b *peerBacking) LoadUnpublished(ctx context.Context, offset uint64, dst []byte) ([]bool, error) {
	if err := b.backing.Load(ctx, offset, dst); err != nil {
		return nil, err
	}
	b.loads++
	size := uint64(b.pageSize)
	pages := uint64(len(dst)) / size
	result := make([]bool, pages)
	for index := range pages {
		page := offset/size + index
		served, own := b.served[page], b.unpublished[page]
		if hidden, only := b.hidden[page]; only {
			served, own = hidden, true
		}
		if !own {
			continue
		}
		result[index] = true
		clear(dst[index*size : (index+1)*size])
		dst[index*size] = served
	}
	return result, nil
}

// Locate reports the pages the source holds as bytes of this region alone, which
// is what the peer backing does so the pager never resolves them against a
// checkpoint that does not have them.
func (b *peerBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	extents, err := b.backing.Locate(ctx, offset, length)
	if err != nil {
		return nil, err
	}
	size := uint64(b.pageSize)
	var result []control.Extent
	for _, extent := range extents {
		end := extent.Offset + extent.Length
		for cursor := extent.Offset; cursor < end; {
			page := cursor / size
			stop := min(end, (page+1)*size)
			next := control.Extent{Offset: cursor, Length: stop - cursor}
			// A page of the handoff's set is reported as this region's own for
			// exactly as long as the checkpoint the volume names for it
			// predates the handoff: another VM's, or this one's own up to the
			// sequence the handoff selected. Anything this VM publishes after
			// receiving is newer, and stripping that would tell the pager a
			// page it has just published has no object. See
			// vmmigrate.PeerBacking.Locate, whose rule this mirrors.
			ref := extent.Identity.Ref
			if !b.unpublished[page] || !(ref.VM != b.owner || ref.Sequence <= b.selected) {
				next.Identity = extent.Identity
			}
			if n := len(result); n > 0 && result[n-1].Identity == next.Identity && result[n-1].Offset+result[n-1].Length == next.Offset {
				result[n-1].Length += next.Length
			} else {
				result = append(result, next)
			}
			cursor = stop
		}
	}
	return result, nil
}

// A page a backing reports as unpublished is this region's own dirty state: the
// guest may store into it without faulting again, it counts against the dirty
// budget, and the region's next checkpoint publishes it. Anything else stays
// clean.
func TestUnpublishedLoadBecomesDirtyAndReachesTheNextCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		base := f.newBacking(4)
		peer := &peerBacking{backing: base,
			unpublished: map[uint64]bool{1: true, 2: true},
			served:      map[uint64]byte{1: 71, 2: 72}}
		r, m := f.attach(peer)

		// A page the source does not hold is the checkpoint's own and stays clean.
		if got := access(t, r, m, 0, false)[0]; got != 1 {
			t.Fatalf("a published page reads %d, want the checkpoint's 1", got)
		}
		for page, want := range map[uint64]byte{1: 71, 2: 72} {
			if got := access(t, r, m, page, false)[0]; got != want {
				t.Fatalf("page %d reads %d, want the %d the source served", page, got, want)
			}
		}
		s, err := f.h.Stats(t.Context())
		if err != nil || s.DirtyPages != 2 {
			t.Fatalf("the peer-served pages left %d dirty, want 2: %v", s.DirtyPages, err)
		}
		// They are writable already: a store into one takes no further fault.
		faults := s.Faults
		if _, err := memoryByte(t.Context(), r, m, 1, ptr(byte(99))); err != nil {
			t.Fatal(err)
		}
		if s, err := f.h.Stats(t.Context()); err != nil || s.Faults != faults {
			t.Fatalf("a store into a peer-served page took %d extra faults: %v", s.Faults-faults, err)
		}

		// The next checkpoint publishes them, after which they are the volume's.
		f.mustCheckpoint(r, base)
		if base.data[pageSize] != 99 || base.data[2*pageSize] != 72 {
			t.Fatalf("the checkpoint published %d and %d, want 99 and 72", base.data[pageSize], base.data[2*pageSize])
		}
		if s, err := f.h.Stats(t.Context()); err != nil || s.DirtyPages != 0 {
			t.Fatalf("the checkpoint left %d dirty pages: %v", s.DirtyPages, err)
		}
		// Nothing asks the source again for a page the checkpoint published.
		peer.unpublished, peer.served = nil, nil
		for page, want := range map[uint64]byte{1: 99, 2: 72} {
			if got := access(t, r, m, page, false)[0]; got != want {
				t.Fatalf("page %d reads %d after the checkpoint, want %d", page, got, want)
			}
		}
	})
}

// A destination at the dirty bound waits for the budget exactly as a store
// does — host-wide and pressure-aware — and the post-copy read lands once a
// checkpoint releases the reservations. Failing it instead fails the fault,
// which closes the session and kills the guest this host has just received.
func TestUnpublishedLoadBeyondTheDirtyBudgetWaitsForACheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 4, LogicalPages: 8, DirtyPages: 1, ReadAheadPages: 1})
		base := f.newBacking(4)
		peer := &peerBacking{backing: base,
			unpublished: map[uint64]bool{0: true, 1: true},
			served:      map[uint64]byte{0: 61, 1: 62}}
		r, m := f.attach(peer)
		if got := access(t, r, m, 0, false)[0]; got != 61 {
			t.Fatalf("page 0 reads %d, want the 61 the source served", got)
		}
		// The one reservation is page 0's, so only a checkpoint of this region
		// can admit the next peer-served page.
		requested := make(chan *vmmemory.Region, 4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(region *vmmemory.Region) bool {
			select {
			case requested <- region:
			default:
			}
			return true
		}})
		loaded := make(chan error, 1)
		go func() { loaded <- r.Fault(t.Context(), 1, false) }()
		synctest.Wait()
		select {
		case err := <-loaded:
			t.Fatalf("the post-copy read did not wait for the dirty budget: %v", err)
		default:
		}
		select {
		case got := <-requested:
			if got != r {
				t.Fatal("the host asked to checkpoint a region other than the waiting one")
			}
		default:
			t.Fatal("the post-copy read asked for no checkpoint")
		}
		f.mustCheckpoint(r, base)
		if err := <-loaded; err != nil {
			t.Fatalf("the post-copy read failed after the checkpoint released its reservation: %v", err)
		}
		if got := access(t, r, m, 1, false)[0]; got != 62 {
			t.Fatalf("page 1 reads %d, want the 62 the source served", got)
		}
	})
}

// A destination's guest runs while the pages only its source holds are still
// arriving, and its first touch of one of them can be a store rather than a
// read. The store reads that page from the source exactly as a load does and
// binds those bytes here as this region's own dirty state, so the source stops
// holding the only copy of it and has to be told: a backing that hears only
// about loads goes on expecting a page this host already has, and the source it
// speaks for is never allowed to stop serving.
func TestAStoreTakesAPageNoCheckpointHoldsAndReportsItInstalled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		base := f.newBacking(4)
		peer := &peerBacking{backing: base,
			unpublished: map[uint64]bool{1: true, 2: true},
			served:      map[uint64]byte{1: 71, 2: 72}}
		r, m := f.attach(peer)

		// Page 2 arrives the ordinary way, by being read.
		if got := access(t, r, m, 2, false)[0]; got != 72 {
			t.Fatalf("page 2 reads %d, want the 72 the source served", got)
		}
		if !peer.holds(2) {
			t.Fatal("a load of a page only the source holds left the backing still expecting it")
		}

		// Page 1 is never read: the guest's first touch of it is a store, which
		// this region serves by reading the source's bytes and copying the guest
		// away from them.
		if _, err := memoryByte(t.Context(), r, m, 1, ptr(byte(99))); err != nil {
			t.Fatal(err)
		}
		if !peer.holds(1) {
			t.Fatal("a store into a page only the source holds left the backing still expecting it")
		}
		// The store landed on the source's bytes rather than the checkpoint's:
		// the volume holds 2 in every byte of page 1, the source holds 71 and
		// then zeros, and the store replaced the 71.
		page := access(t, r, m, 1, false)
		if page[0] != 99 || page[1] != 0 {
			t.Fatalf("page 1 reads %d then %d after the store, want 99 over the source's zeros", page[0], page[1])
		}
		if s, err := f.h.Stats(t.Context()); err != nil || s.DirtyPages != 2 {
			t.Fatalf("the pages only the source held left %d dirty, want 2: %v", s.DirtyPages, err)
		}
	})
}

func ptr[T any](value T) *T { return &value }

// A store on a migration destination reads its own page and no more, where a
// store over an ordinary volume brings its whole read-ahead window in. The
// difference is the second answer this backing gives: whether the source still
// holds a page is per page and only a load can report it, so a window read
// would be asking for pages it cannot say that about. It stays a page at a
// time until the backing can answer for a run.
func TestAStoreOnAMigrationDestinationReadsItsOwnPageAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 32,
			DirtyPages: 8, ReadAheadPages: 8})
		base := f.newBacking(8)
		peer := &peerBacking{backing: base,
			unpublished: map[uint64]bool{5: true}, served: map[uint64]byte{5: 55}}
		r, m := f.attach(peer)
		if _, err := memoryByte(t.Context(), r, m, 1, ptr(byte(99))); err != nil {
			t.Fatal(err)
		}
		if peer.loads != 1 || base.loadedBytes != pageSize {
			t.Fatalf("a destination's store made %d loads of %d bytes, want one of its own page",
				peer.loads, base.loadedBytes)
		}
		if len(m.pages) != 1 || !m.pages[1].writable {
			t.Fatalf("the store mapped %d pages (%v), want its own alone and writable", len(m.pages), m.pages)
		}
		if got := access(t, r, m, 1, false)[0]; got != 99 {
			t.Fatalf("the page the guest stored into holds %d, want 99", got)
		}
	})
}

// A store into a page this region holds no memory for reads that page in first,
// and puts what it read into the sharing index under the identity the volume
// gives it, so that the copy has an origin and every sibling that inherits the
// identity maps the page instead of reading it again. That is only sound while
// the bytes it read are that identity's bytes.
//
// A post-copy destination gets two answers about where a page's bytes are, and
// they come from different places: the extents report the set the handoff
// fixed, while the load reports what the source said when it answered. A page
// the source serves out of its own dirty pages that the handoff did not list is
// located as the volume's and loaded as the source's own — and the load's
// answer is the one that saw the bytes. Publishing them under the volume's
// identity gives every sibling that inherits it the other machine's private
// memory in place of its own page.
func TestASourceServedPageTheExtentsCallPublishedIsNotSharedUnderItsIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		base := f.newBacking(4)
		// Page 1 is the volume's as far as the handoff said; the source serves
		// it out of its own dirty pages anyway.
		peer := &peerBacking{backing: base, hidden: map[uint64]byte{1: 71}}
		r, m := f.attach(peer)
		sibling, siblingMapping, _ := f.region(4)

		// The store reads the page in, copies away from it and stores.
		access(t, r, m, 1, true)[0] = 99

		// The sibling inherits the same identity for page 1. Its bytes are the
		// volume's — 2 — and never the source's private 71.
		if got := access(t, sibling, siblingMapping, 1, false)[0]; got != 2 {
			t.Fatalf("the sibling reads %d for page 1, want the volume's 2: "+
				"the source's own page was published under the volume's identity", got)
		}
		// And the source is told the destination has the page, or it goes on
		// holding bytes this host has already taken.
		if !peer.holds(1) {
			t.Fatal("the backing was never told this region took the page the source served")
		}
	})
}
