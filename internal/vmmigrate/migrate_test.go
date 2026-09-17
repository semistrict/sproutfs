package vmmigrate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// sourceAddress is where every test's source host serves pages.
const sourceAddress platform.Address = "source-pages"

// migration is one prepared migration: a cluster, both hosts' managers and
// pagers, a VM running on the source, and the page server the destination
// fetches from.
type migration struct {
	cluster     *cluster
	source      *volume.Manager
	destination *volume.Manager
	sourcePager *pager
	destPager   *pager
	vm          *volume.VM
	machine     *machine
	pages       *vmmigrate.PageSource
}

// newMigration creates a VM, writes and publishes a checkpoint so some of its
// bytes live in object storage rather than in the log, and starts the guest.
func newMigration(t *testing.T) *migration {
	t.Helper()
	c := newCluster(t)
	m := &migration{cluster: c, source: c.manager(t, "source"), destination: c.manager(t, "dest"),
		sourcePager: newPager(t, c, "source"), destPager: newPager(t, c, "dest")}
	vm, err := m.source.Create(t.Context(), "vm-1", vmSpec)
	if err != nil {
		t.Fatal(err)
	}
	m.vm = vm
	m.machine, err = newMachine(t, m.sourcePager, vm, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Pages written before the checkpoint live in a page object, so a
	// destination that could not reach the peer would have to read one.
	for page := range uint64(8) {
		m.machine.write("ram0", page)
		if page < 4 {
			m.machine.write("disk", page)
		}
	}
	if err := vm.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Eight pages per request, so a test can watch one request cover a run.
	m.pages, err = vmmigrate.NewPageSource(t.Context(), vmmigrate.SourceConfig{PageSize: pageSize,
		MaxPagesPerRequest: 8, MaxBytesInFlightPerPeer: 32 << 20, Network: c.runtime.Network(), Address: sourceAddress})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pages.Close() })
	return m
}

