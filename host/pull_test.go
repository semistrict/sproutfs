package host_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// pullPages is how many pages the pulled VM's disk has, and pullResident how
// many of them its host's pagers keep resident: fewer, so a guest that reads
// them all has evicted the first by the time it reads them again.
const (
	pullPages    = 6
	pullResident = 2
)

var pullVolumes = []volume.VolumeSpec{{Name: "disk", Size: pullPages * migrationPageSize, PageSize: migrationPageSize}}

// pullPage is what the published disk holds in one page: bytes of its own the
// encoder cannot shrink, so the checkpoint costs the disk what it holds.
func pullPage(page uint64) []byte {
	data := make([]byte, migrationPageSize)
	state := 0x9e3779b97f4a7c15 ^ (page + 1)
	for at := 0; at < len(data); at += 8 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(data[at:], state)
	}
	return data
}

// pulledRun publishes a VM from one host and gives a second host, whose object
// reads are counted, a page cache disk of diskBytes and a memory tier no page
// fits in: every page the disk does not serve is a request of the store. The
// second host opens the VM and registers its machine marked to pull. It reports
// the counted reads, the pagers, the running guest, its handle and the host.
func pulledRun(t *testing.T, diskBytes int64) (*countedObjects, *hostPagers, *machine, *volume.VM, *hostHarness) {
	t.Helper()
	return pulledRunWith(t, diskBytes, nil)
}

// pulledRunWith is pulledRun with a last say over the second host's
// configuration before it starts.
func pulledRunWith(t *testing.T, diskBytes int64,
	configure func(h *hostHarness)) (*countedObjects, *hostPagers, *machine, *volume.VM, *hostHarness) {
	t.Helper()
	h := newSizedHostHarness(t, 2)
	counted := &countedObjects{ObjectStore: h.configs[1].ObjectStore}
	h.configs[1].ObjectStore = counted
	h.configs[1].CacheBytes = 4 << 10
	file, err := h.disks[1].Open(t.Context(), "cache", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	h.configs[1].Cache = checkpoint.CacheConfig{Disk: file, DiskBytes: diskBytes, DiskRegionBytes: 8 << 20}
	pagers := newPagerWithConfig(t, h.configs[1].Resources, vmmemory.Config{
		ResidentPages: pullResident, LogicalPages: 2 * pullPages, DirtyPages: pullResident, ReadAheadPages: 1})
	h.configs[1].Pagers = pagers.pagers
	if configure != nil {
		configure(h)
	}
	h.start(t)
	if h.configs[1].CacheList.Read != nil {
		// The host reads the list as it starts, on a goroutine of its own:
		// the pull must find the list it read, not the host alone.
		for h.hosts[1].Status().Caches.Reads == 0 {
			if err := t.Context().Err(); err != nil {
				t.Fatal(err)
			}
			runtime.Gosched()
		}
	}

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", pullVolumes)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(pullPages) {
		if err := vm.Volume("disk").Write(t.Context(), page*migrationPageSize, pullPage(page)); err != nil {
			t.Fatal(err)
		}
	}
	if err := vm.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := vm.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	opened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, opened, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[1].AddMachineWith("vm-1", guest, host.MachineTerms{Pull: true}); err != nil {
		t.Fatal(err)
	}
	return counted, pagers, guest, opened, h
}

// readPulled reads every page of the pulled VM's disk through its guest and
// checks each against what was published.
func readPulled(t *testing.T, guest *machine) {
	t.Helper()
	for page := range uint64(pullPages) {
		if got := guest.load("disk", page); !bytes.Equal(got, pullPage(page)) {
			t.Fatalf("page %d holds %d..., want %d...", page, got[0], pullPage(page)[0])
		}
	}
}

