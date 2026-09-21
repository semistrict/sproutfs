package checkpoint

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/internal/checkpoint/internal/part"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/testresource"
)

// errReaderDisagreed is what one of several concurrent readers reports when the
// bytes it was served are not the ones that were published.
var errReaderDisagreed = errors.New("a reader read bytes that were never published")

// What one run of pages costs in requests is what these measure. A RAM pager
// asks its volume for a 2 MiB run of 4 KiB pages in one Load, and until the
// reads of that run were grouped it cost one request per page: on a GCE host on
// 2026-09-21 a cold restore of a 16 GiB guest spent 169 s in 1,632 such loads,
// against 3.8 s when a page was 2 MiB.

const (
	// runPages is the 2 MiB read-ahead run of a 4 KiB-page volume the pager
	// loads in one call, which is 512 pages.
	runPages = PageSize2MiB / PageSize4KiB
	// runVolumeSize spans the volume's first page-table segment boundary, which
	// at 16,384 pages of 4 KiB is 64 MiB, so a run can be read across one.
	runVolumeSize = 64<<20 + PageSize2MiB
	// segmentReads is what a run costs in page table: one range read of the
	// segment that locates its pages, whatever else that segment names.
	segmentReads = 1
)

// randomPages fills every page with bytes that do not compress, so a member is
// the page it holds plus its envelope and what a run costs in bytes is the
// run's own size rather than the encoder's opinion of its contents.
type randomPages struct{ tag uint64 }

func (r randomPages) fill(volume string, page uint64, dst []byte) {
	state := r.tag ^ (page+1)*0x9e3779b97f4a7c15 ^ uint64(len(volume))*0x632be59bd9b4e019
	var word [8]byte
	for at := 0; at < len(dst); at += len(word) {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(word[:], state)
		copy(dst[at:], word[:])
	}
}

func (r randomPages) ReadPage(_ context.Context, volume string, page uint64, dst []byte) error {
	r.fill(volume, page, dst)
	return nil
}

// memberBytes is what one whole 4 KiB page costs in a part: the page and the
// envelope around it, because bytes that do not compress are stored raw.
const memberBytes = PageSize4KiB + blob.HeaderSize

// runModel is what the volume holds: the bytes each publication wrote, so a
// read is compared with what was published rather than with itself.
type runModel struct {
	size     uint64
	contents map[uint64][]byte
}

func newRunModel(size uint64) *runModel {
	return &runModel{size: size, contents: make(map[uint64][]byte)}
}

func (m *runModel) publish(source randomPages, volume string, pages []uint64) {
	for _, page := range pages {
		data := make([]byte, PageSize4KiB)
		source.fill(volume, page, data)
		m.contents[page] = data
	}
}

// want is what a range of the volume reads back as: the bytes of every page
// that has them, and zeroes for every page that never had any.
func (m *runModel) want(offset, length uint64) []byte {
	data := make([]byte, length)
	for cursor := offset; cursor < offset+length; {
		page := cursor / PageSize4KiB
		stop := min(offset+length, (page+1)*PageSize4KiB)
		if held := m.contents[page]; held != nil {
			copy(data[cursor-offset:stop-offset], held[cursor-page*PageSize4KiB:])
		}
		cursor = stop
	}
	return data
}

// runFixture is a counted store over one 4 KiB-page volume, with the model of
// what has been published into it.
type runFixture struct {
	store   *Store
	counter *readCounter
	model   *runModel
	index   *Index
	ref     control.Ref
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	return newConfiguredRunFixture(t, Config{})
}

// newConfiguredRunFixture is newRunFixture over a store the test has configured
// — a part size small enough to spread one run over several parts, or a cache
// to read it through.
func newConfiguredRunFixture(t *testing.T, config Config) *runFixture {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 104})
	prefix, err := platform.NewObjectPrefix(fixturePrefix)
	if err != nil {
		t.Fatal(err)
	}
	counter := &readCounter{ObjectStore: runtime.ObjectStore()}
	config.ObjectStore, config.ObjectPrefix = counter, prefix
	store, err := NewStore(config)
	if err != nil {
		t.Fatal(err)
	}
	ref := control.Ref{VM: "run", Sequence: 1}
	root, err := store.Root(t.Context(), ref,
		volumesAt(at4KiB, map[string]uint64{"ram": runVolumeSize}))
	if err != nil {
		t.Fatal(err)
	}
	return &runFixture{store: store, counter: counter, model: newRunModel(runVolumeSize),
		index: root, ref: ref}
}

