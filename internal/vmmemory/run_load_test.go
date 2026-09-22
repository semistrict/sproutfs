package vmmemory_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// What one fault costs a real volume is what these measure. A RAM pager at
// 4 KiB faults a 2 MiB read-ahead run of 512 pages, and a checkpoint's run of
// pages is one ranged read per part it lies in — but only if the pager asks for
// the run. A fault that asks for each stretch of it separately pays a request
// per stretch, and the pages it already holds resident are what cut the run
// into stretches: on a GCE host on 2026-09-22 a fork fan-out took 8,660 loads
// to bring 31,867 pages, 3.7 pages a load, and made 7,365 object reads of them.

const (
	// runWindow is the 2 MiB read-ahead run of a 4 KiB pager, which is 512
	// pages: one fault's whole window.
	runWindow = checkpoint.PageSize2MiB / checkpoint.PageSize4KiB
	// runCheckpoints is how many checkpoints publish that window between them,
	// page by page, so its pages lie in that many parts.
	runCheckpoints = 3
	// runVolume is the volume the window covers, whole.
	runVolume = runWindow * checkpoint.PageSize4KiB
	// windowSegmentReads is what one fault costs in page table: nothing, because
	// the segment locating the window's pages was read when the region attached
	// and the index it was read into is the region's for the VM's life.
	windowSegmentReads = 0
)

// objectReads counts the reads a store makes and the bytes they return, so a
// test measures what one fault cost rather than everything before it.
type objectReads struct {
	platform.ObjectStore
	mu      sync.Mutex
	lengths []int64
}

func (c *objectReads) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	result, err := c.ObjectStore.Get(ctx, request)
	if err == nil {
		c.mu.Lock()
		c.lengths = append(c.lengths, result.ContentLength)
		c.mu.Unlock()
	}
	return result, err
}

func (c *objectReads) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lengths = nil
}

// count reports the reads since the last reset and the bytes they returned.
func (c *objectReads) count() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := int64(0)
	for _, length := range c.lengths {
		total += length
	}
	return len(c.lengths), total
}

// publishedPages fills every page with bytes that do not compress, so what a
// run costs in bytes is the run's own size and not the encoder's opinion of it.
type publishedPages struct{}

func (publishedPages) ReadPage(_ context.Context, volume string, page uint64, dst []byte) error {
	state := (page + 1) * 0x9e3779b97f4a7c15
	var word [8]byte
	for at := 0; at < len(dst); at += len(word) {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(word[:], state)
		copy(dst[at:], word[:])
	}
	return nil
}

// publishedVolume is one 4 KiB-page volume of a published checkpoint, with the
// object reads its store makes counted. Every backing built from it is a fork
// of the same checkpoint, so equal pages carry equal identities.
type publishedVolume struct {
	store  *checkpoint.Store
	reads  *objectReads
	ref    control.Ref
	source publishedPages
}

// newPublishedVolume publishes one window of pages, page by page, across the
// given number of checkpoints, so the run a fault makes lies in that many parts.
func newPublishedVolume(t *testing.T, checkpoints int) *publishedVolume {
	t.Helper()
	prefix, err := platform.NewObjectPrefix("run")
	if err != nil {
		t.Fatal(err)
	}
	reads := &objectReads{ObjectStore: sim.New(sim.Config{Seed: 17}).ObjectStore()}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: reads, ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	ref := control.Ref{VM: "run", Sequence: 1}
	index, err := store.Root(t.Context(), ref,
		map[string]checkpoint.VolumeSpec{"ram": {Size: runVolume, PageSize: checkpoint.PageSize4KiB}})
	if err != nil {
		t.Fatal(err)
	}
	v := &publishedVolume{store: store, reads: reads}
	for at := range checkpoints {
		ref = control.Ref{VM: ref.VM, Sequence: ref.Sequence + 1}
		publication := store.Begin(index, ref)
		for page := uint64(at); page < runWindow; page += uint64(checkpoints) {
			publication.Dirty("ram", page)
		}
		if index, err = publication.Commit(t.Context(), v.source); err != nil {
			t.Fatal(err)
		}
	}
	v.ref = ref
	return v
}

// fork is one region's view of the published checkpoint, opened by itself: two
// forks of one checkpoint hold their own index handles and share nothing but
// the objects.
func (v *publishedVolume) fork(t *testing.T) *publishedBacking {
	t.Helper()
	index, err := v.store.Open(t.Context(), v.ref)
	if err != nil {
		t.Fatal(err)
	}
	return &publishedBacking{volume: v, index: index}
}

// forkHolding is a fork of the same checkpoint that inherited only the pages
// keep names, every other page of its volume being a hole. It is how these make
// a scattered set of one window's pages resident, under the identities a whole
// fork of that checkpoint inherits, without bringing the window in: a fork that
// inherits a page and faults it holds the whole run around it.
func (v *publishedVolume) forkHolding(t *testing.T, keep func(uint64) bool) *publishedBacking {
	t.Helper()
	b := v.fork(t)
	b.holds = keep
	return b
}

