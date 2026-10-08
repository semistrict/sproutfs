//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/volume"
)

// The shape of the reads below, a sixteenth of the run an embedder measured on
// 2026-10-08: an 80 GiB DAX root over a 6.4 GiB PMEM arena, a 512 MiB file read
// at random while several GiB were written. Here the root is 2 GiB, the arena
// 256 MiB, the file 32 MiB, and the writes four arenas.
const (
	readsRootBytes  = 2 << 30
	readsArenaBytes = 256 << 20
	readsFileBytes  = 32 << 20
	readsWriteBytes = 4 * readsArenaBytes
)

// readsReport is what `witness reads` prints (cmd/sproutfs-guest-witness/reads.go).
type readsReport struct {
	WrittenBytes int64      `json:"written_bytes"`
	WriteSeconds float64    `json:"write_seconds"`
	Writing      readsPhase `json:"writing"`
	Idle         readsPhase `json:"idle"`
}

// readsPhase is one phase's reads, in microseconds.
type readsPhase struct {
	Reads int     `json:"reads"`
	P50   float64 `json:"p50_us"`
	P90   float64 `json:"p90_us"`
	P99   float64 `json:"p99_us"`
	Max   float64 `json:"max_us"`
}

// TestWhatAGuestReadsOfItsDAXRootWhileItWrites measures what a guest's reads
// of one file of its DAX root cost while it writes four times the PMEM arena
// of fresh files, and after. A DAX read copies out of the host's page through
// the guest's mapping, which the pager never sees, so the reads are fast only
// while the evictor keeps the pages the guest keeps reading. The witness checks
// every byte it reads; the test requires the writes to have evicted, and logs
// the latencies and what the PMEM pager did, which is the measurement TASK-109
// compares before and after its harvest.
func TestWhatAGuestReadsOfItsDAXRootWhileItWrites(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	vm := newGuestVM(t, ctx, "reads")
	if err := vm.DiscardMemory(ctx, vmmachine.RAMVolume,
		volume.Shape{Sizes: map[string]uint64{"root": readsRootBytes}}); err != nil {
		t.Fatal(err)
	}
	ram := residentPages(t, pagerPageBytes(t), 128<<20) * pagerPageBytes(t)
	pagers := newSizedMigrationPager(t, ctx, ram, readsArenaBytes, readsRootBytes+(256<<20), readsRootBytes)
	p := bootGuestWithAgentOn(t, ctx, binaryPath, pagers, vm)
	runInGuest(t, ctx, p, fmt.Sprintf("%s grow /", guestWitness))

	before := statsOf(t, ctx, pagers.pagers.Pmem)
	command := fmt.Sprintf("%s reads --file /reads-file --dir /reads-fresh --size %d --write-bytes %d "+
		"--interval 2ms --idle 10s", guestWitness, readsFileBytes, readsWriteBytes)
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: command, Timeout: 900})
	if err != nil {
		t.Fatalf("running %q in the guest: %v\n%s", command, err, consoleText(p))
	}
	if result.Exit != 0 {
		t.Fatalf("%q exited %d in the guest\nstdout: %s\nstderr: %s", command, result.Exit, result.Stdout, result.Stderr)
	}
	var report readsReport
	if err := json.Unmarshal([]byte(result.Stdout), &report); err != nil {
		t.Fatalf("the reads printed %q: %v", result.Stdout, err)
	}
	after := statsOf(t, ctx, pagers.pagers.Pmem)
	if report.WrittenBytes != readsWriteBytes {
		t.Fatalf("the guest wrote %d bytes, want %d", report.WrittenBytes, readsWriteBytes)
	}
	if after.Evictions == before.Evictions {
		t.Fatalf("writing %d bytes over a %d-byte arena evicted nothing", readsWriteBytes, readsArenaBytes)
	}
	t.Logf("reads during %.1f s of writes: %d, p50 %.0f us, p90 %.0f us, p99 %.0f us, max %.0f us",
		report.WriteSeconds, report.Writing.Reads, report.Writing.P50, report.Writing.P90, report.Writing.P99,
		report.Writing.Max)
	t.Logf("reads after: %d, p50 %.0f us, p90 %.0f us, p99 %.0f us, max %.0f us",
		report.Idle.Reads, report.Idle.P50, report.Idle.P90, report.Idle.P99, report.Idle.Max)
	t.Logf("PMEM pager: evictions %d, spills %d, spill refaults %d, loads %d, harvested %d, second chances %d",
		after.Evictions-before.Evictions, after.Spills-before.Spills, after.SpillRefaults-before.SpillRefaults,
		after.Loads-before.Loads, after.HarvestedPages-before.HarvestedPages,
		after.SecondChances-before.SecondChances)
}
