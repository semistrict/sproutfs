package checkpoint_test

import (
	"context"
	"io"
	"math/rand/v2"
	"runtime"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// partsBound is the bytes of part bodies a publication may hold at once:
// one body per upload slot, each a member that took its part over the size it
// seals at, plus the part's table and trailer. Nothing in it scales with the
// number of pages published.
func partsBound(concurrency, partBytes int) int64 {
	return int64(concurrency) * int64(partBytes+checkpoint.PageSize2MiB+64<<10)
}

// A publication uploads its parts as they fill rather than at the end, so the
// bodies it holds at once are the ones its upload slots admit, whatever the
// size of the dirty set.
func TestPublicationHoldsAtMostItsSlotsWorthOfPackBytes(t *testing.T) {
	const (
		pages       = 16
		concurrency = 3
		partBytes   = 1 // Every page fills a part, so the checkpoint has one per page.
	)
	synctest.Test(t, func(t *testing.T) {
		objects := &concurrencyStore{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, Concurrency: concurrency, PartBytes: partBytes})
		sizes := map[string]uint64{"root": pages * checkpoint.PageSize2MiB}
		root, err := store.Root(t.Context(), control.Ref{VM: "streamed", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		// Incompressible pages, so a member is the size of the page it holds and
		// the bodies in flight are the bytes the bound is about.
		source := noiseSource{}
		publication := store.Begin(root, control.Ref{VM: "streamed", Sequence: 2})
		for page := range uint64(pages) {
			publication.Dirty("root", page)
		}
		objects.peak.Store(0)
		objects.peakBytes.Store(0)
		objects.parts.Store(0)
		index, err := publication.Commit(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}

		// One part per page: the segment that locates them is in the index
		// object rather than in a part of its own.
		if got, want := objects.parts.Load(), int64(pages); got != want {
			t.Fatalf("uploaded %d parts, want %d", got, want)
		}
		if got := objects.peak.Load(); got != concurrency {
			t.Fatalf("held %d part bodies at once, want %d", got, concurrency)
		}
		if got, bound := objects.peakBytes.Load(), partsBound(concurrency, partBytes); got > bound {
			t.Fatalf("held %d bytes of part bodies at once, want at most %d", got, bound)
		}
		// The streamed parts are the parts a reader expects.
		page, want := make([]byte, checkpoint.PageSize2MiB), make([]byte, checkpoint.PageSize2MiB)
		for _, number := range []uint64{0, pages - 1} {
			if err := store.Read(t.Context(), index, "root", number*checkpoint.PageSize2MiB, page); err != nil {
				t.Fatal(err)
			}
			if err := source.ReadPage(t.Context(), "root", number, want); err != nil {
				t.Fatal(err)
			}
			if string(page) != string(want) {
				t.Fatalf("page %d reads back as other bytes than it was published with", number)
			}
		}
	})
}

// Compaction writes the pages it rescues through the same parts as the edits,
// so rewriting a checkpoint costs the same bounded memory as publishing one.
func TestCompactionStreamsItsRewrites(t *testing.T) {
	const (
		pages       = 16
		overwritten = 9 // Enough that the first checkpoint's parts are mostly dead.
		concurrency = 3
		partBytes   = 1
	)
	synctest.Test(t, func(t *testing.T) {
		objects := &concurrencyStore{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, Concurrency: concurrency, PartBytes: partBytes})
		sizes := map[string]uint64{"root": pages * checkpoint.PageSize2MiB}
		root, err := store.Root(t.Context(), control.Ref{VM: "compacted", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		first := store.Begin(root, control.Ref{VM: "compacted", Sequence: 2})
		for page := range uint64(pages) {
			first.Dirty("root", page)
		}
		original := noiseSource{}
		parent, err := first.Commit(t.Context(), original)
		if err != nil {
			t.Fatal(err)
		}

		second := store.Begin(parent, control.Ref{VM: "compacted", Sequence: 3})
		for page := range uint64(overwritten) {
			second.Dirty("root", page)
		}
		rewritten := offsetSource{delta: pages}
		objects.peak.Store(0)
		objects.peakBytes.Store(0)
		objects.parts.Store(0)
		index, err := second.Commit(t.Context(), rewritten)
		if err != nil {
			t.Fatal(err)
		}

		// Every page of the first checkpoint's parts is in the second's now:
		// the ones it overwrote, and the ones compaction rescued.
		if got, want := objects.parts.Load(), int64(pages); got != want {
			t.Fatalf("uploaded %d parts, want %d", got, want)
		}
		if got := objects.peak.Load(); got != concurrency {
			t.Fatalf("held %d part bodies at once, want %d", got, concurrency)
		}
		if got, bound := objects.peakBytes.Load(), partsBound(concurrency, partBytes); got > bound {
			t.Fatalf("held %d bytes of part bodies at once, want at most %d", got, bound)
		}
		page, want := make([]byte, checkpoint.PageSize2MiB), make([]byte, checkpoint.PageSize2MiB)
		for number, source := range map[uint64]checkpoint.Source{0: rewritten, pages - 1: original} {
			if err := store.Read(t.Context(), index, "root", number*checkpoint.PageSize2MiB, page); err != nil {
				t.Fatal(err)
			}
			if err := source.ReadPage(t.Context(), "root", number, want); err != nil {
				t.Fatal(err)
			}
			if string(page) != string(want) {
				t.Fatalf("page %d reads back as other bytes than it was published with", number)
			}
		}
	})
}

// offsetSource is another volume's worth of pages, so a test can overwrite a
// page with contents nothing else produces.
type offsetSource struct{ delta uint64 }

func (s offsetSource) ReadPage(ctx context.Context, volume string, page uint64, dst []byte) error {
	return noiseSource{}.ReadPage(ctx, volume, page+s.delta, dst)
}

// noiseSource generates a volume's pages instead of holding them, so a test can
// publish more bytes than it is allowed to keep in memory. Its pages are
// incompressible and deterministic: the same page always reads the same bytes.
type noiseSource struct{}

func (noiseSource) ReadPage(_ context.Context, volume string, page uint64, dst []byte) error {
	var seed [32]byte
	copy(seed[:], volume)
	for index := range 8 {
		seed[24+index] = byte(page >> (8 * index))
	}
	_, err := io.ReadFull(rand.NewChaCha8(seed), dst)
	return err
}

// sinkStore accepts and discards every object, so a publication of many pages
// costs the test's heap nothing beyond what the publication itself holds.
type sinkStore struct{}

func (sinkStore) Head(context.Context, platform.ObjectKey) (platform.ObjectMetadata, error) {
	return platform.ObjectMetadata{}, platform.ErrNotFound
}

func (sinkStore) Get(context.Context, platform.GetRequest) (platform.GetResult, error) {
	return platform.GetResult{}, platform.ErrNotFound
}

func (sinkStore) Put(_ context.Context, request platform.PutRequest) (platform.PutResult, error) {
	return platform.PutResult{Metadata: platform.ObjectMetadata{
		Key: request.Key, Size: request.Size, ETag: "sunk"}}, nil
}

func (sinkStore) Delete(context.Context, platform.DeleteRequest) error { return nil }

func (sinkStore) List(context.Context, platform.ListRequest) (platform.ListResult, error) {
	return platform.ListResult{}, nil
}

// A publication's heap is the parts it has in flight, whatever the size of the
// dirty set it is publishing.
func TestPublicationHeapIsBoundedByPartSizeNotDirtySet(t *testing.T) {
	const (
		partBytes   = 4 << 20
		concurrency = 4
		// A publication holds the parts it has in flight, the one it is filling
		// and the page it is reading. The rest of the bound is the codec
		// workspaces the first page allocates once for the process.
		bound = 6 * partBytes
	)
	store := mustStore(t, checkpoint.Config{ObjectStore: sinkStore{}, Concurrency: concurrency, PartBytes: partBytes})
	for _, pages := range []uint64{32, 128} {
		sizes := map[string]uint64{"root": pages * checkpoint.PageSize2MiB}
		root, err := store.Root(t.Context(), control.Ref{VM: "bounded", Sequence: pages}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		publication := store.Begin(root, control.Ref{VM: "bounded", Sequence: pages + 1})
		for page := range pages {
			publication.Dirty("root", page)
		}
		source := &sampledSource{}
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := publication.Commit(t.Context(), source); err != nil {
			t.Fatal(err)
		}
		if source.samples != int(pages) {
			t.Fatalf("sampled the heap %d times for %d pages", source.samples, pages)
		}
		growth := int64(source.peak) - int64(before.HeapAlloc)
		t.Logf("%d pages (%d MiB) grew the heap by %d MiB at its peak",
			pages, pages*checkpoint.PageSize2MiB>>20, growth>>20)
		if growth > bound {
			t.Fatalf("publishing %d pages grew the heap by %d bytes, want at most %d", pages, growth, bound)
		}
	}
}

// sampledSource measures the heap the publication is holding as it asks for
// each page, which is where a publication that keeps what it has read shows the
// growth. It collects first, so what it reports is what the publication still
// holds rather than what the collector has not got to yet.
type sampledSource struct {
	inner   noiseSource
	peak    uint64
	samples int
}

func (s *sampledSource) ReadPage(ctx context.Context, volume string, page uint64, dst []byte) error {
	var stats runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&stats)
	s.peak, s.samples = max(s.peak, stats.HeapAlloc), s.samples+1
	return s.inner.ReadPage(ctx, volume, page, dst)
}