// publishedBacking is a pager's view of that volume: the one backing a region
// maps, reading through the checkpoint store exactly as *volume.Volume does.
type publishedBacking struct {
	volume *publishedVolume
	index  *checkpoint.Index
	mu     sync.Mutex
	// loads records the pages each Load asked for: its first page and how many
	// pages of the volume it covered.
	loads [][2]uint64
	// holds names the pages this fork inherited; every other page of its volume
	// reads as a hole. Nil inherits the whole checkpoint.
	holds func(page uint64) bool
}

func (b *publishedBacking) Size() uint64     { return runVolume }
func (b *publishedBacking) PageSize() uint64 { return checkpoint.PageSize4KiB }
func (b *publishedBacking) Verify(context.Context) error {
	return nil
}

func (b *publishedBacking) Load(ctx context.Context, offset uint64, dst []byte) error {
	return b.LoadPages(ctx, offset, dst, nil)
}

func (b *publishedBacking) LoadPages(ctx context.Context, offset uint64, dst []byte, wanted []bool) error {
	b.mu.Lock()
	b.loads = append(b.loads, [2]uint64{offset / checkpoint.PageSize4KiB,
		uint64(len(dst)) / checkpoint.PageSize4KiB})
	b.mu.Unlock()
	return b.volume.store.ReadPages(ctx, b.index, "ram", offset, dst, wanted)
}

func (b *publishedBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	extents, err := b.index.Locate(ctx, "ram", offset, length)
	if err != nil || b.holds == nil {
		return extents, err
	}
	for at := range extents {
		if !b.holds(extents[at].Offset / checkpoint.PageSize4KiB) {
			extents[at].Identity = control.ZeroIdentity
		}
	}
	return extents, nil
}

func (b *publishedBacking) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loads = nil
}

func (b *publishedBacking) recorded() [][2]uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.loads)
}

// newRunPager is the pager these measure a window against: a 4 KiB page, a
// read-ahead run of one whole 2 MiB window, and arena for several of them.
func newRunPager(t *testing.T) *fixture {
	t.Helper()
	return newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
		ResidentPages: 4 * runWindow, LogicalPages: 8 * runWindow, DirtyPages: runWindow,
		ReadAheadPages: runWindow, WriteAheadPages: 1})
}

// residentWindow is the shape a fan-out leaves a child to fault into: a fork
// whose first window is scattered with pages this pager already holds, because
// a sibling fork inherited exactly those and has faulted them. They are what
// leave its first fault a window with holes in it — the fault binds each of
// them to the sibling's own page rather than reading it — and the attach maps
// none of them, because runs of one page are not worth a mapping command each.
// It reports the region, its mapping, its backing and the pages the pager
// holds, with the load and object-read counts reset to that moment.
func residentWindow(t *testing.T, f *fixture, published *publishedVolume) (*vmmemory.Region, *mapping, *publishedBacking, []uint64) {
	t.Helper()
	inherited := func(page uint64) bool { return page%8 == 3 }
	var resident []uint64
	for page := range uint64(runWindow) {
		if inherited(page) {
			resident = append(resident, page)
		}
	}
	sibling := published.forkHolding(t, inherited)
	s, sm := f.attach(sibling)
	access(t, s, sm, resident[0], false)
	if hits, err := f.h.Stats(t.Context()); err != nil || hits.LoadedPages != uint64(len(resident)) {
		t.Fatalf("the sibling loaded %d pages, want the %d it inherited: %v", hits.LoadedPages, len(resident), err)
	}
	fork := published.fork(t)
	r, m := f.attach(fork)
	if len(m.pages) != 0 {
		t.Fatalf("the populate mapped %d pages, want none: a sibling holds them in runs of one",
			len(m.pages))
	}
	fork.reset()
	published.reads.reset()
	return r, m, fork, resident
}

// A fault brings its whole window in one read, and the pages the region already
// holds — the ones its populate mapped because a sibling had made them resident
// — do not cut that read into pieces. What is left of the run is one ranged
// read per part its pages lie in, so a window published by three checkpoints
// costs three reads however scattered through it the pages the region holds are.
func TestAFaultOverResidentPagesIsOneLoadAndOneReadPerCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		published := newPublishedVolume(t, runCheckpoints)
		f := newRunPager(t)
		r, m, fork, resident := residentWindow(t, f, published)
		before, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		access(t, r, m, 0, false)
		after, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		loads := fork.recorded()
		reads, bytes := published.reads.count()
		if len(loads) != 1 || loads[0] != [2]uint64{0, runWindow} {
			t.Fatalf("one fault over %d pages of which %d were already held made %d loads (%v) and %d object reads of %d bytes, want one load of the whole window",
				runWindow, len(resident), len(loads), summarise(loads), reads, bytes)
		}
		if got := after.Loads - before.Loads; got != 1 {
			t.Fatalf("the fault counted %d loads, want one", got)
		}
		if got, want := after.LoadedPages-before.LoadedPages, uint64(runWindow-len(resident)); got != want {
			t.Fatalf("the fault loaded %d pages, want the %d the window did not already hold", got, want)
		}
		if len(m.pages) != runWindow {
			t.Fatalf("the fault left %d of the window's %d pages mapped", len(m.pages), runWindow)
		}
		if want := windowSegmentReads + runCheckpoints; reads != want {
			t.Fatalf("the fault made %d object reads of %d bytes, want %d: one per checkpoint that published the run",
				reads, bytes, want)
		}
	})
}

