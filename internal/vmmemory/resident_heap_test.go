//go:build !sproutfsprobe

package vmmemory_test

import (
	"os"
	"runtime"
	"runtime/pprof"
	"testing"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// residentHeapPages is how many pages the measurement below holds resident:
// enough that what each costs dominates everything the fixture itself holds.
const residentHeapPages = 1 << 16

// residentHeapBytes bounds what one resident page costs the host's heap. It
// measured 923 bytes with a map of aliases and a list element per page, and 731
// with one alias inline and the lists' links in the page: the rest is the page
// struct, its lock, its entry in the sharing index and its binding.
const residentHeapBytes = 768

// What the pager's own heap costs per page it holds resident, beyond the page
// itself, which is in the arena. A 4 KiB pager's arena of 24 GiB is six million
// pages, so every hundred bytes here is 600 MB of the host's heap.
func TestAResidentPageCostsLittleHeap(t *testing.T) {
	// A 4 KiB pager, whichever page the rest of the suite is running: it is
	// the page there are millions of. The probe build keeps a history per page
	// on purpose, so this is a bound on the ordinary one.
	f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB, ResidentPages: residentHeapPages,
		LogicalPages: residentHeapPages, DirtyPages: 8, ReadAheadPages: 512})
	r, m, _ := f.region(residentHeapPages)
	runtime.MemProfileRate = 64
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for page := uint64(0); page < residentHeapPages; page += 512 {
		access(t, r, m, page, false)
	}
	clear(m.pages) // the fixture's own record of the mapping is not the pager's
	runtime.GC()
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