// parts reports how many parts the last checkpoint this fixture published has.
func (f *runFixture) parts() uint32 { return f.index.checkpoints[f.ref].parts }

// publish writes the given pages under the next checkpoint of this VM.
func (f *runFixture) publish(t *testing.T, tag uint64, pages []uint64) {
	t.Helper()
	f.ref = control.Ref{VM: f.ref.VM, Sequence: f.ref.Sequence + 1}
	publication := f.store.Begin(f.index, f.ref)
	for _, page := range pages {
		publication.Dirty("ram", page)
	}
	source := randomPages{tag: tag}
	index, err := publication.Commit(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	f.model.publish(source, "ram", pages)
	f.index = index
}

// read reopens the checkpoint, so that neither its page table nor any page of
// it is in hand, reads one range of the volume and reports how many object
// reads that cost and how many bytes they returned. The bytes must be what was
// published, whatever the reads were grouped into.
func (f *runFixture) read(t *testing.T, offset, length uint64) (reads int, bytesRead int64) {
	t.Helper()
	index, err := f.store.Open(t.Context(), f.ref)
	if err != nil {
		t.Fatal(err)
	}
	f.counter.reset()
	got := make([]byte, length)
	if err := f.store.Read(t.Context(), index, "ram", offset, got); err != nil {
		t.Fatal(err)
	}
	if want := f.model.want(offset, length); !bytes.Equal(got, want) {
		t.Fatalf("[%d,%d) reads back as %#x..., want %#x...", offset, offset+length, got[:8], want[:8])
	}
	lengths := f.counter.reads()
	total := int64(0)
	for _, at := range lengths {
		total += at
	}
	return len(lengths), total
}

// pagesOf lists the pages of a run that satisfy keep, in ascending order.
func pagesOf(first, count uint64, keep func(uint64) bool) []uint64 {
	var pages []uint64
	for page := first; page < first+count; page++ {
		if keep == nil || keep(page) {
			pages = append(pages, page)
		}
	}
	return pages
}

// A run of pages one checkpoint published in page order is one request. The
// publication writes a volume's changed pages in ascending page order, so their
// members lie next to each other in the part and one ranged read holds all of
// them; the segment that locates them is the run's only other read.
func TestARunPublishedByOneCheckpointIsOneRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		f.publish(t, 0x11, pagesOf(0, runPages, nil))
		reads, read := f.read(t, 0, PageSize2MiB)
		if want := segmentReads + 1; reads != want {
			t.Fatalf("a run of %d pages from one checkpoint cost %d reads of %d bytes, want %d",
				runPages, reads, read, want)
		}
	})
}

// A run whose pages come from three checkpoints is one request per checkpoint:
// each of them wrote its own pages next to each other in its own part, and a
// part is where a ranged read can reach.
func TestARunSpreadOverThreeCheckpointsIsOneRequestEach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		for remainder := range uint64(3) {
			f.publish(t, 0x20+remainder, pagesOf(0, runPages, func(page uint64) bool {
				return page%3 == remainder
			}))
		}
		reads, read := f.read(t, 0, PageSize2MiB)
		if want := segmentReads + 3; reads != want {
			t.Fatalf("a run of %d pages from three checkpoints cost %d reads of %d bytes, want %d",
				runPages, reads, read, want)
		}
	})
}

// A sparse run costs what the pages that exist cost and nothing for the ones
// that do not: a page no checkpoint ever wrote has no member, reads as zeroes,
// and leaves the members around it adjacent.
func TestASparseRunCostsOneRequestAndNothingForItsHoles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		f.publish(t, 0x33, pagesOf(0, runPages, func(page uint64) bool { return page%2 == 0 }))
		reads, read := f.read(t, 0, PageSize2MiB)
		if want := segmentReads + 1; reads != want {
			t.Fatalf("a run half of whose pages were never written cost %d reads of %d bytes, want %d",
				reads, read, want)
		}
		if want := int64(runPages / 2 * memberBytes); read < want || read > want+int64(defaultIndexTail) {
			t.Fatalf("the run read %d bytes, want about the %d its %d members hold",
				read, want, runPages/2)
		}
	})
}

// A gap between the members of one part that is small enough is read through
// rather than split at: one request that carries bytes nothing wants beats two
// round trips. Here a later checkpoint rewrote five pages in the middle of the
// run, so the first checkpoint's members have a five-member hole in them.
func TestASmallGapInARunIsReadThrough(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		f.publish(t, 0x44, pagesOf(0, runPages, nil))
		f.publish(t, 0x55, pagesOf(100, 5, nil))
		reads, read := f.read(t, 0, PageSize2MiB)
		if want := segmentReads + 2; reads != want {
			t.Fatalf("a run with a five-member gap cost %d reads of %d bytes, want %d",
				reads, read, want)
		}
	})
}

