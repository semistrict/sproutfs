package vmmemory_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// publishedIn is a backing that states the page size its volume is published
// in, which *volume.Volume does. A pager instance has one page and every number
// it faults, keys and maps is in it.
type publishedIn struct {
	vmmemory.Backing
	pageSize uint64
}

func (b publishedIn) PageSize() uint64 { return b.pageSize }

// A volume published in a page size other than this pager's is refused when it
// is attached: a page number of it means something else, so serving it would
// fault, key and map the wrong bytes. The refusal names both sizes, and it
// happens before any mapping is armed. It is also how a memory region reaches the
// wrong one of a host's two pagers, which is why both directions are here: the
// 4 KiB pager refuses a 2 MiB-page volume and the 2 MiB pager refuses a 4 KiB
// one, whichever page the suite is running.
func TestAttachRefusesAVolumeOfAnotherPageSize(t *testing.T) {
	f := newFixture(t, 4, 8, 4)
	own := f.h.PageSize()
	for _, stated := range []uint64{checkpoint.PageSize4KiB, 64 << 10, checkpoint.PageSize2MiB, 4 << 20} {
		if stated == own {
			continue
		}
		m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
		_, err := f.h.Attach(t.Context(), ram(publishedIn{Backing: f.newBacking(2), pageSize: stated}), m)
		if !errors.Is(err, vmmemory.ErrConfig) {
			t.Fatalf("attaching a volume of %d-byte pages to a %d-byte pager: %v", stated, own, err)
		}
		if got := err.Error(); !strings.Contains(got, fmt.Sprintf("this pager's page is %d", own)) ||
			!strings.Contains(got, fmt.Sprintf("published in %d-byte pages", stated)) {
			t.Fatalf("the refusal reads %q, want it to name both pages", got)
		}
		if len(m.pages) != 0 {
			t.Fatalf("a refused attach armed %d mappings", len(m.pages))
		}
	}
	// The pager's own page attaches, so what was refused is the geometry and
	// not the backing.
	f.attach(publishedIn{Backing: f.newBacking(2), pageSize: own})
}

// A pager cannot be built on a page no volume could be published in: the page
// is durable geometry, and checkpoint.GeometryFor is the one place that says
// which sizes exist.
func TestNewRefusesAPageNoVolumeCouldBePublishedIn(t *testing.T) {
	for _, size := range []uint64{0, 512, 8 << 10, 1 << 20, 4 << 20, 1 << 30} {
		if _, err := newBrokenFixture(t, vmmemory.Config{PageSize: size,
			ResidentPages: 2, LogicalPages: 4, DirtyPages: 2}); !errors.Is(err, vmmemory.ErrConfig) {
			t.Fatalf("a pager of %d-byte pages was built: %v", size, err)
		}
	}
}

// Every budget a pager keeps is in its own page: the spill file is its dirty
// pages times that page, a memory region is admitted in that page, and the read-ahead
// run a deployment states in bytes becomes that many of them. Two pagers of
// different pages given the same page counts therefore hold different amounts
// of memory and disk, which is the whole reason a host accounts in bytes.
func TestBudgetsAreCountedInThisPagersPage(t *testing.T) {
	f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
		ResidentPages: 8, LogicalPages: 64, DirtyPages: 8, ReadAheadPages: 4})
	if got := f.h.PageSize(); got != checkpoint.PageSize4KiB {
		t.Fatalf("the pager's page is %d", got)
	}
	// The spill file's whole extent is this pager's fixed disk cap: eight of
	// its own pages and not eight of the other's.
	if got := f.spillBytes(); got != 8*checkpoint.PageSize4KiB {
		t.Fatalf("the spill file is %d bytes, want %d", got, 8*checkpoint.PageSize4KiB)
	}
	// Read-ahead is in this pager's pages too: a four-page run over 4 KiB pages
	// serves 16 KiB, so a fault on page 0 of a memory region maps pages 0 to 3 and no
	// more.
	r, m, b := f.memoryRegion(16)
	access(t, r, m, 0, false)
	if len(m.pages) != 4 || b.loadedBytes != 4*checkpoint.PageSize4KiB {
		t.Fatalf("a four-page read-ahead run mapped %d pages and loaded %d bytes",
			len(m.pages), b.loadedBytes)
	}
	// The dirty budget is in pages of this pager, so eight stores fit and the
	// bytes they hold are eight 4 KiB pages.
	for page := range uint64(8) {
		access(t, r, m, page, true)[0] = byte(page + 1)
	}
	stats, err := r.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PrivatePages != 8 || stats.PrivateBytes() != 8*checkpoint.PageSize4KiB {
		t.Fatalf("eight stores hold %d pages and %d bytes, want 8 and %d",
			stats.PrivatePages, stats.PrivateBytes(), 8*checkpoint.PageSize4KiB)
	}
}

