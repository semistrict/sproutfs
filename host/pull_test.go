package host_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
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
// the counted reads, the pagers, the running guest and the host.
func pulledRun(t *testing.T, diskBytes int64) (*countedObjects, *hostPagers, *machine, *hostHarness) {
	t.Helper()
	h := newSizedHostHarness(t, 2)
	counted := &countedObjects{ObjectStore: h.configs[1].ObjectStore}
	h.configs[1].ObjectStore = counted
	h.configs[1].CacheBytes = 4 << 10
	file, err := h.disks[1].Open(t.Context(), "cache", platform.OpenOptions{Create: true, Truncate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	h.configs[1].Cache = checkpoint.CacheConfig{Disk: file, DiskBytes: diskBytes}
	pagers := newPagerWithConfig(t, h.configs[1].Resources, vmmemory.Config{
		ResidentPages: pullResident, LogicalPages: 2 * pullPages, DirtyPages: pullResident, ReadAheadPages: 1})
	h.configs[1].Pagers = pagers.pagers
	h.start(t)

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
	if err := h.hosts[1].AddPullingMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	return counted, pagers, guest, h
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
	counted, pagers, guest, h := pulledRun(t, 64<<20)
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

// A VM whose checkpoint does not fit in what the page cache's disk has left is
// not pulled at all. It runs all the same, and its faults read the store.
func TestAVMThatDoesNotFitReadsTheObjectStore(t *testing.T) {
	counted, _, guest, h := pulledRun(t, 8<<20)
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
	if err := h.hosts[0].AddPullingMachine("vm-1", source); err != nil {
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