// A gap too large to read through splits the request instead: the bytes between
// the two halves of the run are worth more than the round trip that skips them.
func TestALargeGapInARunSplitsTheRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		f.publish(t, 0x66, pagesOf(0, runPages, nil))
		f.publish(t, 0x77, pagesOf(100, 100, nil))
		reads, read := f.read(t, 0, PageSize2MiB)
		if want := segmentReads + 3; reads != want {
			t.Fatalf("a run with a hundred-member gap cost %d reads of %d bytes, want %d",
				reads, read, want)
		}
	})
}

// A run that crosses a page-table segment boundary costs one read of each
// segment, and one member read still: a segment boundary is a boundary of the
// page table, and the pages on either side of it were published consecutively
// into one part, so what groups them is the part and not the segment that
// located them.
func TestARunAcrossASegmentBoundaryReadsEachSegmentOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		const boundary = 16 << 10
		f.publish(t, 0x88, pagesOf(boundary-runPages/2, runPages, nil))
		reads, read := f.read(t, (boundary-runPages/2)*PageSize4KiB, PageSize2MiB)
		if want := 2*segmentReads + 1; reads != want {
			t.Fatalf("a run across a segment boundary cost %d reads of %d bytes, want %d",
				reads, read, want)
		}
	})
}

// A checkpoint's members for consecutive pages of one volume are adjacent in
// its part. That is what makes a run one request, so it is asserted of the part
// itself rather than inferred from what a read cost.
func TestAPublicationWritesConsecutivePagesAdjacently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		f.publish(t, 0x99, pagesOf(0, runPages, nil))
		table, err := f.store.readPartTable(t.Context(), f.ref, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(table.members) != runPages {
			t.Fatalf("the part holds %d members, want the %d pages published", len(table.members), runPages)
		}
		members := slices.Clone(table.members)
		slices.SortFunc(members, func(a, b part.Member) int { return int(a.Page) - int(b.Page) })
		for at := 1; at < len(members); at++ {
			if members[at].Page != members[at-1].Page+1 {
				t.Fatalf("member %d holds page %d after page %d", at, members[at].Page, members[at-1].Page)
			}
			if members[at].Offset != members[at-1].Offset+members[at-1].Length {
				t.Fatalf("page %d sits at %d, want the %d its predecessor ends at",
					members[at].Page, members[at].Offset, members[at-1].Offset+members[at-1].Length)
			}
		}
	})
}

// A run spread over several parts is one request per part: a ranged read
// reaches inside one object, so a part is as far as one of them goes, and the
// parts are read at once rather than one after another.
func TestARunAcrossPartsIsOneRequestPerPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredRunFixture(t, Config{PartBytes: 512 << 10})
		f.publish(t, 0xbb, pagesOf(0, runPages, nil))
		parts := int(f.parts())
		if parts < 4 {
			t.Fatalf("a run of %d pages went into %d parts, want it spread over several", runPages, parts)
		}
		reads, read := f.read(t, 0, PageSize2MiB)
		if want := segmentReads + parts; reads != want {
			t.Fatalf("a run across %d parts cost %d reads of %d bytes, want %d",
				parts, reads, read, want)
		}
	})
}

// A run that starts and ends inside a page reads the pages it touches whole and
// copies out the bytes that were asked for, which is one request all the same.
func TestARunThatStartsAndEndsInsideAPageIsOneRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		f.publish(t, 0xcc, pagesOf(0, runPages, nil))
		reads, read := f.read(t, PageSize4KiB/2, PageSize2MiB-PageSize4KiB)
		if want := segmentReads + 1; reads != want {
			t.Fatalf("a run between two page boundaries cost %d reads of %d bytes, want %d",
				reads, read, want)
		}
	})
}