// A store into a page this region holds nothing for brings its window in
// exactly as a read fault does — one load, one object read per checkpoint that
// published the run — and then copies one page. On x86-64 that is how a fork
// faults at all: KVM finishes a fault that had to wait for the pager from a
// worker that asks for the page writable, so a guest merely reading what it
// inherited reaches the pager as a store, and a store that read one page at a
// time made a whole guest's first pass one round trip per 4 KiB.
//
// What it brings in is shared. Every other page of the window is mapped
// read-only under the identity its volume gave it and is there for the next
// fork of that checkpoint; the one private page is the one the guest stored
// into, and that page is never mapped read-only first, so a store still costs
// no revocation.
func TestAStoreIntoAColdPageBringsItsWholeWindowIn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		published := newPublishedVolume(t, runCheckpoints)
		f := newRunPager(t)
		r, m, fork, resident := residentWindow(t, f, published)
		before, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		access(t, r, m, 0, true)[0] = 0x5a
		after, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		loads := fork.recorded()
		reads, bytes := published.reads.count()
		if len(loads) != 1 || loads[0] != [2]uint64{0, runWindow} {
			t.Fatalf("a store into a cold page of a %d-page window made %d loads (%v) and %d object reads of %d bytes, want one load of the whole window",
				runWindow, len(loads), summarise(loads), reads, bytes)
		}
		if want := windowSegmentReads + runCheckpoints; reads != want {
			t.Fatalf("the store made %d object reads of %d bytes, want %d: one per checkpoint that published the run",
				reads, bytes, want)
		}
		if got := after.Loads - before.Loads; got != 1 {
			t.Fatalf("the store counted %d loads, want one", got)
		}
		if got, want := after.LoadedPages-before.LoadedPages, uint64(runWindow-len(resident)); got != want {
			t.Fatalf("the store loaded %d pages, want the %d the window did not already hold", got, want)
		}
		if got := after.CopyOnWrites - before.CopyOnWrites; got != 1 {
			t.Fatalf("the store copied %d pages, want the one the guest wrote", got)
		}
		if after.DirtyPages != 1 {
			t.Fatalf("the region holds %d dirty pages, want the one page stored into", after.DirtyPages)
		}
		if got := after.Revocations - before.Revocations; got != 0 {
			t.Fatalf("the store revoked %d mappings, want none: its own page was never mapped read-only first", got)
		}
		if len(m.pages) != runWindow {
			t.Fatalf("the store left %d of the window's %d pages mapped", len(m.pages), runWindow)
		}
		for page := range uint64(runWindow) {
			if writable := m.pages[page].writable; writable != (page == 0) {
				t.Fatalf("page %d is writable = %t, want %t: only the page stored into is private",
					page, writable, page == 0)
			}
		}
		if got := access(t, r, m, 0, false)[0]; got != 0x5a {
			t.Fatalf("the page the guest stored into holds %#x, want the byte it wrote", got)
		}
		// The window is in the sharing index, not this region's own: a third
		// fork of the same checkpoint takes every page of it and reads nothing.
		// Its attach maps none of them — the sibling's pages and this fork's are
		// interleaved in the arena, so no run of the window is a window long and
		// none is worth a mapping command of its own — and its first fault takes
		// the whole window out of the index.
		third := published.fork(t)
		tr, om := f.attach(third)
		if len(om.pages) != 0 {
			t.Fatalf("a third fork's attach mapped %d pages, want none: the window is in no run worth a command",
				len(om.pages))
		}
		access(t, tr, om, 0, false)
		if third.recorded() != nil || len(om.pages) != runWindow {
			t.Fatalf("a third fork loaded %v and mapped %d of %d pages, want the whole window from the sharing index",
				summarise(third.recorded()), len(om.pages), runWindow)
		}
	})
}

// summarise names the first few loads a fault made, so a failure says what
// shape they had rather than printing hundreds of pairs.
func summarise(loads [][2]uint64) string {
	if len(loads) > 6 {
		return fmt.Sprintf("%v and %d more", loads[:6], len(loads)-6)
	}
	return fmt.Sprintf("%v", loads)
}
