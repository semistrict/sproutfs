//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/api/guest"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

const (
	// flushArrival is how long a guest's sync may take to reach the host. It is
	// a guest write, a virtio notification and one socket read, so anything
	// near this bound is a flush that never came.
	flushArrival = 30 * time.Second
	// flushHeld is how long the host holds the flush before answering it. A
	// sync that returned in that time would have returned without the host.
	flushHeld = 3 * time.Second
)

// TestAGuestFlushWaitsForTheHost: a guest that writes a file on its PMEM root
// and syncs it makes a virtio-pmem flush, and the sync returns only once the
// host has answered it — which a host does once what was flushed is durable,
// taking a disk checkpoint first if it has to. The host here holds its answer:
// while it does the guest's sync has not returned, and once it answers, it
// does.
func TestAGuestFlushWaitsForTheHost(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	vm := newGuestVM(t, ctx, "flushed")
	pagers := newMigrationPager(t, ctx)

	// Flushes are answered at once until the test holds them: the guest's own
	// boot flushes its root as it mounts and writes it.
	var mu sync.Mutex
	holding := false
	var held []func(error)
	var memoryRegions []*vmmemory.MemoryRegion
	arrived := make(chan struct{}, 1)
	pagers.pagers.Pmem.SetFlushed(func(r *vmmemory.MemoryRegion, done func(error)) {
		mu.Lock()
		if !holding {
			mu.Unlock()
			done(nil)
			return
		}
		held = append(held, done)
		memoryRegions = append(memoryRegions, r)
		mu.Unlock()
		select {
		case arrived <- struct{}{}:
		default:
		}
	})
	release := func() {
		mu.Lock()
		holding = false
		answer := held
		held = nil
		mu.Unlock()
		for _, done := range answer {
			done(nil)
		}
	}
	t.Cleanup(release)
	p := bootGuestWithAgentOn(t, ctx, binaryPath, pagers, vm)
	root := p.MemoryRegions()["root"]

	mu.Lock()
	holding = true
	mu.Unlock()
	synced := execInGuest(ctx, p, guest.ExecRequest{
		Cmd: "echo flushed > /flushed && sync && cat /flushed", Timeout: 60})
	select {
	case <-arrived:
	case answered := <-synced:
		t.Fatalf("the guest's sync returned before any flush reached the host: %+v %v\n%s",
			answered.result, answered.err, consoleText(p))
	case <-time.After(flushArrival):
		t.Fatalf("the guest's sync reached no flush callback within %s\n%s", flushArrival, consoleText(p))
	}
	select {
	case answered := <-synced:
		t.Fatalf("the guest's sync returned while the host held its flush: %+v %v",
			answered.result, answered.err)
	case <-time.After(flushHeld):
	}
	mu.Lock()
	for _, r := range memoryRegions {
		if r != root {
			mu.Unlock()
			t.Fatalf("a flush reached the host as memory region %p, want the root's %p", r, root)
		}
	}
	mu.Unlock()

	release()
	select {
	case answered := <-synced:
		if answered.err != nil {
			t.Fatalf("the guest's sync failed once the host answered: %v\n%s", answered.err, consoleText(p))
		}
		if answered.result.Exit != 0 || answered.result.Stdout != "flushed\n" {
			t.Fatalf("the guest's write and sync answered %+v", answered.result)
		}
	case <-time.After(flushArrival):
		t.Fatalf("the guest's sync had not returned %s after the host answered\n%s", flushArrival, consoleText(p))
	}
	stats, err := pagers.pagers.Pmem.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Flushes == 0 {
		t.Fatal("the PMEM pager counted no flushes")
	}
}
