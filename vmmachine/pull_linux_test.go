//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/testnet"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmachine"
)

// The whole supervisor on real Firecracker, with guests marked to pull their
// memory. A create boots a guest whose checkpoint is pulled onto the host's
// disk behind it. The guest fills its root with 40 MiB of random bytes and
// stops, and an open with the mark pulls the checkpoint that stop published. A
// fork of the opened VM gives its child the mark, and the child pulls the
// checkpoint it inherits. The PMEM arena holds twelve of the fill's twenty
// pages, so a guest reading the fill twice evicts pages it read earlier and
// faults them in again. Once a guest's pull is complete, none of that reads a
// checkpoint object: every page comes from the arena or from the page cache's
// disk.
const (
	// pullRAMArena holds both guests' RAM whole, so no fault of theirs reads
	// RAM back; the pages under test are the root's.
	pullRAMArena = 320 << 20
	// pullPMEMArena is twelve 2 MiB pages against a 64 MiB root.
	pullPMEMArena = 24 << 20
	pullGuestRAM  = 128 << 20
	// pullFillMiB is what the guest writes into its root before it stops, so
	// the checkpoint an open pulls holds more pages than the arena.
	pullFillMiB = 40
)

// checkpointGets counts the reads of checkpoint objects: parts and index
// objects, which are what a page and a segment are fetched from. Control records
// are left out, because the host re-reads them on a timer of its own.
type checkpointGets struct {
	platform.ObjectStore
	gets atomic.Int64
}

func (s *checkpointGets) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if strings.Contains(request.Key.String(), "/ckpt/") {
		s.gets.Add(1)
	}
	return s.ObjectStore.Get(ctx, request)
}

