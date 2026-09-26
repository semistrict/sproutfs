//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testnet"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// TestFirecrackerLiveMigration moves a running Firecracker guest between two
// pagers and two volume managers in one process, over loopback TCP. The guest
// keeps storing into its RAM and its DAX disk
// until the vCPUs stop, and comes back on the destination with its counters
// intact. Those stores are the source's pages alone — a migration publishes
// nothing — so what the destination fetches from the source's page server is
// what makes it whole.
func TestFirecrackerLiveMigration(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	c := newMigrationCluster(t, ctx)

	source, err := c.source.Create(ctx, "migrant", []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: 128 << 20, PageSize: ramPageBytes(t)},
		{Name: "root", Size: 64 << 20, PageSize: checkpoint.PageSize2MiB},
	})
	if err != nil {
		t.Fatal(err)
	}
	loadRootImage(t, ctx, source.Volume("root"))
	if err := source.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}

	sourcePager := newMigrationPager(t, ctx)
	config := migrationConfig(t, binaryPath, sourcePager, source)
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)

	// Both hosts are one process, so every memory region's connections and the
	// comparison's come from the same peer address and share one budget.
	pages, err := vmmigrate.NewPageSource(ctx, vmmigrate.SourceConfig{Network: c.network,
		Address: "source-pages", PageSize: pagerPageBytes(t),
		MaxConnectionsPerPeer: 32, MaxBytesInFlightPerPeer: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pages.Close() })

	// The guest stores into its RAM and its DAX disk until the vCPUs stop. A
	// migration has no phase that runs while the guest does, so the test waits
	// for rounds of its own: the point of the migration is that these stores are
	// nowhere but this host's pages when it is stopped.
	work := driveGuest(ctx, p)
	if err := work.rounds(ctx, 3); err != nil {
		raw := consoleText(p)
		t.Fatalf("the guest did not store before the migration: %v\n%s", err, raw)
	}
	handoff, err := vmmigrate.Migrate(ctx, source, stoppingWith(p, work.wait), pages, vmmigrate.Options{})
	if err != nil {
		raw := consoleText(p)
		t.Fatalf("migrate: %v\n%s", err, raw)
	}
	ram, disk, err := work.result()
	if err != nil {
		t.Fatal(err)
	}
	if ram == 0 || disk == 0 {
		t.Fatalf("the guest acknowledged ram=%d disk=%d before the migration", ram, disk)
	}
	if status := source.Status(); !status.HandedOff {
		t.Fatalf("the source did not hand the VM off: %+v", status)
	}
	final := source.Status().Checkpoint

	// The destination opens the VM the source released — the control record and
	// one index read, no page — and starts the VMM from the captured state.
	destination, err := c.destination.Open(ctx, "migrant")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close(context.Background()) })
	if got := destination.Status().Checkpoint; got != final {
		t.Fatalf("the destination opened at %v, want the handed-off %v", got, final)
	}
	// Every memory region of the destination attaches through the host that still holds
	// its pages, which is what puts the source's page server on the VMM's own
	// fault path rather than beside it. The volumes stay the memory regions' identity.
	destinationPager := newMigrationPager(t, ctx)
	peers := make(map[string]*vmmigrate.PeerBacking, len(handoff.MemoryRegions))
	backings := make(map[string]vmmemory.Backing, len(handoff.MemoryRegions))
	for _, memoryRegion := range handoff.MemoryRegions {
		v := destination.Volume(memoryRegion.Name)
		if v == nil {
			t.Fatalf("the destination opened no volume named %s", memoryRegion.Name)
		}
		backing := peerBacking(t, c, handoff, v)
		peers[memoryRegion.Name], backings[memoryRegion.Name] = backing, backing
	}
	restore := migrationConfig(t, binaryPath, destinationPager, destination)
	restore.RestoreState = handoff.State
	restore.Backings = backings
	fp, err := vmmachine.Start(ctx, restore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fp.Close() })
	ready := time.Now()

	// The source still holds every page it stopped with, and serves them over
	// the same loopback network the hosts use. The pages it never published
	// are the ones only it has, and what it serves for every other page is
	// exactly what the destination's own log holds.
	resident, comparedPeer, comparedVolume := comparePeerAndVolume(t, ctx, c, handoff, destination)

	ramMemoryRegion := fp.MemoryRegions()[vmmachine.RAMVolume]
	if ramMemoryRegion == nil {
		t.Fatal("the destination maps no memory region for its RAM volume")
	}
	beforeFault := peers[vmmachine.RAMVolume].Stats()
	// A page the guest has not reached yet arrives the same way, which pins the
	// count to one fault rather than to whatever the guest happened to touch.
	if err := ramMemoryRegion.Fault(ctx, absentPage(t, ctx, resident, ramMemoryRegion, peers[vmmachine.RAMVolume]), false); err != nil {
		t.Fatal(err)
	}
	faulted := peers[vmmachine.RAMVolume].Stats()
	if faulted.PeerPages <= beforeFault.PeerPages {
		t.Fatalf("a fault on a page only the source holds read %+v, not the source", faulted)
	}

	// A store is as likely as a read to be this host's first touch of a page
	// only the source holds, and it takes that page by a path of its own: the
	// copy-on-write read, which asks the same page server for the same bytes
	// and leaves them here as this host's own dirty state. The source no longer
	// holds the only copy of that page, and the account below is what has to
	// know it — a source told otherwise never stops serving, and the
	// destination is told its guest's memory is part missing.
	stored := unheldUnpublished(t, unpublishedOf(t, handoff, vmmachine.RAMVolume), ramMemoryRegion)
	if err := ramMemoryRegion.Fault(ctx, stored, true); err != nil {
		t.Fatalf("storing into %s page %d, which no checkpoint holds: %v", vmmachine.RAMVolume, stored, err)
	}

	beforeGuest := peers[vmmachine.RAMVolume].Stats()
	if err := fp.Release(ctx); err != nil {
		t.Fatal(err)
	}
	resumed := time.Now()
	// The guest continues on the destination with the counters the source left,
	// reading them back through pages this host does not have: every one of
	// those faults went to the source over the host network.
	command(t, ctx, fp, "read\n", fmt.Sprintf("SPROUTFS_VALUE ram=%d disk=%d", ram, disk))
	command(t, ctx, fp, fmt.Sprintf("ram %d\n", ram+1), fmt.Sprintf("SPROUTFS_RAM ram=%d", ram+1))
	command(t, ctx, fp, fmt.Sprintf("write %d\n", disk+1), fmt.Sprintf("SPROUTFS_FLUSH disk=%d", disk+1))
	command(t, ctx, fp, "read\n", fmt.Sprintf("SPROUTFS_VALUE ram=%d disk=%d", ram+1, disk+1))

	guest := peers[vmmachine.RAMVolume].Stats()
	if guest.PeerPages <= beforeGuest.PeerPages || guest.FellBack {
		t.Fatalf("the resumed guest faulted no RAM page from the source: before=%+v after=%+v", beforeGuest, guest)
	}

	// The post-copy's own pass: every page the handoff named is fetched, by the
	// guest's faults or behind them, and when it is done not one of them is
	// still only on the source. That is the whole of what lets a source stop
	// serving, and it counts the pages a store took as much as the ones a read
	// did — those bytes are just as much here.
	for _, memoryRegion := range handoff.MemoryRegions {
		mapped := fp.MemoryRegions()[memoryRegion.Name]
		if mapped == nil {
			t.Fatalf("the destination maps no memory region for %s", memoryRegion.Name)
		}
		for _, run := range memoryRegion.Unpublished {
			for page := run.First; page < run.First+uint64(run.Count); page++ {
				if err := mapped.Fault(ctx, page, false); err != nil {
					t.Fatalf("fetching %s page %d, which no checkpoint holds: %v", memoryRegion.Name, page, err)
				}
			}
		}
		if left := peers[memoryRegion.Name].Unfetched(); left != 0 {
			t.Fatalf("%s still owes the source %d pages no checkpoint holds, every one of which was fetched", memoryRegion.Name, left)
		}
	}
	if err := pages.Release(handoff.VMID); err != nil {
		t.Fatalf("the source refused to release a VM it had served every page of: %v", err)
	}

	// A source that stops serving sends the destination to its own log for
	// good: the same fault path now reads the volume, once, and never asks that
	// source again. This host is giving the VM up rather than accounting for
	// what the destination fetched, which release_test.go covers, so the pages
	// go either way.
	pages.Discard(handoff.VMID)
	// Nothing the source serves can be counted from here, so this is the last
	// peer page this memory region will ever have.
	atRelease := peers[vmmachine.RAMVolume].Stats()
	// At 2 MiB granularity the resumed guest can hold every stored RAM page.
	// Exercise a cold backing request directly so this does not depend on a
	// particular guest working set leaving an unfaulted page behind. The page
	// is one the checkpoint holds: a page no checkpoint holds is not the
	// volume's to answer, and a load of one this host never installed fails
	// rather than handing the guest bytes from before its own write.
	page := publishedPage(t, handoff, destination.Volume(vmmachine.RAMVolume))
	ramPage := destination.Volume(vmmachine.RAMVolume).PageSize()
	data := make([]byte, ramPage)
	if err := peers[vmmachine.RAMVolume].Load(ctx, page*ramPage, data); err != nil {
		t.Fatal(err)
	}
	expected := make([]byte, len(data))
	if err := destination.Volume(vmmachine.RAMVolume).Read(ctx, page*ramPage, expected); err != nil || !bytes.Equal(data, expected) {
		t.Fatalf("released source fallback returned different RAM bytes: %v", err)
	}
	released := peers[vmmachine.RAMVolume].Stats()
	if released.PeerPages != atRelease.PeerPages {
		t.Fatalf("a released source kept serving: %+v", released)
	}
	if released.VolumePages <= atRelease.VolumePages || !released.FellBack {
		t.Fatalf("the destination did not fall back to its own volume: %+v", released)
	}
	// The guest keeps its counters once its pages come from the log alone.
	command(t, ctx, fp, "read\n", fmt.Sprintf("SPROUTFS_VALUE ram=%d disk=%d", ram+1, disk+1))

	stats, err := sourcePager.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	served := pages.Stats()
	t.Logf("migration pause: stop_to_ready=%s stop_to_resume=%s (guest stores ram=%d disk=%d)",
		ready.Sub(handoff.PausedAt), resumed.Sub(handoff.PausedAt), ram, disk)
	for _, memoryRegion := range handoff.MemoryRegions {
		unpublished := 0
		for _, run := range memoryRegion.Unpublished {
			unpublished += run.Count
		}
		t.Logf("migration handoff %s: unpublished_pages=%d", memoryRegion.Name, unpublished)
	}
	for name, backing := range peers {
		memoryRegion := backing.Stats()
		t.Logf("migration memory region %s: peer_pages=%d volume_pages=%d requests=%d fell_back=%t",
			name, memoryRegion.PeerPages, memoryRegion.VolumePages, memoryRegion.Requests, memoryRegion.FellBack)
		latency := memoryRegion.Latency
		t.Logf("migration memory region %s latency: faults=%d p50<=%s p99<=%s max=%s wait_p99<=%s stream=%d p99<=%s wait_p99<=%s",
			name, latency.Fault.Count, time.Duration(latency.Fault.QuantileUpperNS(0.5)),
			time.Duration(latency.Fault.QuantileUpperNS(0.99)), time.Duration(latency.Fault.MaxNS),
			time.Duration(latency.FaultWait.QuantileUpperNS(0.99)), latency.Stream.Count,
			time.Duration(latency.Stream.QuantileUpperNS(0.99)), time.Duration(latency.StreamWait.QuantileUpperNS(0.99)))
	}
	t.Logf("migration page server: served=%d absent=%d requests=%d compared_peer_pages=%d compared_volume_pages=%d source_resident=%d",
		served.Served, served.Absent, served.Requests, comparedPeer, comparedVolume, stats.ResidentPages)

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	// The source keeps nothing of the VM it handed over: no memory region, no
	// page a checkpoint does not hold. What its arena may still hold is clean
	// pages nothing maps, under the identity their checkpoint published them
	// by, so the next memory region naming one maps it instead of reading it.
	// Those are a cache, and the arena's blocks are exactly them.
	after, err := sourcePager.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.LogicalPages != 0 || after.DirtyPages != 0 {
		t.Fatalf("the migrated source still maps or owns pages: %+v", after)
	}
	unique, mappedBytes, err := sourcePager.SharedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mappedBytes != 0 {
		t.Fatalf("the migrated source's stopped VMM left %d bytes mapped", mappedBytes)
	}
	if allocated, err := sourcePager.AllocatedBytes(); err != nil || allocated != unique {
		t.Fatalf("the migrated source's arena holds %d bytes for %d bytes of unmapped clean pages: %v",
			allocated, unique, err)
	}
}

