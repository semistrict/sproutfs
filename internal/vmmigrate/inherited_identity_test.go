package vmmigrate_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
)

// A post-copy destination reports no identity for the pages its handoff named,
// so the pager loads them through this backing — where the source answers —
// rather than resolving them against a checkpoint that does not hold them.
//
// The two handoffs put opposite demands on that. A migration's unpublished pages
// are the guest's writes since its OWN last checkpoint, and the volume names
// that very checkpoint for them: resolving one would hand the guest bytes from
// before its own write. A fork child's are its parent's, and the child publishes
// them itself within a second of starting: going on stripping them tells the
// pager a page it has just published has no object, and the pager's retire then
// gives up the guest's only copy of those bytes.
//
// One rule serves both, and it is about which checkpoint rather than which VM:
// strip while the checkpoint the volume names predates the handoff.

// A migration keeps the same VM, so the checkpoint its volume names for an
// unpublished page is this VM's own — and it is older than the page. It stays
// stripped.
func TestLocateStripsThisVMsOwnCheckpointFromBeforeTheHandoff(t *testing.T) {
	s := newServed(t, nil, 4)
	// The checkpoint a migration's source selected when it gave the VM up. Its
	// pages are the volume's from here, and the guest's writes since it are what
	// the handoff names.
	if err := s.machine.checkpoint(t.Context(), s.vm); err != nil {
		t.Fatal(err)
	}
	selected := selectedSequence(t, s, 4)
	backing := s.unpublishedSince(t, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}}, selected)

	for _, extent := range locateRAM(t, backing, 4) {
		if !extent.Identity.Ref.IsZero() {
			t.Fatalf("a page the guest wrote past reports identity %+v, which is the checkpoint "+
				"it wrote past: resolving it would rewind the guest", extent.Identity)
		}
	}
}

// A fork child's handoff selected nothing of its own, so every checkpoint that
// names its pages afterwards is one it published itself, and those identities
// are the truth about where the bytes are.
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

// selectedSequence is the checkpoint the volume itself names for these pages,
// which for a migration is what the handoff selected.
func selectedSequence(t *testing.T, s *served, pages uint64) uint64 {
	t.Helper()
	extents, err := s.vm.Volume("ram0").Locate(t.Context(), 0, pages*pageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(extents) == 0 || extents[0].Identity.Ref.VM != "vm-2" {
		t.Fatalf("the volume names %+v for its own published pages", extents)
	}
	return extents[0].Identity.Ref.Sequence
}

func locateRAM(t *testing.T, backing *vmmigrate.PeerBacking, pages uint64) []control.Extent {
	t.Helper()
	extents, err := backing.Locate(t.Context(), 0, pages*pageSize)
	if err != nil {
		t.Fatal(err)
	}
	return extents
}
