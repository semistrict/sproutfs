package vmmemory_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// An enormous logical image with only a few stored pages. This adapter never
// allocates memory proportional to logical capacity.
type sparseMemoryBacking struct {
	size  uint64
	vm    string
	mu    sync.Mutex
	pages map[uint64][]byte
}

func (b *sparseMemoryBacking) Size() uint64                     { return b.size }
func (b *sparseMemoryBacking) Verify(ctx context.Context) error { return context.Cause(ctx) }
func (b *sparseMemoryBacking) Load(ctx context.Context, offset uint64, dst []byte) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(dst)
	for i := 0; i < len(dst); {
		page, inPage := (offset+uint64(i))/pageSize, (offset+uint64(i))%pageSize
		count := min(len(dst)-i, pageSize-int(inPage))
		if data := b.pages[page]; data != nil {
			copy(dst[i:i+count], data[inPage:inPage+uint64(count)])
		}
		i += count
	}
	return nil
}

// checkpoint publishes a region's sealed pages into this backing, which is what
// a checkpoint of it does.
func (b *sparseMemoryBacking) checkpoint(t *testing.T, r *vmmemory.Region) error {
	t.Helper()
	if err := r.Seal(t.Context()); err != nil {
		return err
	}
	ckpt := r.Checkpoint()
	page := make([]byte, pageSize)
	for _, number := range ckpt.DirtyPages() {
		if err := ckpt.ReadDirty(t.Context(), number, page); err != nil {
			return err
		}
		b.mu.Lock()
		if b.pages[number] == nil {
			b.pages[number] = make([]byte, pageSize)
		}
		copy(b.pages[number], page)
		b.mu.Unlock()
	}
	return ckpt.Retire(t.Context(), true)
}
func (b *sparseMemoryBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Tests write at most four pages; gaps are emitted as whole zero ranges.
	end := offset + length
	var result []control.Extent
	for offset < end {
		page := offset / pageSize
		if b.pages[page] != nil {
			stop := min(end, (page+1)*pageSize)
			result = append(result, control.Extent{Offset: offset, Length: stop - offset,
				Identity: control.Identity{Ref: control.Ref{VM: b.vm, Sequence: 2}, Volume: "v", Page: offset / checkpoint.PageSize2MiB}})
			offset = stop
			continue
		}
		stop := end
		for page := range b.pages {
			if pos := page * pageSize; pos > offset {
				stop = min(stop, pos)
			}
		}
		result = append(result, control.Extent{Offset: offset, Length: stop - offset, Identity: control.Identity{Zero: true}})
		offset = stop
	}
	return result, nil
}

func TestLargeLogicalRegionAllocatesMetadataOnlyWhenUsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 32 << 30 / pageSize
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: pages, DirtyPages: 4, ReadAheadPages: 1})
		b := &sparseMemoryBacking{size: pages * pageSize, vm: "sparse", pages: make(map[uint64][]byte)}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r, m := f.attach(b)
		runtime.ReadMemStats(&after)
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
			t.Fatalf("untouched 32 GiB region allocated %d metadata bytes, budget 2 MiB", allocated)
		}
		selected := []uint64{0, 255, 1 << 13, pages - 1}
		for i, page := range selected {
			access(t, r, m, page, true)[0] = byte(i + 41)
		}
		for i, page := range selected {
			if got := access(t, r, m, page, false)[0]; got != byte(i+41) {
				t.Fatalf("far page %d lost private value: %d", page, got)
			}
		}
		if err := b.checkpoint(t, r); err != nil {
			t.Fatal(err)
		}
		for i, page := range selected {
			data := make([]byte, pageSize)
			if err := b.Load(t.Context(), page*pageSize, data); err != nil || data[0] != byte(i+41) {
				t.Fatalf("far dirty page %d was not published: %d %v", page, data[0], err)
			}
		}
	})
}

// Tracks real data mappings individually and zero runs without materializing a
// fake per-page map. There are no actual memory users behind this adapter.
type sparseZeroMapping struct {
	data      map[uint64]int
	zeroPages uint64
}