// migrationCluster is one process holding both hosts of a migration over
// loopback TCP, with one volume manager per host.
type migrationCluster struct {
	network             *testnet.Network
	source, destination *volume.Manager
	// store is the one both managers publish into, which a test reads a
	// checkpoint out of directly.
	store *checkpoint.Store
}

func newMigrationCluster(t *testing.T, ctx context.Context) *migrationCluster {
	t.Helper()
	network := testnet.New()
	runtime := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Nanosecond,
		GetLatency: time.Nanosecond, PutLatency: time.Nanosecond, ListLatency: time.Nanosecond,
		DeleteLatency: time.Nanosecond, BytesPerSecond: 1 << 50}})
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: runtime.ObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	c := &migrationCluster{network: network, store: store}
	manager := func(host string) *volume.Manager {
		client, err := control.NewClient(control.Config{ObjectStore: runtime.ObjectStore()})
		if err != nil {
			t.Fatal(err)
		}
		store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: runtime.ObjectStore()})
		if err != nil {
			t.Fatal(err)
		}
		m, err := volume.NewManager(volume.Config{Control: client, Store: store})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		return m
	}
	c.source, c.destination = manager("source-host"), manager("destination-host")
	return c
}

// newMigrationPager builds one host's pagers, sized for the one VM a migration
// moves. Both hosts of this test live in one process, so each gets arenas of
// its own.
func newMigrationPager(t *testing.T, ctx context.Context) *hostPagers {
	t.Helper()
	// The single-guest resident budget is the suite's own knob, so these pagers
	// take it; a host sized for several guests states its own.
	budget := residentPages(t, pagerPageBytes(t), 128<<20) * pagerPageBytes(t)
	return newSizedMigrationPager(t, ctx, budget, budget, 384<<20, 384<<20)
}

