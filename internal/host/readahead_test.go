package host_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// What a cold read-ahead run costs is what a cold restore costs. A RAM pager
// faults one page and loads the whole 2 MiB run around it in one call to its
// volume, and at 4 KiB that run is 512 pages. On a GCE host on 2026-09-21 a
// restore of a 16 GiB guest made 1,632 such loads of 24,340 pages and took
// 169 s, against 3.8 s when a RAM page was 2 MiB, because every page of a run
// was a request of its own.

// readAheadPages is the 2 MiB run a RAM pager loads at once, in 4 KiB pages.
const readAheadPages = checkpoint.PageSize2MiB / checkpoint.PageSize4KiB

// readAheadVolumes is a VM whose memory is exactly one such run.
var readAheadVolumes = []volume.VolumeSpec{
	{Name: "ram0", Size: checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize4KiB},
}

// countedObjects counts the reads one host makes of object storage.
type countedObjects struct {
	platform.ObjectStore
	mu   sync.Mutex
	gets int
}

func (c *countedObjects) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	result, err := c.ObjectStore.Get(ctx, request)
	if err == nil {
		c.mu.Lock()
		c.gets++
		c.mu.Unlock()
	}
	return result, err
}

func (c *countedObjects) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets = 0
}

func (c *countedObjects) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}

// readAheadContents fills dst with bytes the encoder cannot shrink, so the
// members of the run are the pages they hold.
func readAheadContents(dst []byte) {
	state := uint64(0x9e3779b97f4a7c15)
	var word [8]byte
	for at := 0; at < len(dst); at += len(word) {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(word[:], state)
		copy(dst[at:], word[:])
	}
}

// coldRun publishes one 2 MiB run of 4 KiB RAM pages from one host, takes the
// VM over on a second whose page cache has read none of it, and attaches its
// memory to a RAM pager whose read-ahead run is the whole of it. It reports the
// object reads that host makes, its pagers, the mapping the region installs
// into, the region and the bytes that were published, with the read count reset
// to the moment before the first fault.
func coldRun(t *testing.T) (*countedObjects, *hostPagers, *pageMapping, *vmmemory.Region, []byte) {
	t.Helper()
	h := newSizedHostHarness(t, 2)
	counted := &countedObjects{ObjectStore: h.configs[1].ObjectStore}
	h.configs[1].ObjectStore = counted
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", readAheadVolumes)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, checkpoint.PageSize2MiB)
	readAheadContents(want)
	for page := range uint64(readAheadPages) {
		at := page * checkpoint.PageSize4KiB
		if err := vm.Volume("ram0").Write(t.Context(), at, want[at:at+checkpoint.PageSize4KiB]); err != nil {
			t.Fatal(err)
		}
	}
	if err := vm.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := vm.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The second host takes the VM over, so nothing of the checkpoint is in
	// hand: its page cache is its own and it has read none of it.
	pagers := newPagerWithConfig(t, h.configs[1].Resources, vmmemory.Config{
		PageSize: checkpoint.PageSize4KiB, ResidentPages: 2 * readAheadPages,
		LogicalPages: 4 * readAheadPages, DirtyPages: readAheadPages,
		ReadAheadPages: readAheadPages})
	opened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	mapping := newPageMapping(pagers.arenas[vmmemory.Ram])
	region, err := pagers.pagers.For(vmmemory.Ram).Attach(t.Context(),
		vmmemory.RegionBacking{Kind: vmmemory.Ram, Backing: opened.Volume("ram0")}, mapping)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := region.Detach(context.Background()); err != nil {
			t.Error(err)
		}
	})
	counted.reset()
	return counted, pagers, mapping, region, want
}

// One cold read-ahead run of 512 RAM pages costs two object-store requests: the
// page table segment that locates them, and the one extent their members lie in.
func TestAColdReadAheadRunOfRAMPagesIsTwoRequests(t *testing.T) {
	counted, pagers, mapping, region, want := coldRun(t)
	if err := region.Fault(t.Context(), 0, false); err != nil {
		t.Fatal(err)
	}
	if gets := counted.count(); gets != 2 {
		t.Fatalf("a cold read-ahead run of %d RAM pages cost %d requests, want the segment and the one extent",
			readAheadPages, gets)
	}
	// The whole run is resident and holds what was published: one fault, one
	// run, and the bytes of every page of it.
	arena := pagers.arenas[vmmemory.Ram]
	for page := range uint64(readAheadPages) {
		mapping.mu.Lock()
		slot, mapped := mapping.pages[page]
		mapping.mu.Unlock()
		if !mapped {
			t.Fatalf("page %d of the run was not mapped by the fault", page)
		}
		arena.mu.Lock()
		got := bytes.Clone(arena.slots[slot])
		arena.mu.Unlock()
		at := page * checkpoint.PageSize4KiB
		if !bytes.Equal(got, want[at:at+checkpoint.PageSize4KiB]) {
			t.Fatalf("page %d of the run holds %#x..., want %#x...", page, got[:8], want[at:at+8])
		}
	}
}

// A cold store costs the same two requests, because a store is how a fork
// faults. On x86-64 KVM finishes a fault that had to wait for the pager from a
// worker that asks for the page writable whatever the guest's access was, so a
// guest merely reading what it inherited reaches the pager as a store: on a GCE
// host on 2026-09-22 a fork fan-out took 21,130 faults of which 20,016 were
// copy-on-writes, and a store that read its own page alone made that pass one
// request per 4 KiB. The run it brings in is shared — only the page the guest
// stored into is private, and that page is never mapped read-only first, so a
// store still costs no revocation.
func TestAColdStoreBringsInTheRunAndCopiesOnePage(t *testing.T) {
	counted, pagers, mapping, region, want := coldRun(t)
	ram := pagers.ram()
	before, err := ram.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := region.Fault(t.Context(), 0, true); err != nil {
		t.Fatal(err)
	}
	after, err := ram.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if gets := counted.count(); gets != 2 {
		t.Fatalf("a cold store into a %d-page run cost %d requests, want the segment and the one extent",
			readAheadPages, gets)
	}
	if got := after.Loads - before.Loads; got != 1 {
		t.Fatalf("the store made %d loads, want one of the whole run", got)
	}
	if got := after.LoadedPages - before.LoadedPages; got != readAheadPages {
		t.Fatalf("the store loaded %d pages, want the %d the run holds", got, readAheadPages)
	}
	if got := after.CopyOnWrites - before.CopyOnWrites; got != 1 {
		t.Fatalf("the store copied %d pages, want the one the guest wrote", got)
	}
	if got := after.Revocations - before.Revocations; got != 0 {
		t.Fatalf("the store revoked %d mappings, want none: its own page was never mapped read-only first", got)
	}
	arena := pagers.arenas[vmmemory.Ram]
	for page := range uint64(readAheadPages) {
		mapping.mu.Lock()
		slot, mapped := mapping.pages[page]
		writable := mapping.write[page]
		mapping.mu.Unlock()
		if !mapped {
			t.Fatalf("page %d of the run was not mapped by the store", page)
		}
		if writable != (page == 0) {
			t.Fatalf("page %d is writable = %t, want %t: only the page stored into is private",
				page, writable, page == 0)
		}
		arena.mu.Lock()
		got := bytes.Clone(arena.slots[slot])
		arena.mu.Unlock()
		at := page * checkpoint.PageSize4KiB
		if !bytes.Equal(got, want[at:at+checkpoint.PageSize4KiB]) {
			t.Fatalf("page %d of the run holds %#x..., want %#x...", page, got[:8], want[at:at+8])
		}
	}
}
