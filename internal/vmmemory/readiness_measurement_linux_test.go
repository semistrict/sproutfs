//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// Timings are observations, never correctness thresholds. Run through the
// Linux qualification script with SPROUTFS_PAGER_MEASURE=1.
func TestManagedPagerReadinessMeasurements(t *testing.T) {
	if os.Getenv("SPROUTFS_PAGER_MEASURE") == "" {
		t.Skip("set SPROUTFS_PAGER_MEASURE=1")
	}
	const pages = 1024
	h := kernelHost(t, pages, 4*pages)
	a := startNative(t, h, pages)
	cpu := func() int64 {
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			t.Fatal(err)
		}
		return usage.Utime.Nano() + usage.Stime.Nano()
	}
	before, _ := h.Stats(t.Context())
	cpuStart, idleStart := cpu(), time.Now()
	time.Sleep(500 * time.Millisecond)
	idleWall, idleCPU := time.Since(idleStart).Nanoseconds(), cpu()-cpuStart
	after, _ := h.Stats(t.Context())
	latencies := make([]int64, 0, pages/2)
	for page := 0; page < pages; page += 2 {
		start := time.Now()
		a.request(fmt.Sprintf("kvmread 0 %d", page*vmmemory.PageSize), fmt.Sprintf("kvm %d", byte(page+1)))
		latencies = append(latencies, time.Since(start).Nanoseconds())
	}
	slices.Sort(latencies)
	beforeAttach, _ := h.Stats(t.Context())
	start := time.Now()
	b := startNative(t, h, pages)
	attachNS := time.Since(start).Nanoseconds()
	afterAttach, _ := h.Stats(t.Context())
	for page := 0; page < pages; page += 2 {
		b.request(fmt.Sprintf("kvmread 0 %d", page*vmmemory.PageSize), fmt.Sprintf("kvm %d", byte(page+1)))
	}
	checked, _ := h.Stats(t.Context())
	if checked.Faults != afterAttach.Faults || afterAttach.Loads != beforeAttach.Loads {
		t.Fatal("fragmented attach left resident pages to fault or load")
	}
	result := map[string]any{
		"idle_connections": 1, "idle_wall_ns": idleWall, "idle_pager_cpu_ns": idleCPU,
		"idle_uffd_reads":    after.UFFDReads - before.UFFDReads,
		"cold_fault_samples": len(latencies), "cold_fault_p50_ns": latencies[len(latencies)/2],
		"cold_fault_p95_ns": latencies[len(latencies)*95/100], "cold_fault_p99_ns": latencies[len(latencies)*99/100],
		"fragmented_attach_ns": attachNS, "fragmented_mapping_runs": afterAttach.MappingRuns - beforeAttach.MappingRuns,
		"fragmented_remap_events": afterAttach.RemapEvents - beforeAttach.RemapEvents,
		"guest_page_size":         vmmemory.PageSize,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PAGER_MEASUREMENT %s", raw)
}