func TestPulledGuestsFaultWithoutTheObjectStore(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	logs := capturing(t)
	runtime := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Nanosecond,
		GetLatency: time.Nanosecond, PutLatency: time.Nanosecond, ListLatency: time.Nanosecond,
		DeleteLatency: time.Nanosecond, BytesPerSecond: 1 << 50}})
	objects := &checkpointGets{ObjectStore: runtime.ObjectStore()}
	disk, err := adapters.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := host.Start(ctx, host.SupervisorConfig{
		ObjectStore: objects, Network: testnet.New(), Disk: disk, Disks: adapters.NewDisk,
		PodIP: "127.0.0.1", PagePort: 1, PodName: "pull-host", Orchestrator: "http://127.0.0.1:1",
		HugepageDir: t.TempDir(), ScratchDir: t.TempDir(), RAMPageSize: ramPageBytes(t),
		ArenaBytes:  host.KindBytes{RAM: pullRAMArena, PMEM: pullPMEMArena},
		MemoryBytes: pullRAMArena + pullPMEMArena,
		// The memory tier keeps no page, so a page the arena let go of is read
		// from the disk or from the store and nowhere else.
		CacheBytes:     4 << 10,
		CacheDiskBytes: 256 << 20,
		// The fill is dirty until the stop publishes it, and this host runs no
		// checkpoint loop to relieve the budget sooner.
		SpillBytes:   host.KindBytes{RAM: pullRAMArena, PMEM: 128 << 20},
		LogicalPages: host.KindPages{RAM: int(4 * pullGuestRAM / ramPageBytes(t)), PMEM: 128},
		DirtyPages:   host.KindPages{RAM: int(pullRAMArena / ramPageBytes(t)), PMEM: 64},
		Starter: &vmmachine.Firecracker{Binary: binaryPath,
			SeccompFilter: os.Getenv("SPROUTFS_FIRECRACKER_SECCOMP"),
			Kernel:        os.Getenv("SPROUTFS_FIRECRACKER_KERNEL"), BootArgs: guestPmemBootArgs,
			VCPUs: 1, VsockCID: guestVsockCID},
		Templates: host.Templates{"guest": {Path: os.Getenv("SPROUTFS_FIRECRACKER_ROOT"),
			MemoryBytes: pullGuestRAM}},
		VMMemoryBytes:      pullGuestRAM,
		CheckpointInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Errorf("closing the host: %v", err)
		}
		if t.Failed() {
			logs.mu.Lock()
			defer logs.mu.Unlock()
			for _, entry := range logs.logs {
				t.Logf("host log: %s %s %v", entry.level, entry.message, entry.attrs)
			}
		}
	})

	created, err := service.Create(ctx, hostapi.CreateRequest{ID: "pull-parent", Template: "guest", Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.VM.Pull == nil {
		t.Fatalf("a VM created with Pull reports no pull: %+v", created.VM)
	}
	awaitAgent(t, ctx, service, "pull-parent")
	awaitPull(t, ctx, service, "pull-parent")
	fill := fmt.Sprintf("dd if=/dev/urandom of=/fill bs=1M count=%d && sync", pullFillMiB)
	if result, err := service.Exec(ctx, "pull-parent", guest.ExecRequest{Cmd: fill, Timeout: 120}); err != nil ||
		result.Exit != 0 {
		t.Fatalf("filling the root: %+v, %v", result, err)
	}
	if _, err := service.Stop(ctx, "pull-parent", hostapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	opened, err := service.Open(ctx, "pull-parent", hostapi.OpenRequest{Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if opened.VM.Pull == nil {
		t.Fatalf("a VM opened with Pull reports no pull: %+v", opened.VM)
	}
	awaitAgent(t, ctx, service, "pull-parent")
	parent := awaitPull(t, ctx, service, "pull-parent")
	if parent.Bytes < pullFillMiB<<20 {
		t.Fatalf("the opened VM pulled %d bytes, want at least the %d MiB its guest wrote", parent.Bytes, pullFillMiB)
	}
	readsFillWithoutTheStore(t, ctx, service, objects, "pull-parent", parent)

	before := diskUsed(t, ctx, service)
	forked, err := service.Fork(ctx, "pull-parent", hostapi.ForkRequest{IDs: []string{"pull-child"}, Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(forked.Handoffs) != 1 || !forked.Handoffs[0].Pull {
		t.Fatalf("the fork's handoffs do not carry the mark: %+v", forked.Handoffs)
	}
	if _, err := service.Receive(ctx, forked.Handoffs[0]); err != nil {
		t.Fatal(err)
	}
	if err := service.Released(ctx, "pull-child"); err != nil {
		t.Fatal(err)
	}
	child := awaitPull(t, ctx, service, "pull-child")
	// The child's root names the pages of the parent's checkpoint, among them
	// the fill, which the parent's pull has already copied: the child holds
	// the parent's copy of those rather than making another.
	if added := diskUsed(t, ctx, service) - before; added > child.Bytes-pullFillMiB<<20+diskBlockBytes {
		t.Fatalf("the child's pull of %d bytes took %d more of the disk, want the %d MiB fill shared with the parent",
			child.Bytes, added, pullFillMiB)
	}
	readsFillWithoutTheStore(t, ctx, service, objects, "pull-child", child)
	// The parent kept running across the fork, and still reads without the store.
	readsFillWithoutTheStore(t, ctx, service, objects, "pull-parent", parent)
}

// readsFillWithoutTheStore has a guest read the fill twice. The root is mounted
// with DAX, so the guest keeps no cache of it and every read is of the PMEM
// pages. The arena holds a fraction of the fill, so the second pass faults in
// pages the first evicted. Neither pass may read a checkpoint object.
func readsFillWithoutTheStore(t *testing.T, ctx context.Context, service host.Service, objects *checkpointGets,
	id string, pulled hostapi.Pull) {
	t.Helper()
	before := pmemPager(t, ctx, service)
	objects.gets.Store(0)
	for range 2 {
		result, err := service.Exec(ctx, id, guest.ExecRequest{Cmd: "cat /fill > /dev/null", Timeout: 120})
		if err != nil || result.Exit != 0 {
			t.Fatalf("%s reading the fill: %+v, %v", id, result, err)
		}
	}
	after := pmemPager(t, ctx, service)
	if gets := objects.gets.Load(); gets != 0 {
		t.Fatalf("%s read the fill twice after pulling %d bytes and made %d requests of checkpoint objects, want none",
			id, pulled.Bytes, gets)
	}
	// Twenty pages through an arena of twelve: each pass faults at least eight
	// of them in over pages it evicts, and those the second faults in are ones
	// an earlier read evicted.
	short := uint64(pullFillMiB<<20-pullPMEMArena) / (2 << 20)
	if evictions := after.Evictions - before.Evictions; evictions < 2*short {
		t.Fatalf("%s reading the fill twice evicted %d PMEM pages, want at least %d", id, evictions, 2*short)
	}
}

// awaitAgent returns once a guest's agent serves on its vsock, as its console
// says.
func awaitAgent(t *testing.T, ctx context.Context, service host.Service, id string) {
	t.Helper()
	want := fmt.Sprintf("sproutfs-guest-agent: serving on vsock port %d", guest.Port)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		console, err := service.Console(ctx, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(console.Data, want) {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("%s's agent never served: %v\n%s", id, context.Cause(ctx), console.Data)
		}
	}
}

// awaitPull returns a VM's pull once the host reports it complete, and fails
// the test on a pull that stopped short or was refused.
func awaitPull(t *testing.T, ctx context.Context, service host.Service, id string) hostapi.Pull {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := service.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		at := slices.IndexFunc(status.VMs, func(vm hostapi.VM) bool { return vm.ID == id })
		if at < 0 || status.VMs[at].Pull == nil {
			t.Fatalf("the host reports no pull of %s: %+v", id, status.VMs)
		}
		if pull := *status.VMs[at].Pull; pull.Done {
			if pull.Error != "" || pull.Bytes == 0 || pull.Pulled != pull.Bytes {
				t.Fatalf("the pull of %s ended at %+v, want the whole checkpoint on the disk", id, pull)
			}
			if status.Resources.CacheDiskUsed == 0 {
				t.Fatalf("the page cache's disk holds nothing after %s's pull", id)
			}
			return pull
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("the pull of %s did not finish: %v", id, context.Cause(ctx))
		}
	}
}

// diskBlockBytes is the unit the page cache's disk is handed out in, which one
// pull may round its copy up by.
const diskBlockBytes = 4 << 10

// diskUsed is what the pulls on the host hold of the page cache's disk.
func diskUsed(t *testing.T, ctx context.Context, service host.Service) int64 {
	t.Helper()
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return status.Resources.CacheDiskUsed
}

// pmemPager is the host's PMEM pager report.
func pmemPager(t *testing.T, ctx context.Context, service host.Service) hostapi.PagerKind {
	t.Helper()
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return status.Pager.PMEM
}
