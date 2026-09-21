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
	"reflect"
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

// bothKinds gives a machine the same pager for both kinds of region, which a
// test whose subject is neither the geometry nor the memory a guest holds uses
// to avoid a second arena. A host assembles one pager per kind; hostPagers
// below is that assembly, and it is what the full-guest suites run on.
func bothKinds(pager *vmmemory.Host) vmmemory.Pagers {
	return vmmemory.Pagers{Ram: pager, Pmem: pager}
}

// hostPagers is a host's two pagers as a deployment assembles them: RAM's
// 4 KiB page over an arena of ordinary memory, PMEM's 2 MiB page over one of
// the node's HugeTLB pool. The pages are internal/host's choice; this package
// cannot import it, since a host is built on a machine.
type hostPagers struct {
	pagers   vmmemory.Pagers
	arenas   []*vmmemory.LinuxArena
	capacity uint64
}

// newHostPagers gives each pager an arena of its own size and a logical cap and
// dirty budget of the given bytes, each converted into that pager's own page.
// Everything is stated in bytes because a number of pages would mean different
// amounts of memory in the two, and the arenas are stated apart because what a
// guest's memory needs of one says nothing about what its disks need of the
// other: a test that pressed both with one number would be pressing whichever
// of them happened to be smaller in its own pages.
func newHostPagers(t testing.TB, ctx context.Context, ramArenaBytes, pmemArenaBytes, logicalBytes, dirtyBytes int) *hostPagers {
	t.Helper()
	p := &hostPagers{}
	for _, kind := range []vmmemory.RegionKind{vmmemory.Ram, vmmemory.Pmem} {
		page, arenaBytes := uint64(checkpoint.PageSize4KiB), ramArenaBytes
		if kind == vmmemory.Pmem {
			page, arenaBytes = checkpoint.PageSize2MiB, pmemArenaBytes
		}
		arena, err := vmmemory.NewLinuxArena(int(uint64(arenaBytes)/page), page)
		if err != nil {
			t.Fatalf("%s arena: %v", kind, err)
		}
		t.Cleanup(func() { _ = arena.Close() })
		disk, err := adapters.NewDisk(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		spill, err := disk.Open(ctx, "spill", platform.OpenOptions{Create: true, Exclusive: true, Permissions: 0o600})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = spill.Close() })
		pager, err := vmmemory.New(ctx, testresource.New(), vmmemory.Config{PageSize: page,
			ResidentPages: int(uint64(arenaBytes) / page),
			LogicalPages:  int(uint64(logicalBytes) / page),
			DirtyPages:    int(uint64(dirtyBytes) / page),
			// A read-ahead run is stated in bytes, as a deployment states it,
			// because it is a buffer: 2 MiB is the run the page-geometry plan
			// targets, which is one PMEM page and 512 RAM pages loaded into
			// consecutive slots and installed as one mapping. Write-ahead is
			// one page — the plan's decision for RAM, and what these suites
			// have always given PMEM.
			ReadAheadPages:  int(checkpoint.PageSize2MiB / page),
			WriteAheadPages: 1}, arena, spill)
		if err != nil {
			t.Fatalf("%s pager: %v", kind, err)
		}
		if kind == vmmemory.Ram {
			p.pagers.Ram = pager
		} else {
			p.pagers.Pmem = pager
		}
		p.arenas = append(p.arenas, arena)
		p.capacity += uint64(arenaBytes) / page * page
	}
	return p
}

// Stats is both pagers' counters added. The event counters add because they
// count events; the page counts are added only to ask whether both pagers are
// empty, which is a question with no unit. A test asserting a number of pages
// asks the pager of the kind it means.
func (p *hostPagers) Stats(ctx context.Context) (vmmemory.Stats, error) {
	var total vmmemory.Stats
	for _, pager := range []*vmmemory.Host{p.pagers.Ram, p.pagers.Pmem} {
		s, err := pager.Stats(ctx)
		if err != nil {
			return vmmemory.Stats{}, err
		}
		addStats(&total, s)
	}
	return total, nil
}