// newSizedMigrationPager builds one host's pagers with a RAM arena and a PMEM
// arena of their own sizes, room for logicalBytes of mapped memory region and
// dirtyBytes of private state no checkpoint has published. A host taking in
// more than one VM needs a larger logical budget than the single-VM default; an
// arena smaller than what its kind of memory region maps puts eviction, spill and
// refault on every path of that kind, and a dirty budget no larger than the
// arenas is what a deployment actually gives one. Every budget is in bytes
// because the two pagers count them in their own pages.
func newSizedMigrationPager(t *testing.T, ctx context.Context,
	ramArenaBytes, pmemArenaBytes, logicalBytes, dirtyBytes int) *hostPagers {
	t.Helper()
	return newHostPagers(t, ctx, ramArenaBytes, pmemArenaBytes, logicalBytes, dirtyBytes)
}

func migrationConfig(t *testing.T, binary string, pagers *hostPagers, vm *volume.VM) vmmachine.Config {
	t.Helper()
	return vmmachine.Config{Starter: &vmmachine.Firecracker{Binary: binary, SeccompFilter: os.Getenv("SPROUTFS_FIRECRACKER_SECCOMP"), Kernel: os.Getenv("SPROUTFS_FIRECRACKER_KERNEL"), Initrd: os.Getenv("SPROUTFS_FIRECRACKER_INITRD"), BootArgs: guestPmemBootArgs, VCPUs: 1},
		Pagers: pagers.pagers, VM: vm, Pmem: []vmmachine.Pmem{{ID: "root", Root: true}},
		Scratch: mustScratch(t),
		Connection: vmmemory.ConnectionConfig{QueuePages: 4096, CommandTimeout: 2 * time.Minute,
			VerifyInterval: time.Second}}
}

