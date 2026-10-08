//go:build !sproutfsprobe

package vmmemory_test

import (
	"os"
	"runtime"
	"runtime/pprof"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// residentHeapPages is how many pages the measurement below holds resident:
// enough that what each costs dominates everything the fixture itself holds.
const residentHeapPages = 1 << 16

// residentHeapBytes bounds what one resident page costs the host's heap. It
// measured 923 bytes with a map of aliases and a list element per page, and 731
// with one alias inline and the lists' links in the page: the rest is the page
// struct, its lock, its entry in the sharing index and its binding. Both counted
// the fixture's record of its mapping too, which is about fifty bytes a page;
// without it the page costs 678. It is 674 on 2026-09-30, measured with the
// read-ahead buffers the pager pools let go (see settle): before that, a run
// read 706, 738 or 771 by how many of them the pool happened to hold.
const residentHeapBytes = 768

// What the pager's own heap costs per page it holds resident, beyond the page
// itself, which is in the arena. A 4 KiB pager's arena of 24 GiB is six million
// pages, so every hundred bytes here is 600 MB of the host's heap.
func TestAResidentPageCostsLittleHeap(t *testing.T) {
	vmmemory.WithoutMappingAudit(t)
	// A 4 KiB pager, whichever page the rest of the suite is running: it is
	// the page there are millions of. The probe build keeps a history per page
	// on purpose, so this is a bound on the ordinary one.
	f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB, ResidentPages: residentHeapPages,
		LogicalPages: residentHeapPages, DirtyPages: 8, ReadAheadPages: 512})
	r, m, _ := f.memoryRegion(residentHeapPages)
	runtime.MemProfileRate = 64
	var before, after runtime.MemStats
	settle()
	runtime.ReadMemStats(&before)
	for page := uint64(0); page < residentHeapPages; page += 512 {
		access(t, r, m, page, false)
	}
	// The fixture's own record of the mapping is not the pager's, and a map
	// cleared keeps its capacity, so the record is dropped whole.
	m.pages = make(map[uint64]mapped)
	settle()
	runtime.ReadMemStats(&after)
	stats, err := f.h.Stats(t.Context())
	if err != nil || stats.ResidentPages != residentHeapPages {
		t.Fatalf("resident pages: %+v %v", stats, err)
	}
	// SPROUTFS_RESIDENT_HEAP_PROFILE names a file that receives the heap as
	// it stands here, which is what says whose each byte is.
	if path := os.Getenv("SPROUTFS_RESIDENT_HEAP_PROFILE"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.Lookup("heap").WriteTo(file, 0); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// The fixture's arena keeps each page's bytes on the heap too; a Linux
	// arena keeps them in its memfd, so they are not the pager's heap.
	perPage := (after.HeapAlloc-before.HeapAlloc)/residentHeapPages - checkpoint.PageSize4KiB
	t.Logf("%d bytes of heap per resident page", perPage)
	if perPage > residentHeapBytes {
		t.Fatalf("a resident page costs %d bytes of heap, want at most %d", perPage, residentHeapBytes)
	}
	runtime.KeepAlive(r)
}

// settle collects until the heap holds only what is reachable. The pager keeps
// its read-ahead buffers, 2 MiB each here, in a sync.Pool, and how many a run
// leaves there depends on which processor each fault ran on. One collection
// only moves a pool's contents to its victim cache, where they still count;
// the second frees them. The pool is a cache of scratch space, not what a
// resident page costs.
func settle() {
	runtime.GC()
	runtime.GC()
}
