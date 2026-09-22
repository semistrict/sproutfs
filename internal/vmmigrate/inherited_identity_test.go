package vmmigrate_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
)

// A post-copy destination reports no identity for the pages its handoff named,
// so the pager loads them through this backing — where the source answers —
// rather than resolving them against a checkpoint that does not hold them. That
// is right for exactly as long as no checkpoint holds them.
//
// Once this VM's own checkpoint publishes such a page, the volume names it, and
// that identity is the truth about where the page's bytes are. Going on
// stripping it tells the pager that a page it has just published has no object,
// and the pager's retire then throws the guest's only copy of those bytes away:
// a guest reading an older version of memory it wrote.
func TestLocateReportsAPageThisVMHasPublishedItself(t *testing.T) {
	s := newServed(t, nil, 4)
	backing := s.unpublishedBacking(t, nil, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}})

	// Before any checkpoint of this VM: every page the handoff named is the
	// source's alone, and none of them may be resolved by identity.
	for _, extent := range locateRAM(t, backing, 4) {
		if !extent.Identity.Ref.IsZero() {
			t.Fatalf("a page only the source holds reports identity %+v", extent.Identity)
		}
	}

	// This VM publishes them itself, which is what a child's root index does.
	if err := s.machine.checkpoint(t.Context(), s.vm); err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, extent := range locateRAM(t, backing, 4) {
		if extent.Identity.Ref.VM == "vm-2" {
			named += int(extent.Length / pageSize)
			continue
		}
		if !extent.Identity.Ref.IsZero() {
			t.Fatalf("a published page reports somebody else's checkpoint %+v", extent.Identity)
		}
	}
	if named != 4 {
		t.Fatalf("%d of 4 pages this VM published report its own identity, want all of them: "+
			"the pager is told a page it has published has no object, and its retire drops the "+
			"guest's only copy of those bytes", named)
	}
}

func locateRAM(t *testing.T, backing *vmmigrate.PeerBacking, pages uint64) []control.Extent {
	t.Helper()
	extents, err := backing.Locate(t.Context(), 0, pages*pageSize)
	if err != nil {
		t.Fatal(err)
	}
	return extents
}
