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

// publishedBacking is a pager's view of that volume: the one backing a region
// maps, reading through the checkpoint store exactly as *volume.Volume does.
type publishedBacking struct {
	volume *publishedVolume
	index  *checkpoint.Index
	mu     sync.Mutex
	// loads records the pages each Load asked for: its first page and how many
	// pages of the volume it covered.
	loads [][2]uint64
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
	return b.index.Locate(ctx, "ram", offset, length)
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

// A fault brings its whole window in one read, and the pages the region already
// holds — the ones its populate mapped because a sibling had made them resident
// — do not cut that read into pieces. What is left of the run is one ranged
// read per part its pages lie in, so a window published by three checkpoints
// costs three reads however scattered through it the pages the region holds are.
func TestAFaultOverResidentPagesIsOneLoadAndOneReadPerCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		published := newPublishedVolume(t, runCheckpoints)
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
			ResidentPages: 4 * runWindow, LogicalPages: 8 * runWindow, DirtyPages: runWindow,
			ReadAheadPages: runWindow, WriteAheadPages: 1})

		// A sibling fork stores into scattered pages of the same window. Each
		// store reads the page it copies from in under its published identity,
		// so those pages are resident for every region that inherits them.
		sibling := published.fork(t)
		s, sm := f.attach(sibling)
		var resident []uint64
		for page := uint64(3); page < runWindow; page += 8 {
			resident = append(resident, page)
			access(t, s, sm, page, true)[0] = 0xaa
		}

		// The fork attaches and its populate maps exactly those pages, which is
		// what leaves its first fault a window with holes in it.
		fork := published.fork(t)
		r, m := f.attach(fork)
		if len(m.pages) != len(resident) {
			t.Fatalf("the populate mapped %d pages, want the %d a sibling had made resident", len(m.pages), len(resident))
		}
		fork.reset()
		published.reads.reset()
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

// summarise names the first few loads a fault made, so a failure says what
// shape they had rather than printing hundreds of pairs.
func summarise(loads [][2]uint64) string {
	if len(loads) > 6 {
		return fmt.Sprintf("%v and %d more", loads[:6], len(loads)-6)
	}
	return fmt.Sprintf("%v", loads)
}