// A VM marked to pull its memory has every page of the checkpoint it started
// from copied onto its host's disk while its guest runs. Once the copy is
// complete its faults make no request of the object store: not a page's first
// fault, and not the fault of a page its pager has evicted since.
func TestAPulledVMFaultsWithoutTheObjectStore(t *testing.T) {
	counted, pagers, guest, _, h := pulledRun(t, 64<<20)
	stats, err := h.hosts[1].WaitPulled(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Done || stats.Err != nil || stats.Bytes == 0 || stats.Pulled != stats.Bytes {
		t.Fatalf("the pull ended at %+v, want the whole checkpoint on the disk", stats)
	}
	counted.reset()
	before, err := pagers.pmem().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	readPulled(t, guest)
	readPulled(t, guest)
	after, err := pagers.pmem().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Six pages through two resident slots, twice over: every fault after the
	// second evicts a page, and the second pass faults every page back in.
	if faults := after.Faults - before.Faults; faults != 2*pullPages {
		t.Fatalf("the guest faulted %d times, want every page twice", faults)
	}
	if evictions := after.Evictions - before.Evictions; evictions != 2*pullPages-pullResident {
		t.Fatalf("the pager evicted %d pages, want %d", evictions, 2*pullPages-pullResident)
	}
	if gets := counted.count(); gets != 0 {
		t.Fatalf("faulting a pulled VM's pages in, and again after they were evicted, made %d requests "+
			"of the object store, want none", gets)
	}
	if disk := h.hosts[1].Status().Cache.Disk; disk.Hits != 2*pullPages+1 || disk.Lost != 0 {
		t.Fatalf("the page cache's disk reports %+v, want every fault and the segment served from it", disk)
	}
}

// A pulled VM keeps what it publishes later on its host's disk too. Its guest
// stores into two pages and a checkpoint publishes them; then every page,
// those two among them, faults in twice through two resident slots, and makes
// no request of the object store.
func TestAPulledVMKeepsItsLaterCheckpointsOnTheDisk(t *testing.T) {
	counted, _, guest, vm, h := pulledRun(t, 64<<20)
	if _, err := h.hosts[1].WaitPulled(t.Context(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	stored := map[uint64]byte{1: 77, 4: 78}
	for page, value := range stored {
		guest.store("disk", page, value)
	}
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if kept := h.hosts[1].Status().Cache.Disk; kept.Entries <= pullPages+1 {
		t.Fatalf("the page cache's disk holds %d entries after the checkpoint, want the pull's %d and the checkpoint's",
			kept.Entries, pullPages+1)
	}
	counted.reset()
	for range 2 {
		for page := range uint64(pullPages) {
			want := pullPage(page)[0]
			if value, wrote := stored[page]; wrote {
				want = value
			}
			if got := guest.load("disk", page); got[0] != want {
				t.Fatalf("page %d holds %d, want %d", page, got[0], want)
			}
		}
	}
	if gets := counted.count(); gets != 0 {
		t.Fatalf("faulting a pulled VM's pages in after a later checkpoint made %d requests of the object store, want none",
			gets)
	}
}

// A VM whose checkpoint does not fit in what the page cache's disk has left is
// not pulled at all. It runs all the same, and its faults read the store.
func TestAVMThatDoesNotFitReadsTheObjectStore(t *testing.T) {
	counted, _, guest, _, h := pulledRun(t, 8<<20)
	stats, err := h.hosts[1].WaitPulled(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Done || !errors.Is(stats.Err, checkpoint.ErrDiskFull) {
		t.Fatalf("the pull ended at %+v, want it refused with %v", stats, checkpoint.ErrDiskFull)
	}
	counted.reset()
	readPulled(t, guest)
	// The segment once, then one request per page.
	if gets := counted.count(); gets != 1+pullPages {
		t.Fatalf("faulting the pages in made %d requests, want %d", gets, 1+pullPages)
	}
	if disk := h.hosts[1].Status().Cache.Disk; disk.UsedBytes != 0 || disk.Hits != 0 {
		t.Fatalf("the page cache's disk reports %+v, want nothing on it", disk)
	}
}

// The mark travels with the VM: a migration's handoff carries it from the
// source, and the destination pulls the checkpoint it opened. A VM without the
// mark carries none.
func TestAMigrationCarriesThePullMark(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 0, 1)
	if err := h.hosts[0].AddMachineWith("vm-1", source, host.MachineTerms{Pull: true}); err != nil {
		t.Fatal(err)
	}
	if _, marked := h.hosts[0].Pulled("vm-1"); !marked {
		t.Fatal("the source does not report its VM as marked to pull")
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	if !handoff.Pull {
		t.Fatalf("the handoff of a VM marked to pull does not carry the mark: %+v", handoff)
	}
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	// The harness's hosts keep no disk, so the destination is refused the pull
	// and says so; the mark is what this is about.
	stats, err := h.hosts[1].WaitPulled(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Done || !errors.Is(stats.Err, checkpoint.ErrNoDisk) {
		t.Fatalf("the destination's pull ended at %+v, want it refused with %v", stats, checkpoint.ErrNoDisk)
	}
	if err := h.hosts[0].ReleaseMigrated("vm-1"); err != nil {
		t.Fatal(err)
	}

	unmarked, err := h.hosts[1].Volumes().Create(t.Context(), "vm-2", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := newMachine(t, pagers[1], unmarked, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[1].AddMachine("vm-2", plain); err != nil {
		t.Fatal(err)
	}
	if _, marked := h.hosts[1].Pulled("vm-2"); marked {
		t.Fatal("a VM registered without the mark reports a pull")
	}
	if _, err := h.hosts[1].WaitPulled(t.Context(), "vm-2"); !errors.Is(err, host.ErrNotPulling) {
		t.Fatalf("waiting on the pull of a VM without the mark returned %v, want %v", err, host.ErrNotPulling)
	}
}

// The disk limiter sets the page cache's disk and takes it back. A pulled VM's
// pages are on its host's disk while the disk has room; once the disk fills
// from outside the host, the limiter's share falls to nothing, the cache gives
// every region back, and the VM's faults read the object store again.
func TestTheDiskLimiterTakesThePageCachesDiskBack(t *testing.T) {
	{
		var limiter *resource.DiskLimiter
		var clock *sim.Clock
		counted, _, guest, _, h := pulledRunWith(t, 0, func(h *hostHarness) {
			var err error
			clock = h.runtime.NewClock("limiter")
			limiter, err = resource.NewDiskLimiter(t.Context(), resource.DiskLimiterConfig{Space: h.disks[1],
				Goal: resource.DiskGoal{FreeBytes: 64 << 20}, Region: 8 << 20, Clock: clock})
			if err != nil {
				t.Fatal(err)
			}
			h.configs[1].DiskLimiter = limiter
		})
		defer limiter.Close()
		stats, err := h.hosts[1].WaitPulled(t.Context(), "vm-1")
		if err != nil {
			t.Fatal(err)
		}
		if !stats.Done || stats.Err != nil || stats.Pulled != stats.Bytes {
			t.Fatalf("the pull ended at %+v, want the whole checkpoint on the disk", stats)
		}
		// Six incompressible pages of 2 MiB take two regions of 8 MiB.
		if disk := h.hosts[1].Status().Cache.Disk; disk.UsedBytes != 16<<20 || disk.LimitBytes != limiter.CacheShare() {
			t.Fatalf("the page cache's disk holds %d bytes of a share of %d, want two regions of the limiter's %d",
				disk.UsedBytes, disk.LimitBytes, limiter.CacheShare())
		}

		// The limiter acts on readings smoothed over a minute, so the disk stays
		// full for ten minutes of the limiter's clock.
		h.disks[1].SetOutsideBytes(limiter.Status().TotalBytes)
		for range 10 {
			clock.Advance(time.Minute)
			clock.Settle()
			if err := limiter.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if disk := h.hosts[1].Status().Cache.Disk; disk.UsedBytes != 0 || disk.LimitBytes != 0 {
			t.Fatalf("the page cache's disk holds %d bytes of a share of %d once the disk is full, want none of none",
				disk.UsedBytes, disk.LimitBytes)
		}
		counted.reset()
		readPulled(t, guest)
		if gets := counted.count(); gets != pullPages+1 {
			t.Fatalf("faulting the pages in after the disk was taken back made %d requests of the object store, "+
				"want one a page and one for the segment, %d", gets, pullPages+1)
		}
	}
}

// The page cache's disk outlives its host process. A pulled VM's pages, two
// regions of 8 MiB, are on its host's disk when the host stops. A new host
// process starts over the same disk with a new disk limiter, as a restarted
// supervisor does, and reads the disk back. Its free-space goal leaves 20 MiB
// above the floor with the file counted as the cache's, a share of 32 MiB,
// which keeps both regions. Were the file counted as another writer's, the
// share would be 16 MiB and the cache would give a region back. The VM opened
// on the new host faults every page in from the disk, and makes no request of
// the object store for any of them.
func TestAPulledVMsPagesOutliveItsHostsRestart(t *testing.T) {
	deployment := checkpoint.CacheDeployment{Store: "sim", Bucket: "host-cluster", Prefix: "host-cluster/"}
	startLimiter := func(h *hostHarness, file platform.File, goal resource.DiskGoal, clock string) *resource.DiskLimiter {
		limiter, err := resource.NewDiskLimiter(t.Context(), resource.DiskLimiterConfig{Space: h.disks[1],
			Goal: goal, Region: 8 << 20, Clock: h.runtime.NewClock(clock),
			CacheFile: file.(platform.FileAllocation).Allocated})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(limiter.Close)
		h.configs[1].DiskLimiter = limiter
		return limiter
	}
	counted, _, _, _, h := pulledRunWith(t, 0, func(h *hostHarness) {
		startLimiter(h, h.configs[1].Cache.Disk, resource.DiskGoal{FreeBytes: 64 << 20}, "limiter")
		h.configs[1].Cache.Deployment = deployment
	})
	if stats, err := h.hosts[1].WaitPulled(t.Context(), "vm-1"); err != nil || !stats.Done || stats.Err != nil {
		t.Fatalf("the pull ended at %+v, %v, want the whole checkpoint on the disk", stats, err)
	}
	before := h.hosts[1].Status().Cache.Disk
	if before.UsedBytes != 16<<20 {
		t.Fatalf("the pull left %d bytes on the disk, want two regions of 8 MiB", before.UsedBytes)
	}

	h.stop(t, 1)
	h.configs[1].DiskLimiter.Close()
	file, err := h.disks[1].Open(t.Context(), "cache", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	h.configs[1].Cache.Disk = file
	space, err := h.disks[1].Space(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	limiter := startLimiter(h, file, resource.DiskGoal{FreeBytes: int64(space.Available) - 20<<20},
		"limiter-after-restart")
	pagers := newPagerWithConfig(t, h.configs[1].Resources, vmmemory.Config{
		ResidentPages: pullResident, LogicalPages: 2 * pullPages, DirtyPages: pullResident, ReadAheadPages: 1})
	h.configs[1].Pagers = pagers.pagers
	h.launch(t, 1)

	after := h.hosts[1].Status().Cache.Disk
	if after.Identity != before.Identity || after.UsedBytes != before.UsedBytes || after.Entries != before.Entries ||
		after.FromTables != 2 || after.GivenBackOnOpen != 0 || after.Evicted != 0 {
		t.Fatalf("the restarted host's disk reports %+v, want what it held before, %+v, read back from its tables",
			after, before)
	}
	// 36 MiB of room above the floor, less a band of a fifth of the 20 the
	// cache has left of it. The file also holds its header's block, 4 KiB,
	// which the limiter counted as the cache's too.
	if share := limiter.CacheShare(); share != 32<<20+4<<10 {
		t.Fatalf("the new limiter gives the cache a share of %d bytes, want 32 MiB and 4 KiB", share)
	}
	opened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, opened, nil)
	if err != nil {
		t.Fatal(err)
	}
	counted.reset()
	readPulled(t, guest)
	if gets := counted.count(); gets != 0 {
		t.Fatalf("faulting a pulled VM's pages in after its host restarted made %d requests of the object store, "+
			"want none", gets)
	}
}