// receive opens the VM on the destination and starts a machine over it, which is
// what a destination daemon's start function does.
func (m *migration) receive(t *testing.T, handoff vmmigrate.Handoff) (*vmmigrate.Received, *machine) {
	t.Helper()
	var destination *machine
	received, err := vmmigrate.Receive(t.Context(), m.destination, handoff, m.cluster.dialer("dest"),
		func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (vmmigrate.Runtime, error) {
			built, err := newMachine(t, m.destPager, vm, backings, state)
			if err != nil {
				return nil, err
			}
			destination = built
			// A restored machine comes back running, exactly as the destination's
			// supervisor resumes the VMM it started from the captured state.
			return built, built.Resume(ctx)
		}, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return received, destination
}

// TestMigrationMovesARunningGuestWithoutObjectStorage is the whole contract: a
// guest that never stops storing is stopped, handed off and resumed on another
// host with its bytes intact, and the pause between the two uploads nothing at
// all.
func TestMigrationMovesARunningGuestWithoutObjectStorage(t *testing.T) {
	m := newMigration(t)
	m.machine.start(4)

	handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if handoff.VMID != "vm-1" || handoff.Source != sourceAddress || len(handoff.State) != stateBytes {
		t.Fatalf("handoff: %+v", handoff)
	}
	if len(handoff.Regions) != 2 || handoff.Regions[0].Name != "disk" || handoff.Regions[1].Name != "ram0" ||
		handoff.Regions[0].Size != 4*pageSize || handoff.Regions[1].Size != 8*pageSize {
		t.Fatalf("handoff regions: %+v", handoff.Regions)
	}
	if handoff.PageSize != pageSize {
		t.Fatalf("handoff page size = %d", handoff.PageSize)
	}
	if status := m.vm.Status(); !status.HandedOff {
		t.Fatalf("source status %+v does not report the handoff", status)
	}
	model := m.machine.snapshot()
	resident := m.machine.residentPages()

	received, destination := m.receive(t, handoff)
	if err := received.Done(t.Context()); err != nil {
		t.Fatal(err)
	}
	stats := received.Stats()

	// The pause is from the source's final checkpoint to the destination's resume.
	// The only objects it reads are the VM's own control record and the one
	// checkpoint index that record selects. Not one page is read: a page of this
	// guest coming from object storage here is exactly the round trip this design
	// exists to avoid.
	keys := m.cluster.objects.between(handoff.PausedAt, stats.ResumedAt)
	control, indexes, pages := classify(keys)
	if pages != 0 || indexes != 1 || control == 0 {
		t.Fatalf("the pause read %d control records, %d indexes and %d pages: %v", control, indexes, pages, keys)
	}
	// The pause uploads nothing and reads nothing of the volumes: the frames the
	// source keeps are the whole of what moves.
	loads, verifies := m.machine.volumeWork()
	if loads != m.machine.stopLoads || verifies != m.machine.stopVerifies {
		t.Fatalf("the pause read the volumes %d times and checked authority %d times, want none of either",
			loads-m.machine.stopLoads, verifies-m.machine.stopVerifies)
	}

	// Everything the source still held was served by the source, exactly once,
	// and every page of it that no checkpoint has arrived before Done returned:
	// only then may the source stop serving.
	if stats.PeerPages != int64(resident) {
		t.Fatalf("post-copy read %d pages from the peer, want the %d the source held", stats.PeerPages, resident)
	}
	if stats.Unpublished == 0 || stats.Fetched != stats.Unpublished {
		t.Fatalf("post-copy fetched %d of the source's %d unpublished pages", stats.Fetched, stats.Unpublished)
	}
	if err := destination.verify(t.Context(), model); err != nil {
		t.Fatal(err)
	}
	if again := received.Stats(); again.PeerPages != int64(resident) {
		t.Fatalf("reading the whole guest asked the source for %d pages, want the %d it held",
			again.PeerPages, resident)
	}
	// The captured state reached the destination, so its guest continues counting
	// where the source stopped.
	if destination.stored() != m.machine.stored() {
		t.Fatalf("destination restored %d stores, source made %d", destination.stored(), m.machine.stored())
	}
	destination.adopt(model)
	destination.start(16)
	destination.storedMore(t, m.machine.stored())
	destination.pause()
	if err := destination.verify(t.Context(), destination.snapshot()); err != nil {
		t.Fatal(err)
	}
}

// TestReleasedSourceSendsTheDestinationToItsVolume covers the end of a
// migration: the destination fetches every page no checkpoint has, publishes
// them in its own next checkpoint, and from then on reads everything from its
// own volume and never asks the peer again. That is what lets the source host
// exit.
func TestReleasedSourceSendsTheDestinationToItsVolume(t *testing.T) {
	m := newMigration(t)
	m.machine.start(4)
	handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	model := m.machine.snapshot()
	received, destination := m.receive(t, handoff)
	if err := received.Done(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := received.Stats(); stats.Unpublished == 0 || stats.Fetched != stats.Unpublished {
		t.Fatalf("Done returned with %d of %d unpublished pages fetched", stats.Fetched, stats.Unpublished)
	}
	// The destination's own interval checkpoint is what makes the pages it fetched
	// durable. Until it lands they are dirty here and nowhere else.
	destination.pause()
	if err := destination.checkpoint(t.Context(), received.VM()); err != nil {
		t.Fatal(err)
	}
	if err := m.pages.Release(handoff.VMID); err != nil {
		t.Fatalf("releasing a VM the destination reported done: %v", err)
	}

	// The destination's memory is dropped and attached again, which is every page
	// faulting from scratch with the source no longer serving. Its post-copy is
	// over, so its connections go first: a source at its per-peer connection
	// budget refuses without answering, which is not the answer this is about.
	received.Close()
	destination.close()
	peers := map[string]vmmemory.Backing{}
	backings := map[string]*vmmigrate.PeerBacking{}
	for _, region := range handoff.Regions {
		backing, err := vmmigrate.NewPeerBacking(vmmigrate.PeerConfig{
			Volume: received.VM().Volume(region.Name), Peer: handoff.Source, VM: handoff.VMID,
			PageSize: pageSize, Dial: m.cluster.dialer("dest")})
		if err != nil {
			t.Fatal(err)
		}
		peers[region.Name], backings[region.Name] = backing, backing
	}
	restarted, err := newMachine(t, m.destPager, received.VM(), peers, handoff.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.verify(t.Context(), model); err != nil {
		t.Fatal(err)
	}
	for name, backing := range backings {
		stats := backing.Stats()
		if stats.PeerPages != 0 || stats.VolumePages == 0 || !stats.FellBack {
			t.Fatalf("%s read %d peer and %d volume pages, fell back %v", name, stats.PeerPages, stats.VolumePages, stats.FellBack)
		}
		// One refusal is enough: nothing retries a source that said it no longer
		// serves the VM.
		if stats.Requests != 1 {
			t.Fatalf("%s asked the released source %d times", name, stats.Requests)
		}
	}
}

// TestFailedStopResumesTheGuest requires that a migration abandoned during the
// pause put the guest back exactly where it was, with its memory unsealed and
// its log still its own.
func TestFailedStopResumesTheGuest(t *testing.T) {
	m := newMigration(t)
	m.machine.start(4)
	m.machine.failStop = errInjected
	if _, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{}); !errors.Is(err, errInjected) {
		t.Fatalf("failed stop reported %v", err)
	}
	if status := m.vm.Status(); status.HandedOff || status.Err != nil {
		t.Fatalf("failed stop gave the log up: %+v", status)
	}
	m.machine.storedMore(t, m.machine.stored())
	m.machine.failStop = nil
	handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	model := m.machine.snapshot()
	received, destination := m.receive(t, handoff)
	if err := received.Done(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := destination.verify(t.Context(), model); err != nil {
		t.Fatal(err)
	}
}

// TestAbandonedMigrationStillReopens covers the other side of the handoff: a
// destination that never arrives leaves a VM no host runs, which any host opens
// at the checkpoint its control record selects. A handoff publishes nothing, so
// that is the source's last interval checkpoint.
func TestAbandonedMigrationStillReopens(t *testing.T) {
	m := newMigration(t)
	// The guest is quiet, so the checkpoint is the whole of what the reopen can
	// find: nothing is written after it.
	if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
		t.Fatal(err)
	}
	model := m.machine.snapshot()
	handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The source gives its frames up as well, which is the abandonment.
	if err := m.pages.Release(handoff.VMID); err != nil {
		t.Fatalf("releasing a VM the destination reported done: %v", err)
	}
	m.machine.close()

	reopened, err := m.destination.Open(t.Context(), handoff.VMID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newMachine(t, m.destPager, reopened, nil, handoff.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.verify(t.Context(), model); err != nil {
		t.Fatal(err)
	}
}

// TestReceiveRefusesAMachineMissingARegion requires the destination to check
// that its supervisor started the VM it received: a machine that maps fewer
// regions than the source had would leave one faulting from nowhere, so it is
// closed rather than run, and the VM goes back to whoever opens it next.
func TestReceiveRefusesAMachineMissingARegion(t *testing.T) {
	m := newMigration(t)
	// The in-tree bug guard on this check reads the runtime out of the
	// context, so this test is also what kills migration-accept-missing-region.
	ctx := sim.WithRuntime(t.Context(), m.cluster.runtime)
	m.machine.start(4)
	handoff, err := vmmigrate.Migrate(ctx, m.vm, m.machine, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	model := m.machine.snapshot()
	var started *partialMachine
	_, err = vmmigrate.Receive(ctx, m.destination, handoff, m.cluster.dialer("dest"),
		func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (vmmigrate.Runtime, error) {
			built, err := newMachine(t, m.destPager, vm, backings, state)
			if err != nil {
				return nil, err
			}
			started = &partialMachine{machine: built, missing: "ram0"}
			return started, nil
		}, vmmigrate.Options{})
	if !errors.Is(err, vmmigrate.ErrInvalid) {
		t.Fatalf("a destination without a region for ram0 reported %v", err)
	}
	if started == nil || !started.closed.Load() {
		t.Fatalf("the refused machine was left running: %+v", started)
	}
	// Nothing was published and the log was released, so the VM is received in
	// full by the next host that starts it properly.
	_, destination := m.receive(t, handoff)
	if err := destination.verify(ctx, model); err != nil {
		t.Fatal(err)
	}
}
