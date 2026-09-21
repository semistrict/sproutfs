//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// A RAM pager runs 4 KiB on a real host, and what that buys is the contract
// this test pins through the real arena, the real transport and the real
// kernel: a fork inherits a contiguous 2 MiB run of its parent's RAM as one
// mapping command rather than one per page, and a store into one page of that
// run takes exactly 4 KiB of private backing and leaves the other 511 shared.
//
// It is the page-geometry plan's first two acceptance bullets at the pager
// level. The simulated pager asserts the same thing without a kernel; this is
// where the mapping is a real VMA and the private page is a real host page.
func TestManagedPagerSmallRAMPageOwnsOnePageAndMapsARunAtOnce(t *testing.T) {
	// One read-ahead run is 512 pages of 4 KiB, which is the 2 MiB the plan
	// targets, and the volumes are exactly that run.
	const pages = 512
	const size = checkpoint.PageSize4KiB
	h := kernelHostConfigured(t, vmmemory.Config{PageSize: size,
		ResidentPages: 4 * pages, LogicalPages: 16 * pages, DirtyPages: 2 * pages,
		ReadAheadPages: pages, WriteAheadPages: 1})
	c := newPagerCluster(t)
	names := []string{"pmem0", "ram0"}
	specs := []volume.VolumeSpec{
		{Name: names[0], Size: pages * size, PageSize: size},
		{Name: names[1], Size: pages * size, PageSize: size},
	}
	source, err := c.manager.Create(t.Context(), "source", specs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	// Every page gets bytes of its own and one published identity, so an
	// inherited page is shared by name and a copy is visible as a copy. No page
	// is all zeros: a checkpoint publishes one of those as a sparse hole, which
	// owns no arena slot and so is not a page a run of shared memory is made of.
	for region, name := range names {
		data := make([]byte, pages*size)
		for i := range data {
			data[i] = pageByte(region, i/size)
		}
		if err := source.Volume(name).Write(t.Context(), 0, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	point, err := source.ForkPoint(t.Context(), volume.Prepared(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	fork := func(id string) []vmmemory.Backing {
		vm, err := c.manager.Fork(t.Context(), id, point)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vm.Close(context.Background()) })
		var backings []vmmemory.Backing
		for _, name := range names {
			backings = append(backings, vm.Volume(name))
		}
		return backings
	}
	config := vmmemory.ConnectionConfig{QueuePages: pages, CommandTimeout: 30 * time.Second,
		VerifyInterval: time.Hour}

	// The parent reads one byte of each region. Read-ahead serves the whole run
	// from one fault, loading it into consecutive arena slots, which is what
	// lets a single command install it here and on every child after.
	parent := startNativeWithConfig(t, h, pages, config, fork("parent")...)
	for region := range names {
		parent.request(fmt.Sprintf("read %d 0 1", region), fmt.Sprintf("data %02x", pageByte(region, 0)))
	}
	warm := kernelStats(t, h)
	if warm.ResidentPages != 2*pages {
		t.Fatalf("the parent holds %d resident pages after reading both runs, want %d",
			warm.ResidentPages, 2*pages)
	}

	// The child inherits them. Its attachment maps every page it shares before
	// its first access, and a contiguous run of 512 pages in consecutive slots
	// is one command, not 512.
	child := startNativeWithConfig(t, h, pages, config, fork("child")...)
	attached := kernelStats(t, h)
	mapped := attached.MappedPages - warm.MappedPages
	commands := attached.Mappings - warm.Mappings
	if mapped != 2*pages {
		t.Fatalf("the child mapped %d inherited pages, want %d", mapped, 2*pages)
	}
	if commands != 2 {
		t.Fatalf("the child installed %d contiguous runs of %d pages with %d mapping commands, want one each",
			len(names), pages, commands)
	}
	if loads := attached.Loads - warm.Loads; loads != 0 {
		t.Fatalf("the child read its volumes %d times for pages its parent holds", loads)
	}

	// One byte, one page. The child's store copies exactly one 4 KiB page: its
	// own private bytes are that page and nothing more, the other 511 of the run
	// stay shared with the parent, and the parent's own memory is untouched.
	const stored = 100
	ram := child.region(1)
	parentRAM := parent.region(1)
	beforeChild, beforeParent := regionBytes(t, ram), regionBytes(t, parentRAM)
	beforeShared, err := h.Sharing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if beforeChild.PrivatePages != 0 || beforeChild.SharedPages != pages {
		t.Fatalf("before the store the child holds %+v, want no private page and %d shared",
			beforeChild, pages)
	}
	child.request(fmt.Sprintf("fill 1 %d 1 7", stored*size), "filled")
	afterChild, afterParent := regionBytes(t, ram), regionBytes(t, parentRAM)
	if afterChild.PrivatePages != 1 {
		t.Fatalf("one byte stored made %d pages private, want exactly one", afterChild.PrivatePages)
	}
	if got := uint64(afterChild.PrivatePages) * afterChild.PageSize; got != size {
		t.Fatalf("one byte stored holds %d private bytes, want %d", got, size)
	}
	if afterChild.SharedPages != pages-1 {
		t.Fatalf("after the store the child shares %d pages of its run, want %d",
			afterChild.SharedPages, pages-1)
	}
	// The parent keeps every page it had and owns none of them privately: the
	// copy is the child's. What does change is how many of the parent's pages
	// something else maps, and it changes by exactly the one page the child took
	// away, which is the same fact read from the other side.
	if afterParent.ResidentPages != beforeParent.ResidentPages ||
		afterParent.PrivatePages != 0 || beforeParent.PrivatePages != 0 {
		t.Fatalf("the child's store changed the parent's memory: %+v then %+v",
			beforeParent, afterParent)
	}
	if afterParent.SharedPages != beforeParent.SharedPages-1 {
		t.Fatalf("the parent shares %d pages after the child copied one, want %d",
			afterParent.SharedPages, beforeParent.SharedPages-1)
	}
	afterShared, err := h.Sharing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if grew := afterShared.Ram.UniqueBytes - beforeShared.Ram.UniqueBytes; grew != size {
		t.Fatalf("one byte stored cost the host %d bytes of arena, want %d", grew, size)
	}

	// The bytes themselves: the child reads what it stored, the parent reads
	// what it always had, and the child's neighbouring pages are unchanged.
	child.request(fmt.Sprintf("read 1 %d 1", stored*size), "data 07")
	parent.request(fmt.Sprintf("read 1 %d 1", stored*size), fmt.Sprintf("data %02x", pageByte(1, stored)))
	child.request(fmt.Sprintf("read 1 %d 1", (stored+1)*size), fmt.Sprintf("data %02x", pageByte(1, stored+1)))
}

// pageByte is the byte every offset of one page of one region holds. It is
// never zero, so no page of these volumes is published as a hole.
func pageByte(region, page int) byte { return byte(1 + (region*32+page)%251) }

// regionBytes is one region's statistics, which is where a store's cost in
// bytes is read from.
func regionBytes(t *testing.T, r *vmmemory.Region) vmmemory.RegionStats {
	t.Helper()
	stats, err := r.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}
