package vmmigrate_test

import (
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// A post-copy destination reports no identity for the pages whose only copy is
// its source's, so the pager loads them through this backing — where the source
// answers — rather than resolving them against a checkpoint that does not hold
// them.
//
// Until this host takes such a page, whatever the volume names for it is what
// the guest wrote past: a migration's own checkpoint from before the handoff,
// the parent's checkpoint a fork's child inherited, or a hole where either had
// nothing. Once this host has taken it, the page is here, and the next thing the
// volume names for it is this VM's own publication of it: a checkpoint, or a
// hole for a page of zeros. Going on stripping it tells the pager a page it has
// just published has no object, and the pager's retire then gives up the
// guest's only copy of those bytes; a load that goes on asking the source for
// it hands the guest the version it wrote past.
//
// One rule serves every case: the source's until this host takes it.

// A migration keeps the same VM, so the checkpoint its volume names for an
// unpublished page is this VM's own — and it is older than the page. It stays
// stripped until this host has the page.
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

// A fork's child takes every page it inherited from its parent and publishes
// them itself within a second of starting, and from then on the identities the
// volume names for them are the truth about where the bytes are.
func TestLocateReportsAPageThisVMHasPublishedItself(t *testing.T) {
	s := newServed(t, nil, 4)
	backing := s.unpublishedBacking(t, nil, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}})

	// Before this host takes them: every page the handoff named is the source's
	// alone, and none of them may be resolved by identity.
	for _, extent := range locateRAM(t, backing, 4) {
		if !extent.Identity.Ref.IsZero() {
			t.Fatalf("a page only the source holds reports identity %+v", extent.Identity)
		}
	}

	// This host takes them, and this VM publishes them itself, which is what a
	// child's root index does.
	backing.InstalledUnpublished(0, []bool{true, true, true, true})
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

// A hole says nothing about when it was written: it is what the volume names
// for a page no checkpoint had, and it is what this VM's own checkpoint writes
// for a page of zeros. So its age cannot be what decides. A page this host has
// taken is reported as whatever the volume names, a hole included, and the
// pager maps it as zeros rather than asking a source that holds the version
// the guest zeroed.
func TestLocateReportsTheHoleOfAPageThisHostTook(t *testing.T) {
	s := newServed(t, nil, 4)
	// Pages 4 to 7 were never written, so the volume names holes for them.
	backing := s.unpublishedBacking(t, nil, "ram0", []vmmigrate.PageRun{{First: 4, Count: 4}})
	for _, extent := range locateRange(t, backing, 4, 4) {
		if !extent.Identity.Ref.IsZero() || extent.Identity.Zero {
			t.Fatalf("a page only the source holds reports %+v, want no identity at all", extent.Identity)
		}
	}
	backing.InstalledUnpublished(4*pageSize, []bool{true, true, true, true})
	for _, extent := range locateRange(t, backing, 4, 4) {
		if extent.Identity != control.ZeroIdentity {
			t.Fatalf("a page this host took reports %+v, want the hole the volume names", extent.Identity)
		}
	}
}

// A load of a page this VM has published since the handoff is the volume's to
// answer. The source holds at best the version before it — a fork's parent the
// pause it was sealed at, a migration's source the page it stopped with — and
// answering from there hands the guest a page it has written past. So the
// source is not asked at all.
func TestALoadOfAPageThisVMPublishedIsTheVolumes(t *testing.T) {
	s := newServed(t, nil, 4)
	backing := s.unpublishedBacking(t, nil, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}})
	backing.InstalledUnpublished(0, []bool{true, true, true, true})
	if err := s.machine.checkpoint(t.Context(), s.vm); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4*pageSize)
	if err := backing.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if stats := backing.Stats(); stats.Requests != 0 || stats.PeerPages != 0 || stats.VolumePages != 4 {
		t.Fatalf("a load of pages this VM published made %d requests for %d peer pages and read "+
			"%d from the volume, want none, none and 4", stats.Requests, stats.PeerPages, stats.VolumePages)
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
	return locateRange(t, backing, 0, pages)
}

func locateRange(t *testing.T, backing *vmmigrate.PeerBacking, first, pages uint64) []control.Extent {
	t.Helper()
	extents, err := backing.Locate(t.Context(), first*pageSize, pages*pageSize)
	if err != nil {
		t.Fatal(err)
	}
	return extents
}
