//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/adapters"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

const (
	// guestConsoleArgs keeps the serial console, which carries the guest
	// init's protocol lines, and silences the kernel's own messages. The
	// console is an emulated UART that every kernel line crosses
	// synchronously; at the default log level those lines are half of a cold
	// boot's time to init. Level 1 still prints panics and soft lockups.
	guestConsoleArgs = "console=ttyS0 quiet loglevel=1"
	// guestPmemBootArgs boots from a PMEM root mounted with DAX.
	guestPmemBootArgs = guestConsoleArgs + " reboot=k panic=1 init=/init rootfstype=ext4 rootflags=dax=always"
)

func TestFirecrackerDAXCaptureRestoreForkAndFence(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	runtime := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Nanosecond, GetLatency: time.Nanosecond, PutLatency: time.Nanosecond, ListLatency: time.Nanosecond, DeleteLatency: time.Nanosecond, BytesPerSecond: 1 << 50}})
	client, err := control.NewClient(control.Config{ObjectStore: runtime.ObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: runtime.ObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := volume.NewManager(volume.Config{Control: client, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	source, err := manager.Create(ctx, "source", []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: 128 << 20},
		{Name: "root", Size: 64 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	pmem := source.Volume("root")
	rootImage, err := os.Open(os.Getenv("SPROUTFS_FIRECRACKER_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1<<20)
	zero := make([]byte, len(buffer))
	var offset uint64
	for {
		count, err := io.ReadFull(rootImage, buffer)
		if err == io.EOF {
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			t.Fatal(err)
		}
		if !bytes.Equal(buffer[:count], zero[:count]) {
			if err := pmem.Write(ctx, offset, buffer[:count]); err != nil {
				t.Fatal(err)
			}
		}
		offset += uint64(count)
		if err == io.ErrUnexpectedEOF {
			break
		}
	}
	_ = rootImage.Close()
	if offset != pmem.Size() {
		t.Fatalf("root image length %d", offset)
	}
	if err := source.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	pageBytes := pagerPageBytes(t)
	slots := residentPages(t, pageBytes, 192<<20)
	pages := (384 << 20) / pageBytes
	a, err := vmmemory.NewLinuxArena(slots)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	disk, err := adapters.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spill, err := disk.Open(ctx, "spill", platform.OpenOptions{Create: true, Exclusive: true, Permissions: 0o600})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spill.Close() })
	host, err := vmmemory.New(ctx, testresource.New(), vmmemory.Config{ResidentPages: slots, LogicalPages: pages, DirtyPages: pages}, a, spill)
	if err != nil {
		t.Fatal(err)
	}
	vmConfig := vmmachine.Config{Binary: binaryPath, SeccompFilter: os.Getenv("SPROUTFS_FIRECRACKER_SECCOMP"), KernelPath: os.Getenv("SPROUTFS_FIRECRACKER_KERNEL"), InitrdPath: os.Getenv("SPROUTFS_FIRECRACKER_INITRD"), BootArgs: guestPmemBootArgs, Host: host, VM: source, Pmem: []vmmachine.Pmem{{ID: "root", Root: true}}, VCPUs: 1, Connection: vmmemory.ConnectionConfig{QueuePages: 128, CommandTimeout: 2 * time.Minute, VerifyInterval: time.Second}}
	vmConfig.Scratch = mustScratch(t)
	bad := vmConfig
	bad.KernelPath = "/missing-sproutfs-qualification-kernel"
	if _, err := vmmachine.Start(ctx, bad); err == nil {
		t.Fatal("missing kernel started a VM")
	}
	entries, err := os.ReadDir(vmConfig.Scratch.Directory())
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed start retained private artifacts: %v %v", entries, err)
	}
	stats, err := host.Stats(ctx)
	if err != nil || stats.LogicalPages != 0 {
		t.Fatalf("failed start retained mappings: %+v %v", stats, err)
	}
	start := time.Now()
	p, err := vmmachine.Start(ctx, vmConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	t.Logf("DAX boot elapsed=%s", time.Since(start))
	command(t, ctx, p, "write 40\n", "SPROUTFS_FLUSH disk=40")
	command(t, ctx, p, "dirty 41\n", "SPROUTFS_DIRTY disk=41")
	command(t, ctx, p, "flush\n", "SPROUTFS_FLUSH disk=41")
	raw := consoleText(p)
	match := regexp.MustCompile(`SPROUTFS_FLUSH disk=41 offset=([0-9]+)`).FindSubmatch(raw)
	if len(match) != 2 {
		t.Fatal("guest did not report its DAX extent")
	}
	fileOffset, err := strconv.ParseUint(string(match[1]), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	// A guest flush makes nothing durable: the device completes it and the host
	// is never asked. The DAX store is this host's frames alone until a checkpoint
	// publishes them.
	var durable [8]byte
	if err := pmem.Read(ctx, fileOffset, durable[:]); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(durable[:]); got != 0 {
		t.Fatalf("the volume holds %d at the DAX extent after a guest flush, want nothing published", got)
	}
	command(t, ctx, p, "ram 73\n", "SPROUTFS_RAM ram=73")
	// Capture: the VMM pauses and seals every region as it writes its state, the
	// vCPUs resume, and the checkpoint uploads the sealed frames behind them.
	// The pause must contain the page-table work of the seal and none of that
	// traffic.
	beforeCapture, err := host.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paused := time.Now()
	var prepared, resumed time.Time
	var atResume vmmemory.Stats
	ckpt, err := source.Snapshot(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		state, sources, err := p.Prepare(ctx)
		if err != nil {
			return nil, nil, err
		}
		prepared = time.Now()
		if err := p.Resume(ctx); err != nil {
			return nil, nil, err
		}
		resumed = time.Now()
		if atResume, err = host.Stats(ctx); err != nil {
			return nil, nil, err
		}
		return state, sources, nil
	})
	if err != nil {
		raw := consoleText(p)
		t.Fatalf("capture: %v\n%s", err, raw)
	}
	sealed := atResume.CheckpointPages - beforeCapture.CheckpointPages
	if sealed == 0 {
		t.Fatal("the capture sealed no dirty page at all")
	}
	published := time.Now()
	if err := ckpt.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("capture pause=%s (prepare=%s resume=%s) sealed_pages=%d publish=%s",
		resumed.Sub(paused), prepared.Sub(paused), resumed.Sub(prepared), sealed, time.Since(published))
	// The checkpoint is what made the DAX store durable, and the guest's own flush
	// never did: the checkpoint this capture selected holds it.
	if err := pmem.Read(ctx, fileOffset, durable[:]); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(durable[:]); got != 41 {
		t.Fatalf("the published checkpoint holds %d at the DAX extent, want the 41 the guest stored", got)
	}
	command(t, ctx, p, "write 99\n", "SPROUTFS_FLUSH disk=99")
	command(t, ctx, p, "ram 101\n", "SPROUTFS_RAM ram=101")
	// A host that never held the capture rebuilds the instant from the published
	// checkpoint alone, which is what a fork of a template is.
	point, err := manager.Inherit(ctx, ckpt.Ref())
	if err != nil {
		t.Fatal(err)
	}
	fork, err := manager.Fork(ctx, "fork", point)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fork.Close(context.Background()) })
	// The fork's root index is its first checkpoint, and until it lands the fork
	// cannot be opened anywhere else.
	if err := fork.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	forkConfig := vmConfig
	forkConfig.VM = fork
	forkConfig.RestoreState = ckpt.State()
	// Keep the source quiescent while measuring the restore's mandatory attach:
	// paused, and with the checkpoint that pause sealed already published.
	quiet, err := source.Snapshot(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		return p.Prepare(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := quiet.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	beforeAttach, err := host.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := vmmachine.Start(ctx, forkConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fp.Close() })
	attached, err := host.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mapped, commands := attached.MappedPages-beforeAttach.MappedPages, attached.Mappings-beforeAttach.Mappings
	if mapped < 64 || commands >= mapped/8 {
		t.Fatalf("restore did not batch resident pages before resume: mapped=%d commands=%d", mapped, commands)
	}
	// Inherited frames are mapped without any read. The VMM still touches a few
	// pages of its own while it rebuilds devices, and the source never made
	// every page resident, so a small remainder is loaded on demand.
	loaded := attached.LoadedPages - beforeAttach.LoadedPages
	t.Logf("restore before resume: mapped=%d commands=%d runs=%d loads=%d loaded_pages=%d faults=%d",
		mapped, commands, attached.MappingRuns-beforeAttach.MappingRuns, attached.Loads-beforeAttach.Loads, loaded, attached.Faults-beforeAttach.Faults)
	if loaded*16 >= mapped {
		t.Fatalf("restore from a shared checkpoint read back %d of the %d pages it inherited", loaded, mapped)
	}
	if err := fp.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Release(ctx); err != nil {
		t.Fatal(err)
	}
	command(t, ctx, fp, "read\n", "SPROUTFS_VALUE ram=73 disk=41")
	command(t, ctx, fp, "write 57\n", "SPROUTFS_FLUSH disk=57")
	command(t, ctx, p, "read\n", "SPROUTFS_VALUE ram=101 disk=99")
	left, leftVMAs := frames(t, p.PID())
	right, rightVMAs := frames(t, fp.PID())
	shared := 0
	for frame := range left {
		if right[frame] {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("running fork shares no physical backing frames with its source")
	}
	if slots*pageBytes <= pressureBytes {
		// Boot's resident set changes as sharing improves. Deliberately dirty
		// both guests beyond the shared arena budget, then verify every 4 KiB
		// subpage marker after the other guest has forced eviction/refault.
		for _, process := range []*vmmachine.Process{p, fp} {
			command(t, ctx, process, "pressure 48\n", "SPROUTFS_PRESSURE bytes=50331648")
		}
		for _, process := range []*vmmachine.Process{p, fp} {
			command(t, ctx, process, "checkpressure\n", "SPROUTFS_PRESSURE_OK bytes=50331648")
		}
	}
	allocated, err := a.AllocatedBytes()
	if err != nil || allocated > uint64(slots*pageBytes) {
		t.Fatalf("arena exceeded physical budget: %d %v", allocated, err)
	}
	stats, err = host.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slots*pageBytes <= pressureBytes && (stats.Evictions == 0 || stats.Spills == 0 || stats.SpillRefaults == 0) {
		t.Fatalf("pressure run did not exercise live spill/refault: %+v", stats)
	}
	t.Logf("memory pages=%+v arena_bytes=%d shared_frames=%d source_vmas=%d fork_vmas=%d", stats, allocated, shared, leftVMAs, rightVMAs)
	// The source half of a live migration: stop the guest and capture its state,
	// uploading nothing. The frames this host holds keep serving the
	// destination afterwards, which is the whole of what moves.
	regions := p.Regions()
	if len(regions) != 2 || regions[vmmachine.RAMVolume] == nil || regions["root"] == nil {
		t.Fatalf("Regions reports %d entries, want one named for each volume this machine maps", len(regions))
	}
	beforeStop, err := host.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stopStart := time.Now()
	stopped, err := p.Stop(ctx)
	if err != nil {
		raw := consoleText(p)
		t.Fatalf("stop: %v\n%s", err, raw)
	}
	stopElapsed := time.Since(stopStart)
	if len(stopped) == 0 {
		t.Fatal("the stop captured no VMM state for the destination")
	}
	afterStop, err := host.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sealed := afterStop.CheckpointPages - beforeStop.CheckpointPages; sealed != 0 {
		t.Fatalf("the migration's stop sealed %d pages, want none", sealed)
	}
	// A stopped source still holds its frames, which is what the destination
	// fetches from before it ever reads an object.
	ram := regions[vmmachine.RAMVolume]
	resident, err := ram.Resident()
	if err != nil {
		t.Fatalf("listing what the stopped source holds: %v", err)
	}
	if len(resident) == 0 {
		t.Fatal("the stopped source holds no frame to serve its destination")
	}
	sample := make([]byte, pageBytes)
	middle := resident[len(resident)/2]
	held, unpublished, err := ram.ReadResident(ctx, middle, sample)
	if err != nil || !held {
		t.Fatalf("the source could not serve page %d it just listed: %t %v", middle, held, err)
	}
	unpublishedPages, err := ram.Unpublished()
	if err != nil {
		t.Fatalf("listing what no checkpoint of the stopped source holds: %v", err)
	}
	t.Logf("migration stop_pause=%s resident=%d unpublished=%d sampled_page_unpublished=%t",
		stopElapsed, len(resident), len(unpublishedPages), unpublished)
	// A replacement volume writer takes the control record. A running VM writes
	// nothing but its checkpoints now, so that is where the stale host learns it
	// was replaced: the checkpoint uploads, its control write is refused, and the
	// handle is terminal from then on. The guest's own reads, covered by present
	// KVM mappings, never touch the record and never notice.
	newOwner, err := manager.Open(ctx, "fork")
	if err != nil {
		t.Fatal(err)
	}
	defer newOwner.Close(ctx)
	stale, err := fork.Snapshot(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		state, sources, err := fp.Prepare(ctx)
		if err != nil {
			return nil, nil, err
		}
		return state, sources, fp.Resume(ctx)
	})
	if err != nil {
		raw := consoleText(fp)
		t.Fatalf("the fenced host could not take its checkpoint: %v\n%s", err, raw)
	}
	if err := stale.Wait(ctx); !errors.Is(err, volume.ErrNeedsRecovery) {
		t.Fatalf("the fenced host published its checkpoint: %v", err)
	}
	if status := fork.Status(); !errors.Is(status.Err, volume.ErrNeedsRecovery) {
		t.Fatalf("the fenced handle reports %v, want a terminal error", status.Err)
	}
	// Nothing that host holds can become durable again, so its VM is shut down.
	if err := fp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err = host.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LogicalPages != 0 || stats.DirtyPages != 0 || stats.ResidentPages != 0 {
		t.Fatalf("stopped VMM mappings retained: %+v", stats)
	}
	allocated, err = a.AllocatedBytes()
	if err != nil || allocated != 0 {
		t.Fatalf("stopped VMM backing remains allocated: %d %v", allocated, err)
	}
	entries, err = os.ReadDir(vmConfig.Scratch.Directory())
	if err != nil || len(entries) != 0 {
		t.Fatalf("stopped VMM artifacts remain: %v %v", entries, err)
	}
}

// pressureBytes is the resident budget at or below which the full-guest suite
// must see eviction, spill and refault: the guest touches far more than this.
const pressureBytes = 96 << 20

// pagerPageBytes is the fixed 2 MiB production page.
func pagerPageBytes(t testing.TB) int {
	t.Helper()
	return vmmemory.PageSize
}

// residentPages is the arena's slot count: SPROUTFS_FIRECRACKER_RESIDENT_PAGES
// pager pages, or otherwise the given budget in bytes.
func residentPages(t testing.TB, pageBytes, budget int) int {
	t.Helper()
	value := os.Getenv("SPROUTFS_FIRECRACKER_RESIDENT_PAGES")
	if value == "" {
		return budget / pageBytes
	}
	slots, err := strconv.Atoi(value)
	if err != nil || slots < 1 {
		t.Fatalf("invalid resident page budget %q", value)
	}
	return slots
}

func frames(t *testing.T, pid int) (map[uint64]bool, int) {
	t.Helper()
	smaps, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps", pid))
	if err != nil {
		t.Fatal(err)
	}
	managed := false
	for _, line := range strings.Split(string(smaps), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && strings.Contains(fields[0], "-") {
			managed = strings.Contains(line, "memfd:sproutfs-memory")
		}
		if managed && strings.HasPrefix(line, "KernelPageSize:") && (len(fields) != 3 || fields[1] != "2048") {
			t.Fatalf("managed mapping is not backed by 2 MiB HugeTLB pages: %s", line)
		}
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", pid))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	result := make(map[uint64]bool)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range lines {
		if !strings.Contains(line, "memfd:sproutfs-memory") {
			continue
		}
		fields := strings.Fields(line)
		bounds := strings.Split(fields[0], "-")
		start, err := strconv.ParseUint(bounds[0], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		end, err := strconv.ParseUint(bounds[1], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		for address := start; address < end; address += uint64(os.Getpagesize()) {
			var raw [8]byte
			if _, err := f.ReadAt(raw[:], int64(address/uint64(os.Getpagesize())*8)); err != nil {
				t.Fatal(err)
			}
			entry := binary.LittleEndian.Uint64(raw[:])
			if entry>>63 != 0 {
				frame := entry & ((1 << 55) - 1)
				if frame == 0 {
					t.Fatal("physical frame evidence requires privileged pagemap access")
				}
				result[frame] = true
			}
		}
	}
	return result, len(lines)
}

// consoleText is everything a machine's console ring still holds, which is what
// a failing test reports to say what the guest was doing.
func consoleText(p *vmmachine.Process) []byte {
	data, _, _ := p.Console(0, maxConsoleWindow)
	return data
}

// maxConsoleWindow is larger than the ring, so one read takes all of it.
const maxConsoleWindow = 2 << 20

func waitLine(t *testing.T, ctx context.Context, p *vmmachine.Process, want string, offset int64) {
	t.Helper()
	reader := newConsole(p)
	defer reader.close()
	reader.offset = offset
	if _, err := reader.wait(ctx, want); err != nil {
		t.Fatal(err)
	}
}
func command(t *testing.T, ctx context.Context, p *vmmachine.Process, line, want string) {
	t.Helper()
	if err := guestCommand(ctx, p, line, want); err != nil {
		t.Fatal(err)
	}
}
