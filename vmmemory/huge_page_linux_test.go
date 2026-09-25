//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"bytes"
	"errors"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmwire"

	"github.com/semistrict/sproutfs/vmmemory"
)

func TestLinuxArenaUsesWholeHugePages(t *testing.T) {
	if os.Getenv("SPROUTFS_VM_MEMORY_CLIENT") == "" {
		t.Skip("requires a provisioned HugeTLB pool")
	}
	if _, err := vmmemory.NewLinuxArena(0, hugePageSize); !errors.Is(err, vmmemory.ErrConfig) {
		t.Fatalf("empty arena = %v", err)
	}
	a, err := vmmemory.NewLinuxArena(2, hugePageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	data := bytes.Repeat([]byte{73}, hugePageSize)
	if err := a.Write(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hugePageSize)
	if err := a.Read(t.Context(), 0, got); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("whole-page read: %v", err)
	}
	if n, err := a.AllocatedBytes(); err != nil || n != hugePageSize {
		t.Fatalf("allocated %d bytes: %v", n, err)
	}
	if err := a.Release(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if n, err := a.AllocatedBytes(); err != nil || n != 0 {
		t.Fatalf("released page still holds %d bytes: %v", n, err)
	}
	if err := a.Read(t.Context(), 0, got); err != nil || !bytes.Equal(got, make([]byte, hugePageSize)) {
		t.Fatalf("punched slot read: %v", err)
	}
	if n, err := a.AllocatedBytes(); err != nil || n != 0 {
		t.Fatalf("reading a hole allocated %d bytes: %v", n, err)
	}
	if err := a.Zero(t.Context(), 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := a.Read(t.Context(), 0, got); err != nil || !bytes.Equal(got, make([]byte, hugePageSize)) {
		t.Fatalf("reused page exposed old data: %v", err)
	}
}

// Pool exhaustion must be reported before mmap access, which would SIGBUS.
// This test temporarily consumes the host pool and needs a dedicated host.
func TestLinuxArenaPoolExhaustionReturnsError(t *testing.T) {
	if os.Getenv("SPROUTFS_HUGETLB_EXHAUSTION") != "1" {
		t.Skip("requires a dedicated host's HugeTLB pool")
	}
	raw, err := os.ReadFile("/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages")
	if err != nil {
		t.Fatal(err)
	}
	pages, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pages < 1 {
		t.Fatalf("HugeTLB pool: %q, %v", raw, err)
	}
	holder, err := vmwire.HugeMemfd("sproutfs-pool-exhaustion", int64(pages)*hugePageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	for slot := range pages {
		err := syscall.Fallocate(int(holder.Fd()), 1, int64(slot)*hugePageSize, hugePageSize)
		// Faulting in a huge page can be interrupted by the Go runtime's own
		// preemption signal; that is a retry, not a failure of the pool.
		for errors.Is(err, syscall.EINTR) {
			err = syscall.Fallocate(int(holder.Fd()), 1, int64(slot)*hugePageSize, hugePageSize)
		}
		if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.ENOMEM) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := vmmemory.NewLinuxArena(1, hugePageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	data := make([]byte, hugePageSize)
	data[0] = 41
	if err := a.Write(t.Context(), 0, data); !errors.Is(err, syscall.ENOSPC) && !errors.Is(err, syscall.ENOMEM) {
		t.Fatalf("exhausted pool write = %v", err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Write(t.Context(), 0, data); err != nil {
		t.Fatalf("write after pool release: %v", err)
	}
}

// Runtime signals can interrupt HugeTLB allocation and punching. They must not
// turn an otherwise healthy pager into a terminal mapping failure.
func TestLinuxArenaAllocationSurvivesSignals(t *testing.T) {
	if os.Getenv("SPROUTFS_VM_MEMORY_CLIENT") == "" {
		t.Skip("requires a provisioned HugeTLB pool")
	}
	a, err := vmmemory.NewLinuxArena(1, hugePageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tid := syscall.Gettid()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = syscall.Tgkill(os.Getpid(), tid, syscall.SIGUSR1)
			time.Sleep(100 * time.Microsecond)
		}
	}()
	defer func() { close(stop); <-done }()
	data := make([]byte, hugePageSize)
	for range 3000 {
		if err := a.Write(t.Context(), 0, data); err != nil {
			t.Fatalf("allocation interrupted: %v", err)
		}
		if err := a.Release(t.Context(), 0); err != nil {
			t.Fatalf("punch interrupted: %v", err)
		}
	}
}
