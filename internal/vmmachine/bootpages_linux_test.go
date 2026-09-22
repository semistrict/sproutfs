package vmmachine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/volume"
)

// bootPagesRAMBytes is the guest RAM the boot survey runs with, so the survey
// can be taken at the 16 GiB the GCE comparison boots.
func bootPagesRAMBytes(t *testing.T) int {
	t.Helper()
	value := os.Getenv("SPROUTFS_BOOT_RAM_BYTES")
	if value == "" {
		return 1 << 30
	}
	bytes, err := strconv.Atoi(value)
	if err != nil || bytes < 128<<20 {
		t.Fatalf("invalid SPROUTFS_BOOT_RAM_BYTES %q", value)
	}
	return bytes
}

// bootPageClass is what one private RAM page held once the guest had booted.
type bootPageClass struct {
	Pages        int `json:"pages"`
	ZeroPages    int `json:"zero_pages"`
	NonZeroBytes int `json:"non_zero_bytes"`
}

// A guest boot at a 4 KiB RAM page makes every page it touches private, and
// the count of them is what the boot costs in faults. This survey says what
// those pages hold afterwards — whether the guest wrote anything into them at
// all, and where in its address space they are — read byte by byte through the
// pager once the guest is ready. It records what it found rather than
// asserting a number, because the number is what is being asked.
func TestBootSurveyOfPrivateRAMPages(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	ramBytes := bootPagesRAMBytes(t)
	c := newMigrationCluster(t, ctx)
	vm, err := c.source.Create(ctx, "boot-survey", []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: uint64(ramBytes), PageSize: ramPageBytes(t)},
		{Name: "root", Size: guestRootBytes, PageSize: checkpoint.PageSize2MiB},
	})
	if err != nil {
		t.Fatal(err)
	}
	loadRootImage(t, ctx, vm.Volume("root"))
	if err := vm.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	// The arena holds whatever the boot makes private: a 16 GiB guest's boot
	// on GCE made 237,269 pages private, under 1 GiB, and the budget here is
	// twice the RAM so nothing is evicted or spilled during the survey.
	pager := newSizedMigrationPager(t, ctx, ramBytes, 256<<20, 2*ramBytes, 2*ramBytes)
	config := migrationConfig(t, binaryPath, pager, vm)
	started := time.Now()
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	booted := time.Since(started)

	// The guest's own account of its memory: the layout dmesg prints and the
	// counters meminfo keeps, run through the fixture's console before the
	// survey touches anything. The multi-call busybox serves both.
	report := func(cmd string) string {
		reader := newConsole(p)
		defer reader.close()
		if err := reader.skipExisting(); err != nil {
			t.Fatal(err)
		}
		if err := p.WriteConsole(ctx, []byte("run "+cmd+"\n")); err != nil {
			t.Fatal(err)
		}
		_, output, err := reader.waitOutput(ctx, "SPROUTFS_RUN")
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return output
	}
	meminfo := report("/bin/busybox cat /proc/meminfo")
	layout := report("/bin/busybox dmesg | /bin/busybox grep -iE 'memory|memmap|vmemmap|linear|reserved|swiotlb|e820|BIOS-e820|node'")

	stats, err := pager.pagers.Ram.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ram := p.Regions()[vmmachine.RAMVolume]
	resident, err := ram.Resident()
	if err != nil {
		t.Fatal(err)
	}
	// Every private page is read byte by byte. The classes are by content and by
	// place: a page the guest never wrote reads back as zeros, and the 64 MiB
	// bucket says which part of the guest's address space each class lives in.
	page := make([]byte, ram.PageSize())
	const bucketBytes = 64 << 20
	buckets := map[uint64]*bootPageClass{}
	var total bootPageClass
	private := 0
	for _, index := range resident {
		held, unpublished, err := ram.ReadResident(ctx, index, page)
		if err != nil {
			t.Fatal(err)
		}
		if !held || !unpublished {
			continue
		}
		private++
		nonZero := 0
		for _, b := range page {
			if b != 0 {
				nonZero++
			}
		}
		bucket := index * ram.PageSize() / bucketBytes
		class := buckets[bucket]
		if class == nil {
			class = &bootPageClass{}
			buckets[bucket] = class
		}
		for _, c := range []*bootPageClass{class, &total} {
			c.Pages++
			c.NonZeroBytes += nonZero
			if nonZero == 0 {
				c.ZeroPages++
			}
		}
	}
	keys := make([]uint64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var lines []string
	for _, k := range keys {
		c := buckets[k]
		lines = append(lines, fmt.Sprintf("%6d MiB: pages=%6d zero=%6d non-zero bytes=%d",
			k*bucketBytes>>20, c.Pages, c.ZeroPages, c.NonZeroBytes))
	}
	t.Logf("boot %s: ram=%d MiB page=%d faults=%d copy-on-writes=%d resident=%d private=%d zero=%d non-zero bytes=%d\n%s\n%s\n%s",
		booted.Round(time.Millisecond), ramBytes>>20, ram.PageSize(), stats.Faults, stats.CopyOnWrites,
		len(resident), private, total.ZeroPages, total.NonZeroBytes, strings.Join(lines, "\n"), meminfo, layout)
	if out := os.Getenv("SPROUTFS_BOOT_SURVEY_OUT"); out != "" {
		record := map[string]any{"ram_bytes": ramBytes, "page_size": ram.PageSize(), "boot_ns": booted.Nanoseconds(),
			"faults": stats.Faults, "copy_on_writes": stats.CopyOnWrites, "resident": len(resident),
			"private": private, "total": total, "buckets": buckets, "meminfo": meminfo, "layout": layout}
		data, err := json.MarshalIndent(record, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