func (m *sparseZeroMapping) Map(_ context.Context, page uint64, slot, count int, _ bool) error {
	for i := range count {
		m.data[page+uint64(i)] = slot + i
	}
	return nil
}
func (m *sparseZeroMapping) MapZero(_ context.Context, _ uint64, count int) error {
	m.zeroPages += uint64(count)
	return nil
}
func (m *sparseZeroMapping) Revoke(_ context.Context, page uint64) error {
	delete(m.data, page)
	return nil
}
func (m *sparseZeroMapping) Resolve(context.Context, uint64, int, bool) error { return nil }
func (m *sparseZeroMapping) Protect(context.Context, uint64, int) error       { return nil }

func TestEagerZeroPopulationKeepsLargeLogicalMetadataSparse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 32 << 30 / pageSize
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 1, LogicalPages: pages + 1, DirtyPages: 2, ReadAheadPages: 1})
		seed, mapping := f.attach(&sparseMemoryBacking{size: pageSize, vm: "seed", pages: make(map[uint64][]byte)})
		access(t, seed, mapping, 0, false) // establishes known sparse backing in the pager
		b := &sparseMemoryBacking{size: pages * pageSize, vm: "big", pages: make(map[uint64][]byte)}
		m := &sparseZeroMapping{data: make(map[uint64]int)}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r, err := f.h.Attach(t.Context(), ram(b), m)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Detach(t.Context())
		runtime.GC()
		runtime.ReadMemStats(&after)
		if allocated := int64(after.HeapAlloc) - int64(before.HeapAlloc); allocated > 2<<20 {
			t.Fatalf("eager 32 GiB zero region retained %d metadata bytes, budget 2 MiB", allocated)
		}
		if m.zeroPages != pages {
			t.Fatalf("only %d of %d zero pages were eagerly mapped", m.zeroPages, pages)
		}
		if err := r.Fault(t.Context(), pages-1, true); err != nil {
			t.Fatal(err)
		}
		f.a.slots[m.data[pages-1]][0] = 42
		if err := r.Fault(t.Context(), 0, true); err != nil {
			t.Fatal(err)
		}
		f.a.slots[m.data[0]][0] = 17
		if err := r.Fault(t.Context(), pages-1, false); err != nil {
			t.Fatal(err)
		}
		if got := f.a.slots[m.data[pages-1]][0]; got != 42 {
			t.Fatalf("far private page refaulted as zero: %d", got)
		}
		if err := r.Fault(t.Context(), pages-2, false); err != nil {
			t.Fatal(err)
		}
		if err := b.checkpoint(t, r); err != nil {
			t.Fatal(err)
		}
		if b.pages[0][0] != 17 || b.pages[pages-1][0] != 42 {
			t.Fatal("private pages were lost when splitting the zero range")
		}
	})
}

func TestFragmentedPrivatePagesSplitCompressedZeroMappings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 512
		// One page per store: write-ahead would privatize the odd pages between
		// the stores and leave no zero run to split.
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 4, LogicalPages: 2 * pages, DirtyPages: pages, ReadAheadPages: 4, WriteAheadPages: 1})
		backing := f.newBacking(pages)
		clear(backing.data)
		for page := range uint64(pages) {
			backing.zero[page] = true
		}
		first, fm := f.attach(backing)
		access(t, first, fm, 0, false)
		siblingBacking := f.newBacking(pages)
		clear(siblingBacking.data)
		for page := range uint64(pages) {
			siblingBacking.zero[page] = true
		}
		sibling, sm := f.attach(siblingBacking)
		if err := first.Populate(t.Context()); err != nil {
			t.Fatal(err)
		}
		// A coprime permutation repeatedly splits both sides of existing runs.
		for i := range pages / 2 {
			page := uint64((i * 73) % (pages / 2) * 2)
			access(t, first, fm, page, true)[0] = byte(page%251 + 1)
		}
		if err := first.Populate(t.Context()); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(pages) {
			want := byte(0)
			if page%2 == 0 {
				want = byte(page%251 + 1)
			}
			if got := access(t, first, fm, page, false)[0]; got != want {
				t.Fatalf("fragmented page %d=%d want=%d", page, got, want)
			}
			if got := access(t, sibling, sm, page, false)[0]; got != 0 {
				t.Fatalf("zero sibling page %d changed to %d", page, got)
			}
		}
		f.mustCheckpoint(first, backing)
		for page := range uint64(pages) {
			want := byte(0)
			if page%2 == 0 {
				want = byte(page%251 + 1)
			}
			if got := backing.data[page*pageSize]; got != want {
				t.Fatalf("published fragmented page %d=%d want=%d", page, got, want)
			}
		}
	})
}
