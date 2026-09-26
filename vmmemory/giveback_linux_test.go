//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"fmt"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// giveBackPair is two client processes whose RAM inherits one checkpoint in
// pages of size, both reading page 0 so that they map one physical page.
func giveBackPair(t *testing.T, size uint64) (*vmmemory.Host, *nativeProcess, *nativeProcess) {
	t.Helper()
	const pages = 2
	h := kernelHostPaged(t, int(size), 8, 4*pages, 4*pages)
	backings := func() []vmmemory.Backing {
		var result []vmmemory.Backing
		for region := range 2 {
			b := newPagedKernelBacking(byte(region+1), pages*int(size), size)
			for j := range b.data {
				b.data[j] = byte(1 + region*32 + j/int(size))
			}
			result = append(result, b)
		}
		return result
	}
	a, b := startNative(t, h, pages, backings()...), startNative(t, h, pages, backings()...)
	a.request("read 1 0 1", "data 21")
	b.request("read 1 0 1", "data 21")
	if a.pfn(1) != b.pfn(1) {
		t.Fatal("the two processes do not share the page they both read")
	}
	return h, a, b
}

// A store of the byte the page already holds is a write fault that changes
// nothing, which is what a cold read is where KVM asks for every page
// writable: the pager copies. The give-back hands the copy back with the
// process running, points it at the page it shares with its sibling in place,
// and installs that page. So the next read, by the process and through KVM,
// is served by the page tables and reaches the pager not at all.
func TestManagedPagerGivesBackAnUnchangedCopyInPlace(t *testing.T) {
	for _, size := range []uint64{hugePageSize, checkpoint.PageSize4KiB} {
		t.Run(fmt.Sprintf("%dKiB", size>>10), func(t *testing.T) {
			h, a, b := giveBackPair(t, size)
			a.request("fill 1 0 1 33", "filled")
			if a.pfn(1) == b.pfn(1) {
				t.Fatal("the write fault left the process on the page it shares")
			}
			given, err := a.memoryRegion(1).GiveBack(t.Context(), 16)
			if err != nil || given != 1 {
				t.Fatalf("the give-back gave back %d pages: %v; want exactly one", given, err)
			}
			// pfn fails on a page with no page table entry, so this is also the
			// evidence that the page was installed rather than revoked.
			if a.pfn(1) != b.pfn(1) {
				t.Fatal("the process does not map its sibling's physical page again")
			}
			before := kernelStats(t, h)
			a.request("read 1 0 1", "data 21")
			a.request("kvmread 1 0", "kvm 33")
			after := kernelStats(t, h)
			if after.Faults != before.Faults || after.CopyOnWrites != before.CopyOnWrites {
				t.Fatalf("reading the page given back took %d faults and made %d copies, want none",
					after.Faults-before.Faults, after.CopyOnWrites-before.CopyOnWrites)
			}
			if after.GivenBackPages != 1 || after.DirtyPages != 0 {
				t.Fatalf("gave back %d pages with %d dirty reservations held, want 1 and 0",
					after.GivenBackPages, after.DirtyPages)
			}
			// A real store after it copies again and is kept.
			a.request("fill 1 0 1 34", "filled")
			a.request("read 1 0 1", "data 22")
			b.request("read 1 0 1", "data 21")
		})
	}
}

// A copy the guest changed is kept, and its write-protection comes off in
// place: the next store lands through the page tables without a fault.
func TestManagedPagerKeepsAChangedCopyWritable(t *testing.T) {
	for _, size := range []uint64{hugePageSize, checkpoint.PageSize4KiB} {
		t.Run(fmt.Sprintf("%dKiB", size>>10), func(t *testing.T) {
			h, a, b := giveBackPair(t, size)
			a.request("fill 1 0 1 70", "filled")
			copied := a.pfn(1)
			given, err := a.memoryRegion(1).GiveBack(t.Context(), 16)
			if err != nil || given != 0 {
				t.Fatalf("the give-back gave back %d pages the process stored into: %v; want none", given, err)
			}
			before := kernelStats(t, h)
			a.request("fill 1 0 1 71", "filled")
			a.request("kvmwrite 1 1 72", "kvm 72")
			after := kernelStats(t, h)
			if after.Faults != before.Faults {
				t.Fatalf("storing into the kept copy took %d faults, want none", after.Faults-before.Faults)
			}
			if got := a.pfn(1); got != copied {
				t.Fatalf("the process maps physical page %d, want the copy %d it kept", got, copied)
			}
			if after.GiveBackCompares != 1 || after.GivenBackPages != 0 {
				t.Fatalf("compared %d and gave back %d, want 1 and 0", after.GiveBackCompares, after.GivenBackPages)
			}
			a.request("read 1 0 2", "data 4748")
			b.request("read 1 0 2", "data 2121")
		})
	}
}
