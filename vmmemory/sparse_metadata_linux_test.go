//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func processMemory(t *testing.T, pid int) map[string]uint64 {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[2] == "kB" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			result[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	return result
}

func TestLargeNativeZeroMappingsKeepMetadataSparse(t *testing.T) {
	if os.Getenv("SPROUTFS_PAGER_MEASURE") == "" {
		t.Skip("set SPROUTFS_PAGER_MEASURE=1 for 32 GiB mapping qualification")
	}
	const length = 16 << 30
	pages := length / os.Getpagesize()
	h := kernelHostBudget(t, 2, 2*pages, 8)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	a := startNative(t, h, pages,
		&sparseMemoryBacking{size: length, vm: "sparse-pmem", pages: make(map[uint64][]byte)},
		&sparseMemoryBacking{size: length, vm: "sparse-ram", pages: make(map[uint64][]byte)})
	a.request("read 0 0 1", "data 00")
	if err := a.populate(t.Context()); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	native := processMemory(t, a.cmd.Process.Pid)
	stats, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"logical_bytes": 2 * length, "pager_heap_delta_bytes": int64(after.HeapAlloc) - int64(before.HeapAlloc), "pager_process": processMemory(t, os.Getpid()), "vmm_process": native, "arena_resident_pages": stats.ResidentPages, "page_size": os.Getpagesize()}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SPARSE_METADATA_MEASUREMENT %s", raw)
	if native["RssAnon"] > 4<<20 {
		t.Fatalf("32 GiB zero mappings retained %d anonymous VMM bytes, budget 4 MiB", native["RssAnon"])
	}
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 4<<20 {
		t.Fatalf("32 GiB mappings retained %d Go heap bytes, budget 4 MiB", retained)
	}
	// Exercise distant range splits and private spill, then check untouched zeros.
	for memoryRegion := range 2 {
		for i, page := range []int{0, pages / 2, pages - 1} {
			a.request(fmt.Sprintf("fill %d %d 1 %d", memoryRegion, page*os.Getpagesize(), 71+i), "filled")
		}
		for i, page := range []int{0, pages / 2, pages - 1} {
			a.request(fmt.Sprintf("read %d %d 1", memoryRegion, page*os.Getpagesize()), fmt.Sprintf("data %02x", 71+i))
		}
		a.request(fmt.Sprintf("read %d %d 1", memoryRegion, (pages-2)*os.Getpagesize()), "data 00")
	}
	runtime.KeepAlive(a)
}
