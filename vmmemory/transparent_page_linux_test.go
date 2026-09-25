//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"os"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
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

// newHugeArena makes an arena of 4 KiB pages and the one file of it every test
// here works in.
func newHugeArena(t *testing.T) (*vmmemory.LinuxArena, *vmmemory.LinuxFile) {
	t.Helper()
	arena, err := vmmemory.NewLinuxArena(checkpoint.PageSize4KiB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := arena.Close(); err != nil {
			t.Error(err)
		}
	})
	a, err := arena.NewFile(8 * slotsPerHuge)
	if err != nil {
		t.Fatal(err)
	}
	return arena, a
}

func expectArena(t *testing.T, a *vmmemory.LinuxFile, allocated, huge uint64) {
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
	arena, a := newHugeArena(t)
	if arena.HugePolicy() != policy {
		t.Fatalf("the arena recorded the policy %q, want the host's %q", arena.HugePolicy(), policy)
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
	_, a := newHugeArena(t)
	if err := a.Zero(t.Context(), slotsPerHuge+1, 4*slotsPerHuge); err != nil {
		t.Fatal(err)
	}
	expectArena(t, a, 4*hugeBytes, 3*hugeBytes)
}

// One page is one ordinary page, whatever the host's policy: a store's copy put
// into an empty block holds 4 KiB, not the 2 MiB block around it.
func TestOnePageIsOneOrdinaryPage(t *testing.T) {
	_, a := newHugeArena(t)
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
	_, a := newHugeArena(t)
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

// BenchmarkZeroRun is what a boot's write-ahead run costs the arena: 8 MiB of
// fresh zeros allocated, then punched back out for the next one. Aligned, the
// run is four huge pages where the host allows them; shifted by a page, it is
// three and two ends of ordinary pages.
func BenchmarkZeroRun(b *testing.B) {
	for _, shift := range []int{0, 1} {
		b.Run(map[int]string{0: "aligned", 1: "shifted"}[shift], func(b *testing.B) {
			arena, err := vmmemory.NewLinuxArena(checkpoint.PageSize4KiB)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = arena.Close() }()
			a, err := arena.NewFile(8 * slotsPerHuge)
			if err != nil {
				b.Fatal(err)
			}
			const run = 4 * slotsPerHuge
			b.SetBytes(run * checkpoint.PageSize4KiB)
			for b.Loop() {
				if err := a.Zero(b.Context(), slotsPerHuge+shift, run); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				for s := slotsPerHuge + shift; s < slotsPerHuge+shift+run; s++ {
					if err := a.Release(b.Context(), s); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
			}
		})
	}
}