// The plan's first acceptance bullet, at the pager level: a memory region inherits a
// run of pages from a sibling, a store into one of them costs exactly one page
// of private backing, and the pages around it stay shared — before and after a
// checkpoint publishes it, across a spill and refault, and through a settle
// that finds a page unchanged.
func TestAStoreCostsOnePageAndLeavesItsNeighboursShared(t *testing.T) {
	const pages = 8
	f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
		ResidentPages: 2 * pages, LogicalPages: 8 * pages, DirtyPages: pages, ReadAheadPages: 1})
	page := uint64(checkpoint.PageSize4KiB)

	parent, parentMap, _ := f.memoryRegion(pages)
	child, childMap, childBacking := f.memoryRegion(pages)
	for i := range uint64(pages) {
		access(t, parent, parentMap, i, false)
		access(t, child, childMap, i, false)
	}
	// Every page is one resident page two memory regions map, so the arena holds the
	// run once.
	if shared := sharing(t, f).Ram; shared.UniqueBytes != pages*page || shared.MappedBytes != 2*pages*page {
		t.Fatalf("an inherited run holds %d unique and %d mapped bytes, want %d and %d",
			shared.UniqueBytes, shared.MappedBytes, pages*page, 2*pages*page)
	}

	// One store into one page. Exactly one page of private backing appears, and
	// the other seven stay the one copy both memory regions read.
	access(t, child, childMap, 3, true)[0] = 99
	assertOnePagePrivate := func(when string) {
		t.Helper()
		stats, err := child.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.PrivateBytes() != page {
			t.Fatalf("%s the child holds %d private bytes, want one %d-byte page", when, stats.PrivateBytes(), page)
		}
		if stats.SharedBytes() != (pages-1)*page {
			t.Fatalf("%s the child shares %d bytes, want the other %d pages", when, stats.SharedBytes(), pages-1)
		}
		if parentStats, err := parent.Stats(t.Context()); err != nil || parentStats.PrivateBytes() != 0 ||
			parentStats.ResidentBytes() != pages*page {
			t.Fatalf("%s the parent holds %+v (%v), want its whole run and nothing private", when, parentStats, err)
		}
		if unique := sharing(t, f).Ram.UniqueBytes; unique != (pages+1)*page {
			t.Fatalf("%s the arena holds %d bytes, want the run plus one copy, %d", when, unique, (pages+1)*page)
		}
	}
	assertOnePagePrivate("after the store")

	// Publishing it changes nothing about the neighbours: the store's page
	// becomes the child's own clean state under a new identity, and the other
	// seven are still the one copy.
	f.mustCheckpoint(child, childBacking)
	if stats, err := child.Stats(t.Context()); err != nil || stats.PrivateBytes() != 0 ||
		stats.SharedBytes() != (pages-1)*page {
		t.Fatalf("after the checkpoint the child holds %+v (%v), want nothing private and %d shared bytes",
			stats, err, (pages-1)*page)
	}

	// A spill and a refault of the stored page cost the same one page.
	access(t, child, childMap, 5, true)[0] = 17
	for i := range uint64(pages) {
		access(t, parent, parentMap, i, false)
	}
	if got := access(t, child, childMap, 5, false)[0]; got != 17 {
		t.Fatalf("the refaulted page reads %d, want 17", got)
	}
	if stats, err := child.Stats(t.Context()); err != nil || stats.PrivateBytes() != page {
		t.Fatalf("after a spill and refault the child holds %+v (%v), want one %d-byte page", stats, err, page)
	}

	// A write fault that stores nothing: the settle gives the page back to the
	// one the copy was made from, so the child ends up sharing the whole run
	// again and the checkpoint publishes nothing.
	f.mustCheckpoint(child, childBacking)
	if err := child.Fault(t.Context(), 6, true); err != nil {
		t.Fatal(err)
	}
	if err := child.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	if unchanged := f.settle(child); unchanged != 1 {
		t.Fatalf("the settle dropped %d pages, want the one the guest never stored into", unchanged)
	}
	if err := child.Checkpoint().Retire(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if stats, err := child.Stats(t.Context()); err != nil || stats.PrivateBytes() != 0 {
		t.Fatalf("after the settle the child holds %+v (%v), want nothing private", stats, err)
	}
	// The arena holds the inherited run once, plus the child's own copies of the
	// two pages it really stored into — and nothing for the page it merely took
	// writable, which the settle gave back.
	if unique := sharing(t, f).Ram.UniqueBytes; unique != (pages+2)*page {
		t.Fatalf("after the settle the arena holds %d bytes, want %d", unique, (pages+2)*page)
	}
}