// addStats adds every counted field of one pager's statistics into another's,
// leaving the latency histograms alone: a histogram of two pagers' spans put
// together says nothing either of them said. It is reflective so that a field
// added to Stats is included here without this test being edited into
// agreement with it.
func addStats(total *vmmemory.Stats, one vmmemory.Stats) {
	destination, source := reflect.ValueOf(total).Elem(), reflect.ValueOf(one)
	for i := range destination.NumField() {
		switch destination.Field(i).Kind() {
		case reflect.Uint64:
			destination.Field(i).SetUint(destination.Field(i).Uint() + source.Field(i).Uint())
		case reflect.Int:
			destination.Field(i).SetInt(destination.Field(i).Int() + source.Field(i).Int())
		}
	}
}

// SharedBytes is what both pagers' arenas hold and what their regions map,
// which are bytes and so add across pagers of different pages.
func (p *hostPagers) SharedBytes(ctx context.Context) (unique, mapped uint64, err error) {
	for _, pager := range []*vmmemory.Host{p.pagers.Ram, p.pagers.Pmem} {
		sharing, err := pager.Sharing(ctx)
		if err != nil {
			return 0, 0, err
		}
		for _, gauge := range []vmmemory.Sharing{sharing.Ram, sharing.Pmem} {
			unique += gauge.UniqueBytes
			mapped += gauge.MappedBytes
		}
	}
	return unique, mapped, nil
}

// AllocatedBytes is what both arenas physically hold.
func (p *hostPagers) AllocatedBytes() (uint64, error) {
	var total uint64
	for _, arena := range p.arenas {
		bytes, err := arena.AllocatedBytes()
		if err != nil {
			return 0, err
		}
		total += bytes
	}
	return total, nil
}