// loadRootImage writes the qualification root filesystem into the VM's PMEM
// volume, skipping the holes an empty image is mostly made of.
func loadRootImage(t *testing.T, ctx context.Context, pmem *volume.Volume) {
	t.Helper()
	rootImage, err := os.Open(os.Getenv("SPROUTFS_FIRECRACKER_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	defer rootImage.Close()
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
	if offset != pmem.Size() {
		t.Fatalf("root image length %d", offset)
	}
}

// stopped wraps the supervisor so the workload driver stops with the vCPUs: the
// pause of a migration begins at Stop, and a guest whose driver was still
// sending would have nobody to answer it.
type stopped struct {
	*vmmachine.Process
	quiesce func()
}

func stoppingWith(p *vmmachine.Process, quiesce func()) vmmigrate.Runtime {
	return stopped{Process: p, quiesce: quiesce}
}

func (s stopped) Stop(ctx context.Context) ([]byte, error) {
	s.quiesce()
	return s.Process.Stop(ctx)
}

// workload is the guest's own dirty rate: one RAM store and one DAX store per
// round, acknowledged on the console before the next.
type workload struct {
	stop chan struct{}
	done chan struct{}

	mu        sync.Mutex
	ram, disk uint64
	err       error
}

func driveGuest(ctx context.Context, p *vmmachine.Process) *workload {
	w := &workload{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for value := uint64(1000); ; value++ {
			select {
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			default:
			}
			if err := guestCommand(ctx, p, fmt.Sprintf("ram %d\n", value), fmt.Sprintf("SPROUTFS_RAM ram=%d", value)); err != nil {
				w.fail(err)
				return
			}
			w.mu.Lock()
			w.ram = value
			w.mu.Unlock()
			if err := guestCommand(ctx, p, fmt.Sprintf("dirty %d\n", value), fmt.Sprintf("SPROUTFS_DIRTY disk=%d", value)); err != nil {
				w.fail(err)
				return
			}
			w.mu.Lock()
			w.disk = value
			w.mu.Unlock()
		}
	}()
	return w
}

// rounds waits until the guest has acknowledged count complete rounds, each one
// a RAM store and a DAX store. It is what a test does before stopping the guest,
// so the stop has a dirty set to leave behind.
func (w *workload) rounds(ctx context.Context, count uint64) error {
	for {
		w.mu.Lock()
		ram, disk, err := w.ram, w.disk, w.err
		w.mu.Unlock()
		if err != nil {
			return err
		}
		if ram >= 1000+count-1 && disk >= 1000+count-1 {
			return nil
		}
		select {
		case <-w.done:
			return fmt.Errorf("the guest driver stopped after ram=%d disk=%d", ram, disk)
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(time.Millisecond):
		}
	}
}

// wait stops the driver and waits for the store it had in flight, so the values
// it reports are exactly the ones the guest acknowledged before it paused.
func (w *workload) wait() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

func (w *workload) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
}

func (w *workload) result() (ram, disk uint64, err error) {
	w.wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ram, w.disk, w.err
}

// guestCommand is the console exchange the test helpers do, without the
// t.Fatal: it runs on the driver's own goroutine.
func guestCommand(ctx context.Context, p *vmmachine.Process, line, want string) error {
	reader := newConsole(p)
	defer reader.close()
	if err := reader.skipExisting(); err != nil {
		return err
	}
	if err := p.WriteConsole(ctx, []byte(line)); err != nil {
		return err
	}
	_, err := reader.wait(ctx, want)
	return err
}

func peerBacking(t *testing.T, c *migrationCluster, handoff vmmigrate.Handoff, v *volume.Volume) *vmmigrate.PeerBacking {
	t.Helper()
	// The page this memory region's numbers are in is its volume's own, which both
	// hosts read out of the same durable geometry. The handoff's page is the
	// source's budget unit and says nothing about one volume, which is exactly
	// what a mixed VM shows: its RAM is 4 KiB and its root 2 MiB.
	backing, err := vmmigrate.NewPeerBacking(vmmigrate.PeerConfig{Volume: v, Peer: handoff.Source,
		VM: handoff.VMID, Unpublished: unpublishedOf(t, handoff, v.Name()),
		Dial: func(ctx context.Context, peer platform.Address) (platform.Conn, error) {
			return c.network.Dial(ctx, "destination-host", peer)
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backing.Close() })
	return backing
}

// unpublishedOf reports the pages of one memory region that the handoff says exist
// nowhere but the source's pages. A destination that does not carry them treats
// its own checkpoint's bytes as current, which is the whole hazard a post-copy
// has.
func unpublishedOf(t *testing.T, handoff vmmigrate.Handoff, name string) []vmmigrate.PageRun {
	t.Helper()
	for _, memoryRegion := range handoff.MemoryRegions {
		if memoryRegion.Name == name {
			return memoryRegion.Unpublished
		}
	}
	t.Fatalf("the handoff names no memory region %q", name)
	return nil
}

// unheldUnpublished selects a page the handoff named as the source's own that
// this destination has not taken yet, which is what a guest's first touch of
// one of those pages reaches: a store into it has to fetch it from the source,
// exactly as a read would.
func unheldUnpublished(t *testing.T, runs []vmmigrate.PageRun, memoryRegion *vmmemory.MemoryRegion) uint64 {
	t.Helper()
	held := make(map[uint64]bool)
	resident, err := memoryRegion.Resident()
	if err != nil {
		t.Fatalf("listing what the memory region holds: %v", err)
	}
	for _, page := range resident {
		held[page] = true
	}
	for _, run := range runs {
		for page := run.First; page < run.First+uint64(run.Count); page++ {
			if !held[page] {
				return page
			}
		}
	}
	t.Fatal("the destination already holds every page the handoff named as the source's own")
	return 0
}

// publishedPage is a page of one volume the handoff did not name as the
// source's own. Those are the only pages a source that has stopped serving may
// be answered for from this host's own log: everything the handoff named exists
// nowhere but that source's pages, and the volume holds the bytes from before
// the guest wrote them.
func publishedPage(t *testing.T, handoff vmmigrate.Handoff, v *volume.Volume) uint64 {
	t.Helper()
	unpublished := make(map[uint64]bool)
	for _, run := range unpublishedOf(t, handoff, v.Name()) {
		for page := run.First; page < run.First+uint64(run.Count); page++ {
			unpublished[page] = true
		}
	}
	for page := range v.Size() / v.PageSize() {
		if !unpublished[page] {
			return page
		}
	}
	t.Fatalf("every page of %s is one no checkpoint holds", v.Name())
	return 0
}

// absentPage selects a stored page this host has not faulted in yet, as the
// backing that memory region attaches through reports it. Sparse zeros bypass the
// backing and cannot establish a fault's origin, and a page the source holds
// unpublished is exactly the one that is not a hole there while the
// destination's own log has nothing for it.
func absentPage(t *testing.T, ctx context.Context, runs []vmmigrate.PageRun, memoryRegion *vmmemory.MemoryRegion, backing vmmemory.Backing) uint64 {
	t.Helper()
	held := make(map[uint64]bool)
	resident, err := memoryRegion.Resident()
	if err != nil {
		t.Fatalf("listing what the memory region holds: %v", err)
	}
	for _, page := range resident {
		held[page] = true
	}
	// The page these numbers are in is the memory region's, which for RAM is 4 KiB.
	size := memoryRegion.PageSize()
	for _, run := range runs {
		for page := run.First; page < run.First+uint64(run.Count); page++ {
			if !held[page] {
				extents, err := backing.Locate(ctx, page*size, size)
				if err != nil {
					t.Fatal(err)
				}
				for _, extent := range extents {
					if !extent.Identity.Zero {
						return page
					}
				}
			}
		}
	}
	t.Fatal("the destination already holds every stored candidate page")
	return 0
}

// comparePeerAndVolume fetches a window of the migrated RAM from the source's
// page server. A migration publishes nothing, so the pages the handoff calls
// unpublished are the guest's writes since the source's last checkpoint and no
// log holds them; every other page the source serves must be exactly what the
// destination's own log holds. The destination is still paused, so nothing is
// writing either copy. It reports the source's resident runs, which is what the
// migrated memory regions fault from.
func comparePeerAndVolume(t *testing.T, ctx context.Context, c *migrationCluster,
	handoff vmmigrate.Handoff, destination *volume.VM) (resident []vmmigrate.PageRun, peerPages, volumePages int64) {
	t.Helper()
	v := destination.Volume(vmmachine.RAMVolume)
	backing := peerBacking(t, c, handoff, v)
	runs, err := backing.Resident(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("the stopped source lists no resident page for its destination")
	}
	unpublished := make(map[uint64]bool)
	for _, run := range unpublishedOf(t, handoff, v.Name()) {
		for page := run.First; page < run.First+uint64(run.Count); page++ {
			unpublished[page] = true
		}
	}
	if len(unpublished) == 0 {
		t.Fatal("the source stopped a storing guest with nothing unpublished to carry")
	}
	// A page number of this memory region is a page of its volume, which for RAM is
	// not the page the handoff's budget is stated in.
	size := v.PageSize()
	compared, carried := 0, 0
	for _, run := range runs {
		count := min(uint64(run.Count), 64)
		fromPeer := make([]byte, count*size)
		if err := backing.Load(ctx, run.First*size, fromPeer); err != nil {
			t.Fatal(err)
		}
		fromVolume := make([]byte, len(fromPeer))
		if err := v.Read(ctx, run.First*size, fromVolume); err != nil {
			t.Fatal(err)
		}
		for i := uint64(0); i < count; i++ {
			page := run.First + i
			if unpublished[page] {
				carried++
				continue
			}
			peerPage := fromPeer[i*size : (i+1)*size]
			volumePage := fromVolume[i*size : (i+1)*size]
			if !bytes.Equal(peerPage, volumePage) {
				t.Fatalf("the source served bytes the destination's log does not hold, at published page %d", page)
			}
			compared++
		}
		if compared+carried >= 512 {
			break
		}
	}
	if carried == 0 {
		t.Fatal("the source served nothing the destination's log does not already hold")
	}
	t.Logf("source pages sampled: %d unpublished, %d published and equal to the destination's log", carried, compared)
	stats := backing.Stats()
	if stats.PeerPages == 0 || stats.FellBack {
		t.Fatalf("the source served nothing of a VM it still holds: %+v", stats)
	}
	return runs, stats.PeerPages, stats.VolumePages
}