// A run covers a bounded piece of a volume, in whole pages, so a read longer
// than one is served as several and no page is ever split between two of them
// and fetched twice. The largest read-ahead run a pager may be configured with
// is one run, so no pager's load is ever split.
func TestARunIsBoundedInWholePages(t *testing.T) {
	for _, geometry := range []Geometry{at4KiB, at2MiB} {
		if got := runLimit(geometry, 0); got != maximumRunBytes {
			t.Fatalf("the first run of a %d-byte-page volume ends at %d, want %d",
				geometry.PageSize, got, maximumRunBytes)
		}
		if got := runLimit(geometry, geometry.PageSize/2); got != maximumRunBytes {
			t.Fatalf("a run starting inside the first page ends at %d, want %d", got, maximumRunBytes)
		}
		if got := runLimit(geometry, maximumRunBytes); got != 2*maximumRunBytes {
			t.Fatalf("the second run of a %d-byte-page volume ends at %d, want %d",
				geometry.PageSize, got, 2*maximumRunBytes)
		}
	}
	if maximumRunBytes < 16<<20 {
		t.Fatalf("a run covers %d bytes, want at least the largest read-ahead run a pager may hold",
			maximumRunBytes)
	}
}

// extentsIn is how many requests a run of adjacent members costs: one per
// maximumReadExtent of them, because that is what one request carries.
func extentsIn(members int) int {
	return (members*memberBytes + maximumReadExtent - 1) / maximumReadExtent
}

// A read longer than one run is served as several, each costing one request per
// extent of it, and the bytes across the boundary are the ones that were
// published. A 2 MiB run is one extent; a run of a whole maximumRunBytes of
// 4 KiB pages holds more members than one request carries and splits.
func TestAReadLongerThanOneRunIsServedAsSeveralRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const whole = maximumRunBytes / PageSize4KiB
		pages := uint64(whole + runPages)
		f := newRunFixture(t)
		f.publish(t, 0xee, pagesOf(0, pages, nil))
		reads, read := f.read(t, 0, pages*PageSize4KiB)
		if want := segmentReads + extentsIn(whole) + extentsIn(runPages); reads != want {
			t.Fatalf("a read of %d pages cost %d reads of %d bytes, want %d",
				pages, reads, read, want)
		}
		if extentsIn(runPages) != 1 {
			t.Fatalf("a 2 MiB run of 4 KiB pages costs %d requests, want one", extentsIn(runPages))
		}
	})
}

// The second read of a run through a cache costs nothing: every page the first
// read fetched was retained under its own identity, whichever request carried
// it, so a second reader of any of them finds it there.
func TestASecondReadOfARunThroughTheCacheCostsNoRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, err := NewCache(testresource.New(), CacheConfig{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cache.Close)
		f := newConfiguredRunFixture(t, Config{Cache: cache})
		f.publish(t, 0xdd, pagesOf(0, runPages, nil))
		if reads, read := f.read(t, 0, PageSize2MiB); reads != segmentReads+1 {
			t.Fatalf("the first read of the run cost %d reads of %d bytes, want %d",
				reads, read, segmentReads+1)
		}
		index, err := f.store.Open(t.Context(), f.ref)
		if err != nil {
			t.Fatal(err)
		}
		f.counter.reset()
		got := make([]byte, PageSize2MiB)
		if err := f.store.Read(t.Context(), index, "ram", 0, got); err != nil {
			t.Fatal(err)
		}
		if want := f.model.want(0, PageSize2MiB); !bytes.Equal(got, want) {
			t.Fatalf("the cached run reads back as %#x..., want %#x...", got[:8], want[:8])
		}
		// The segment is keyed by its own identity too, so the reopened index
		// shares the copy the first read left.
		if reads := f.counter.reads(); len(reads) != 0 {
			t.Fatalf("a second read of the run cost %v, want nothing at all", reads)
		}
		if stats := cache.Stats(); stats.Hits != runPages+segmentReads {
			t.Fatalf("the cache reports %+v, want a hit for each of the %d pages and the segment",
				stats, runPages)
		}
	})
}

// Concurrent readers of one run each get the bytes that were published,
// whatever requests the grouping shared between them.
func TestConcurrentReadersOfOneRunAgreeOnItsBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRunFixture(t)
		f.publish(t, 0xaa, pagesOf(0, runPages, nil))
		index, err := f.store.Open(t.Context(), f.ref)
		if err != nil {
			t.Fatal(err)
		}
		want := f.model.want(0, PageSize2MiB)
		var wait sync.WaitGroup
		failures := make([]error, 8)
		for reader := range failures {
			wait.Add(1)
			go func() {
				defer wait.Done()
				got := make([]byte, PageSize2MiB)
				if err := f.store.Read(t.Context(), index, "ram", 0, got); err != nil {
					failures[reader] = err
					return
				}
				if !bytes.Equal(got, want) {
					failures[reader] = errReaderDisagreed
				}
			}()
		}
		wait.Wait()
		for reader, err := range failures {
			if err != nil {
				t.Fatalf("reader %d: %v", reader, err)
			}
		}
	})
}
