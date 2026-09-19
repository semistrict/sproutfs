package vmmemory_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// The pre-mortem of the GCE soak's stops and starts. A round stops a VM and
// starts it on the other host; a later round stops it there and starts it back
// on the first, whose pager still holds the pages of its earlier incarnation
// under the identities that incarnation's checkpoints gave them. A start on
// such a host must map a page only where the identity the volume now reports
// is the one the page holds, and load everything a writer elsewhere has
// republished since.

// TestPremortemAStartOnAHostThatStillHoldsItsPagesMapsOnlyWhatIsStillItsOwn
// walks one VM through a stop and a start on one pager, with a fork of it still
// running there: the child holds the pages both inherited, so what the host
// still has of the stopped VM is exactly those pages. A writer
// elsewhere republishes half of them while the VM is away, so half the
// identities the volume reports have changed and half have not. The start back
// on this host must map the child's pages for the unchanged half without
// loading anything, and read the changed half rather than the bytes the pages
// it can still reach hold.
func TestPremortemAStartOnAHostThatStillHoldsItsPagesMapsOnlyWhatIsStillItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 4
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2 * pages,
			LogicalPages: 8 * pages, DirtyPages: pages, ReadAheadPages: 1})
		// The identity of each page, which is the whole of what a pager shares
		// pages by: what this host's incarnation published, and what a writer
		// on the other host published for the half it rewrote.
		kept := control.Identity{Ref: f.source, Volume: "v"}
		moved := control.Identity{Ref: control.Ref{VM: "other-host-writer", Sequence: 7}, Volume: "v"}
		held := make([]control.Identity, pages)
		for page := range held {
			held[page] = kept
		}
		before := &identifiedBacking{f.newBacking(pages), held}
		first, firstMap := f.attach(before)
		for page := range uint64(pages) {
			if got := access(t, first, firstMap, page, false)[0]; got != byte(page+1) {
				t.Fatalf("page %d of the first incarnation reads %d", page, got)
			}
		}
		if before.loads != pages {
			t.Fatalf("the first incarnation loaded %d pages, want each of its %d once",
				before.loads, pages)
		}
		// A fork of this VM, taken on this host in an earlier round and still
		// running: it inherited every page, so it shares the pages by identity
		// and they are what the host still holds once the parent stops.
		child := &identifiedBacking{f.newBacking(pages), held}
		childRegion, childMap := f.attach(child)
		for page := range uint64(pages) {
			access(t, childRegion, childMap, page, false)
		}
		if child.loads != 0 {
			t.Fatalf("the child loaded %d pages its parent already held", child.loads)
		}

		// The stop: the process closes and the region detaches, which gives its
		// logical pages and its own pages back. What this host still holds of
		// the VM is the pages the child shares.
		clear(firstMap.pages)
		if err := first.Detach(context.Background()); err != nil {
			t.Fatal(err)
		}

		// A writer on the other host republishes the second half of the volume
		// and leaves the first half alone, which is what a round that mutated
		// the guest there does.
		next := make([]control.Identity, pages)
		copy(next, held)
		restarted := f.newBacking(pages)
		for page := uint64(pages / 2); page < pages; page++ {
			next[page] = moved
			data := make([]byte, f.pageSize)
			for index := range data {
				data[index] = byte(100 + page)
			}
			restarted.write(page*uint64(f.pageSize), data)
		}
		after := &identifiedBacking{restarted, next}

		// The start back on this host.
		second, secondMap := f.attach(after)
		for page := range uint64(pages) {
			want := byte(page + 1)
			if page >= pages/2 {
				want = byte(100 + page)
			}
			if got := access(t, second, secondMap, page, false)[0]; got != want {
				t.Fatalf("page %d of the restarted VM reads %d, want %d", page, got, want)
			}
		}
		// The half nothing rewrote keeps the identity the pages here hold, so
		// it is mapped rather than read; the half a writer elsewhere published
		// has an identity this host has never seen and must be read.
		if after.loads != pages/2 {
			t.Fatalf("the restarted VM loaded %d pages, want the %d a writer elsewhere republished",
				after.loads, pages/2)
		}
	})
}

// TestPremortemAPageOfAnEarlierIncarnationIsNeverServedForANewIdentity is the
// same host and the same VM, where every page was republished while it ran
// elsewhere: not one of the pages this host still holds is that VM's any more,
// so not one of them may be mapped for it and every page must be read.
func TestPremortemAPageOfAnEarlierIncarnationIsNeverServedForANewIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 3
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2 * pages,
			LogicalPages: 8 * pages, DirtyPages: pages, ReadAheadPages: 1})
		before := f.newBacking(pages)
		first, firstMap := f.attach(before)
		for page := range uint64(pages) {
			access(t, first, firstMap, page, false)
		}
		// A fork of it taken on this host and still running, which is what keeps
		// the earlier incarnation's pages here after the stop.
		sibling := f.newBacking(pages)
		siblingRegion, siblingMap := f.attach(sibling)
		for page := range uint64(pages) {
			access(t, siblingRegion, siblingMap, page, false)
		}
		if sibling.loads != 0 {
			t.Fatalf("the sibling loaded %d pages this host already held", sibling.loads)
		}
		clear(firstMap.pages)
		if err := first.Detach(context.Background()); err != nil {
			t.Fatal(err)
		}

		after := f.newBacking(pages)
		// Every page of the volume is a writer's elsewhere now, published under
		// an epoch this host never wrote in: the same VM, a later incarnation,
		// and nothing of what this host holds belongs to it.
		after.source = control.Ref{VM: before.owner, Sequence: 4242}
		for page := range uint64(pages) {
			data := make([]byte, f.pageSize)
			for index := range data {
				data[index] = byte(200 + page)
			}
			after.write(page*uint64(f.pageSize), data)
		}
		second, secondMap := f.attach(after)
		for page := range uint64(pages) {
			if got := access(t, second, secondMap, page, false)[0]; got != byte(200+page) {
				t.Fatalf("page %d was served a page of an earlier incarnation: it reads %d, want %d",
					page, got, byte(200+page))
			}
		}
		if after.loads != pages {
			t.Fatalf("the restarted VM loaded %d of its %d pages: a page of an earlier incarnation was mapped for it",
				after.loads, pages)
		}
	})
}