const (
	// guestConsoleArgs keeps the serial console, which carries the guest
	// init's protocol lines, and silences the kernel's own messages. The
	// console is an emulated UART that every kernel line crosses
	// synchronously; at the default log level those lines are half of a cold
	// boot's time to init. Level 1 still prints panics and soft lockups.
	// The i8042 flags are Firecracker's own: left to probe that controller for
	// a keyboard and a mouse, the kernel stalls for half a second before it
	// mounts the root, on the managed side and the plain one alike.
	guestConsoleArgs = "console=ttyS0 quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
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
		// Each volume is published in the page of the pager that maps it, which
		// is what a host creates them with.
		{Name: vmmachine.RAMVolume, Size: 128 << 20, PageSize: checkpoint.PageSize4KiB},
		{Name: "root", Size: 64 << 20, PageSize: checkpoint.PageSize2MiB},
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
	arenaBytes := residentPages(t, pageBytes, 192<<20) * pageBytes
	// The production assembly: a 4 KiB RAM pager over ordinary memory beside a
	// 2 MiB PMEM pager over the node's HugeTLB pool, each with an arena of the
	// same number of bytes. The guest's RAM is what this suite puts under
	// pressure, and at 4 KiB its arena is no longer a share of the pool.
	host := newHostPagers(t, ctx, arenaBytes, arenaBytes, 384<<20, 384<<20)
	vmConfig := vmmachine.Config{Binary: binaryPath, SeccompFilter: os.Getenv("SPROUTFS_FIRECRACKER_SECCOMP"), KernelPath: os.Getenv("SPROUTFS_FIRECRACKER_KERNEL"), InitrdPath: os.Getenv("SPROUTFS_FIRECRACKER_INITRD"), BootArgs: guestPmemBootArgs, Pagers: host.pagers, VM: source, Pmem: []vmmachine.Pmem{{ID: "root", Root: true}}, VCPUs: 1, Connection: vmmemory.ConnectionConfig{QueuePages: 4096, CommandTimeout: 2 * time.Minute, VerifyInterval: time.Second}}
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
	// is never asked. The DAX store is this host's pages alone until a checkpoint
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
	// vCPUs resume, and the checkpoint uploads the sealed pages behind them.
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
	// A host that never held the capture rebuilds the point from the published
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
	// Inherited pages are mapped without any read. The VMM still touches a few
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
	left, leftVMAs := residentPFNs(t, p.PID())
	right, rightVMAs := residentPFNs(t, fp.PID())
	shared := 0
	for page := range left {
		if right[page] {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("running fork shares no physical backing pages with its source")
	}
	if arenaBytes <= pressureBytes {
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
	allocated, err := host.AllocatedBytes()
	if err != nil || allocated > host.capacity {
		t.Fatalf("arena exceeded physical budget: %d %v", allocated, err)
	}
	stats, err = host.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if arenaBytes <= pressureBytes && (stats.Evictions == 0 || stats.Spills == 0 || stats.SpillRefaults == 0) {
		t.Fatalf("pressure run did not exercise live spill/refault: %+v", stats)
	}
	t.Logf("memory pages=%+v arena_bytes=%d shared_pages=%d source_vmas=%d fork_vmas=%d", stats, allocated, shared, leftVMAs, rightVMAs)
	// The source half of a live migration: stop the guest and capture its state,
	// uploading nothing. The pages this host holds keep serving the
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
	// A stopped source still holds its pages, which is what the destination
	// fetches from before it ever reads an object.
	ram := regions[vmmachine.RAMVolume]
	resident, err := ram.Resident()
	if err != nil {
		t.Fatalf("listing what the stopped source holds: %v", err)
	}
	if len(resident) == 0 {
		t.Fatal("the stopped source holds no page to serve its destination")
	}
	// A page of this region is a page of the pager that holds it, which for RAM
	// is not the PMEM page the arena budgets above are stated in.
	sample := make([]byte, ram.PageSize())
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
	if stats.LogicalPages != 0 || stats.DirtyPages != 0 {
		t.Fatalf("stopped VMM mappings retained: %+v", stats)
	}
	// Nothing maps anything any more, which is what a stopped VMM must leave.
	// What the arena may still hold is clean pages nothing maps — the pages
	// stores copied away from, which stay under the identity they are published
	// by so the next region naming one maps it instead of reading it, and which
	// the next reclaim short of a slot takes like any other clean page. Those
	// are memory, so the arena's blocks are exactly them and no more.
	unique, mappedBytes, err := host.SharedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mappedBytes != 0 {
		t.Fatalf("stopped VMM mappings retained %d bytes of arena: %+v", mappedBytes, stats)
	}
	allocated, err = host.AllocatedBytes()
	if err != nil || allocated != unique {
		t.Fatalf("the arena holds %d bytes for %d bytes of unmapped clean pages: %v",
			allocated, unique, err)
	}
	entries, err = os.ReadDir(vmConfig.Scratch.Directory())
	if err != nil || len(entries) != 0 {
		t.Fatalf("stopped VMM artifacts remain: %v %v", entries, err)
	}
}

// pressureBytes is the resident budget at or below which the full-guest suite
// must see eviction, spill and refault: the guest touches far more than this.
const pressureBytes = 96 << 20

// pagerPageBytes is the PMEM pager's 2 MiB page, which is the larger of the
// two a host runs and so the unit the suites' arena and page-server budgets are
// stated in. A RAM page is 4 KiB; a test that means one asks its region.
func pagerPageBytes(t testing.TB) int {
	t.Helper()
	return checkpoint.PageSize2MiB
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

func residentPFNs(t *testing.T, pid int) (map[uint64]bool, int) {
	t.Helper()
	smaps, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps", pid))
	if err != nil {
		t.Fatal(err)
	}
	// An arena's memfd is named for the page it is made of, and this is where
	// the kernel is asked whether it agrees: a guest's RAM is on ordinary 4 KiB
	// memory and its disk on 2 MiB HugeTLB pages, and a mapping that is not
	// what its arena says it is would share nothing the way this test means.
	want := ""
	for _, line := range strings.Split(string(smaps), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && strings.Contains(fields[0], "-") {
			want = ""
			for _, arena := range []struct{ name, kernelPage string }{
				{"memfd:sproutfs-memory-4k", "4"},
				{"memfd:sproutfs-memory-2048k", "2048"},
			} {
				if strings.Contains(line, arena.name) {
					want = arena.kernelPage
				}
			}
		}
		if want != "" && strings.HasPrefix(line, "KernelPageSize:") && (len(fields) != 3 || fields[1] != want) {
			t.Fatalf("a managed mapping is not on the memory its arena is made of, want %s kB: %s", want, line)
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
				page := entry & ((1 << 55) - 1)
				if page == 0 {
					t.Fatal("physical page evidence requires privileged pagemap access")
				}
				result[page] = true
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
