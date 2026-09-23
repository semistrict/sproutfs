//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"os"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// hugeBytes is the 2 MiB block an ordinary memfd's zero run is allocated in.
const hugeBytes = 2 << 20

// slotsPerHuge is how many 4 KiB arena offsets one block covers.
const slotsPerHuge = hugeBytes / checkpoint.PageSize4KiB

// hostShmemPolicy is the host's transparent-huge-page policy for shared memory.
// It is the host's setting and not the pager's: a host with it off skips with
// the setting it would need.
func hostShmemPolicy(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("/sys/kernel/mm/transparent_hugepage/shmem_enabled")
	if err != nil {
		t.Skipf("this kernel has no shmem transparent huge pages: %v", err)
	}
	for _, field := range strings.Fields(string(raw)) {
		if policy, ok := strings.CutPrefix(field, "["); ok {
			policy = strings.TrimSuffix(policy, "]")
			if policy == "never" || policy == "deny" {
				t.Skipf("shmem_enabled is %q; the host must set it to advise", strings.TrimSpace(string(raw)))
			}
			return policy
		}
	}
	t.Fatalf("shmem_enabled names no policy: %q", raw)
	return ""
}

func newHugeArena(t *testing.T) *vmmemory.LinuxArena {
	t.Helper()
	a, err := vmmemory.NewLinuxArena(8*slotsPerHuge, checkpoint.PageSize4KiB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func expectArena(t *testing.T, a *vmmemory.LinuxArena, allocated, huge uint64) {
	t.Helper()
	gotAllocated, err := a.AllocatedBytes()
	if err != nil {
		t.Fatal(err)
	}
	gotHuge, err := a.HugePageBytes()
	if err != nil {
		t.Fatal(err)
	}
	if gotAllocated != allocated || gotHuge != huge {
		t.Fatalf("the arena holds %d bytes of which %d are huge pages, want %d of which %d",
			gotAllocated, gotHuge, allocated, huge)
	}
}

// A zero run is the memory a fault hands a guest, and the kernel allocates and
// clears each whole 2 MiB block of it at once rather than 512 times over: an
// aligned 8 MiB run is four huge pages and exactly the 8 MiB the pager counted.
func TestZeroRunIsWholeHugePagesWhereItCoversWholeBlocks(t *testing.T) {
	policy := hostShmemPolicy(t)
	a := newHugeArena(t)
	if a.HugePolicy() != policy {
		t.Fatalf("the arena recorded the policy %q, want the host's %q", a.HugePolicy(), policy)
	}
	if err := a.Zero(t.Context(), slotsPerHuge, 4*slotsPerHuge); err != nil {
		t.Fatal(err)
	}
	expectArena(t, a, 4*hugeBytes, 4*hugeBytes)
}

// A run that is not aligned is huge only in the blocks it covers whole, and its
// ends are ordinary pages: the arena holds exactly the 8 MiB the pager asked
// for and counted, never the whole blocks its ends fall in.
func TestZeroRunEndsAreOrdinaryPages(t *testing.T) {
	hostShmemPolicy(t)
	a := newHugeArena(t)
	if err := a.Zero(t.Context(), slotsPerHuge+1, 4*slotsPerHuge); err != nil {
		t.Fatal(err)
	}
	expectArena(t, a, 4*hugeBytes, 3*hugeBytes)
}

// One page is one ordinary page, whatever the host's policy: a store's copy put
// into an empty block holds 4 KiB, not the 2 MiB block around it.
func TestOnePageIsOneOrdinaryPage(t *testing.T) {
	a := newHugeArena(t)
	page := make([]byte, checkpoint.PageSize4KiB)
	page[0] = 1
	if err := a.Write(t.Context(), 2*slotsPerHuge, page); err != nil {
		t.Fatal(err)
	}
	if err := a.Zero(t.Context(), 4*slotsPerHuge+9, 1); err != nil {
		t.Fatal(err)
	}
	expectArena(t, a, 2*checkpoint.PageSize4KiB, 0)
}

// Ownership stays 4 KiB: releasing one slot inside a huge page splits it and
// gives back that slot's 4 KiB and no more, and the rest of the block reads as
// it did.
func TestReleasingOneSlotSplitsItsHugePage(t *testing.T) {
	hostShmemPolicy(t)
	a := newHugeArena(t)
	if err := a.Zero(t.Context(), slotsPerHuge, 4*slotsPerHuge); err != nil {
		t.Fatal(err)
	}
	page := make([]byte, checkpoint.PageSize4KiB)
	page[7] = 7
	if err := a.Write(t.Context(), 2*slotsPerHuge+8, page); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(t.Context(), 2*slotsPerHuge+7); err != nil {
		t.Fatal(err)
	}
	expectArena(t, a, 4*hugeBytes-checkpoint.PageSize4KiB, 3*hugeBytes)
	got := make([]byte, checkpoint.PageSize4KiB)
	if err := a.Read(t.Context(), 2*slotsPerHuge+8, got); err != nil {
		t.Fatal(err)
	}
	if got[7] != 7 {
		t.Fatalf("the page beside the released one reads %d at byte 7, want 7", got[7])
	}
}
