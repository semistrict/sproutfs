//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// measureBacking is a volume with no bytes of its own: loads read zeros and
// writes are discarded. A seal measurement is about page-table work, so the
// fixture must not spend gigabytes modelling contents it never checks.
type measureBacking struct {
	pages int
	owner string
}

func (b *measureBacking) Size() uint64 { return uint64(b.pages) * uint64(os.Getpagesize()) }
func (b *measureBacking) Load(_ context.Context, _ uint64, dst []byte) error {
	clear(dst)
	return nil
}

// Every page reports a published identity rather than a hole, so a store copies on write
// exactly as it does for a loaded page and nothing is mapped as a hole.
func (b *measureBacking) Locate(_ context.Context, off, length uint64) ([]control.Extent, error) {
	return []control.Extent{{Offset: off, Length: length,
		Identity: control.Identity{Ref: control.Ref{VM: b.owner, Sequence: 1}, Volume: "v", Page: off / checkpoint.PageSize2MiB}}}, nil
}
func (b *measureBacking) Verify(context.Context) error { return nil }

// Seal time is what a capture's pause pays for its dirty set, so it is
// measured over the dirty page counts a capture actually meets, and with the
// pages both contiguous and scattered: a run of consecutive pages whose pages
// are not consecutive is what a real guest's dirty set looks like. The pause
// and the walk behind it are two numbers, because only the first is time the
// guest is stopped for: `seal_ns` is the write-protect commands and
// `seal_walk_ns` the pages, which move into the checkpoint with the guest
// running. Timings are observations, never correctness thresholds. Run through
// the Linux qualification script with SPROUTFS_PAGER_MEASURE=1.
// fillPages bounds one guest store request, so a large dirty set is created in
// several requests rather than one the fixture would wait out.
const fillPages = 4096

func TestManagedPagerSealCostMeasurements(t *testing.T) {
	if os.Getenv("SPROUTFS_PAGER_MEASURE") == "" {
		t.Skip("set SPROUTFS_PAGER_MEASURE=1")
	}
	for _, c := range []struct {
		name       string
		dirty, gap int
	}{
		{"8k-contiguous", 8192, 1},
		{"8k-scattered", 8192, 2},
		{"200k-contiguous", 200000, 1},
		{"200k-scattered", 200000, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			size := os.Getpagesize()
			pages := c.dirty * c.gap
			h := kernelHostBudget(t, c.dirty, 2*pages, c.dirty)
			backing := []vmmemory.Backing{
				&measureBacking{pages: pages, owner: c.name + "-pmem"},
				&measureBacking{pages: pages, owner: c.name + "-ram"},
			}
			p := startNativeWithConfig(t, h, pages, vmmemory.ConnectionConfig{
				QueuePages: 1024, CommandTimeout: 10 * time.Minute, VerifyInterval: time.Hour}, backing...)
			memoryRegion := p.memoryRegion(1)
			start := time.Now()
			// The guest dirties in bounded pieces: one request per fault is the
			// harness's own cost, and a single request for a large memory region would
			// outrun the fixture's output wait rather than the pager.
			for first := 0; first < pages; first += fillPages {
				run := min(fillPages, pages-first)
				p.request(fmt.Sprintf("stridefill 1 %d %d %d 7", first*size, run*size, c.gap*size), "strided")
			}
			dirtyNS := time.Since(start).Nanoseconds()
			before, err := h.Stats(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if before.DirtyPages != c.dirty {
				t.Fatalf("the guest dirtied %d pages, want %d", before.DirtyPages, c.dirty)
			}
			start = time.Now()
			if err := memoryRegion.Seal(t.Context()); err != nil {
				t.Fatal(err)
			}
			sealNS := time.Since(start).Nanoseconds()
			// The pause is over; the walk that moves each page into the
			// checkpoint runs behind it, and asking the checkpoint what it holds
			// is what waits for that walk.
			if got := len(memoryRegion.Checkpoint().DirtyPages()); got != c.dirty {
				t.Fatalf("the seal took %d pages into the checkpoint, want %d", got, c.dirty)
			}
			walkNS := time.Since(start).Nanoseconds() - sealNS
			after, err := h.Stats(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if after.CheckpointPages-before.CheckpointPages != uint64(c.dirty) {
				t.Fatalf("the seal checkpoint %d pages, want %d", after.CheckpointPages-before.CheckpointPages, c.dirty)
			}
			raw, err := json.Marshal(map[string]any{
				"case": c.name, "dirty_pages": c.dirty, "page_gap": c.gap,
				"dirty_ns": dirtyNS, "dirty_ns_per_page": dirtyNS / int64(c.dirty),
				"seal_ns": sealNS, "seal_ns_per_page": sealNS / int64(c.dirty),
				"seal_walk_ns": walkNS, "seal_walk_ns_per_page": walkNS / int64(c.dirty),
				"seal_mapping_commands": after.Mappings - before.Mappings,
				"seal_mapping_runs":     after.MappingRuns - before.MappingRuns,
				"seal_protections":      after.Protections - before.Protections,
				"seal_protected_pages":  after.ProtectedPages - before.ProtectedPages,
				"guest_page_size":       size,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("SEAL_MEASUREMENT %s", raw)
		})
	}
}
