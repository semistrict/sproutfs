package vmmigrate_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// served is one VM whose pages a source host serves without having migrated it,
// which is what isolates the page protocol from the migration that uses it.
type served struct {
	migration *migration
	vm        *volume.VM
	machine   *machine
	written   int
	// source is the page source a test started of its own for this VM, if any.
	source *vmmigrate.PageSource
}

// newServed creates a second VM, stores into the first pages of its RAM without
// ever flushing them, and registers its memory regions. The volume therefore reads as
// zeroes exactly where the pager holds the guest's bytes, so a test can tell
// which of the two answered a load.
func newServed(t *testing.T, source *vmmigrate.PageSource, written int) *served {
	t.Helper()
	m := newMigration(t)
	vm, err := m.source.Create(t.Context(), "vm-2", vmSpec)
	if err != nil {
		t.Fatal(err)
	}
	built, err := newMachine(t, m.sourcePager, vm, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(written) {
		built.write("ram0", page)
	}
	if source == nil {
		source = m.pages
	}
	source.Serve("vm-2", vmmigrate.MemoryRegionPages(built.MemoryRegions()))
	return &served{migration: m, vm: vm, machine: built, written: written}
}

func (s *served) backing(t *testing.T, source *vmmigrate.PageSource, name string) *vmmigrate.PeerBacking {
	t.Helper()
	return s.dialing(t, source, name, s.migration.cluster.dialer("dest"))
}

func (s *served) dialing(t *testing.T, source *vmmigrate.PageSource, name string, dial vmmigrate.Dialer) *vmmigrate.PeerBacking {
	t.Helper()
	address := sourceAddress
	if source != nil {
		address = source.Address()
	}
	backing, err := vmmigrate.NewPeerBacking(vmmigrate.PeerConfig{Volume: s.vm.Volume(name),
		Peer: address, VM: "vm-2", PageSize: pageSize, MaxPagesPerRequest: 8, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backing.Close() })
	return backing
}

// pageSource starts a second page source with budgets of a test's own.
func (m *migration) pageSource(t *testing.T, config vmmigrate.SourceConfig) *vmmigrate.PageSource {
	t.Helper()
	config.PageSize = pageSize
	if config.MaxPagesPerRequest == 0 {
		config.MaxPagesPerRequest = 8
	}
	if config.MaxBytesInFlightPerPeer == 0 {
		config.MaxBytesInFlightPerPeer = 32 << 20
	}
	config.Network = m.cluster.runtime.Network()
	if config.Address == "" {
		config.Address = "source-pages-2"
	}
	source, err := vmmigrate.NewPageSource(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

// TestPageServerAnswersHeldAndAbsentPagesInOneRequest is the protocol itself: one
// request covers a run, the bitmap says which pages came back, and every page the
// source does not hold is read from the destination's own volume instead.
func TestPageServerAnswersHeldAndAbsentPagesInOneRequest(t *testing.T) {
	s := newServed(t, nil, 4)
	backing := s.backing(t, nil, "ram0")
	data := make([]byte, 8*pageSize)
	if err := backing.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	model := s.machine.snapshot()["ram0"]
	for page := range 8 {
		got := data[page*pageSize : (page+1)*pageSize]
		want := make([]byte, pageSize)
		if page < s.written {
			// The source holds these; the volume was never told about them.
			want = model[page*pageSize : (page+1)*pageSize]
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("page %d: got %d..., want %d...", page, got[0], want[0])
		}
	}
	if stats := backing.Stats(); stats.PeerPages != 4 || stats.VolumePages != 4 || stats.Requests != 1 || stats.FellBack {
		t.Fatalf("peer backing: %+v", stats)
	}
	if stats := s.migration.pages.Stats(); stats.Requests != 1 || stats.Served != 4 || stats.Absent != 4 {
		t.Fatalf("page source: %+v", stats)
	}
}

// TestUnknownVolumeFallsBackForGood requires a memory region the source does not serve
// to stop asking after the one answer that says so.
func TestUnknownVolumeFallsBackForGood(t *testing.T) {
	s := newServed(t, nil, 4)
	// Only RAM is registered, so the disk is a volume this source does not serve.
	s.migration.pages.Serve("vm-2", vmmigrate.MemoryRegionPages(map[string]*vmmemory.MemoryRegion{"ram0": s.machine.memoryRegions["ram0"]}))
	backing := s.backing(t, nil, "disk")
	data := make([]byte, 4*pageSize)
	if err := backing.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, make([]byte, len(data))) {
		t.Fatal("an unserved volume did not read as the zeroes it holds")
	}
	if err := backing.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	stats := backing.Stats()
	if !stats.FellBack || stats.Requests != 1 || stats.PeerPages != 0 || stats.VolumePages != 8 {
		t.Fatalf("an unserved volume kept asking: %+v", stats)
	}
}

// TestAnUnreachableSourceStillAnswersForThePagesTheCheckpointHolds requires a
// source that cannot be reached to cost a load the round trip and nothing else.
// Every page of this memory region is in a checkpoint this host can read, so there is
// nothing to wait for: the load reads its volume this time and decides nothing
// for the next one, which asks again, because a host that cannot be dialed now
// is not a host that is gone. Only the source's own answer ends the asking.
func TestAnUnreachableSourceStillAnswersForThePagesTheCheckpointHolds(t *testing.T) {
	s := newServed(t, nil, 4)
	dials := 0
	backing := s.dialing(t, nil, "ram0", func(context.Context, platform.Address) (platform.Conn, error) {
		dials++
		return nil, errors.New("no route to the source")
	})
	data := make([]byte, 4*pageSize)
	for load := range 6 {
		if err := backing.Load(t.Context(), 0, data); err != nil {
			t.Fatal(err)
		}
		if dials != load+1 {
			t.Fatalf("load %d dialed the source %d times in all, want one each", load, dials)
		}
		if stats := backing.Stats(); stats.FellBack {
			t.Fatalf("an unreachable source was taken for a gone one: %+v", stats)
		}
	}
	if !bytes.Equal(data, make([]byte, len(data))) {
		t.Fatal("the volume's own zeroes were not what the load read")
	}
	if stats := backing.Stats(); stats.VolumePages != 24 || stats.PeerPages != 0 {
		t.Fatalf("peer backing: %+v", stats)
	}
}

// TestPageSourceBoundsConnectionsPerPeer requires the source to refuse a peer
// that opens more connections than its budget, and the refused destination to
// carry on from its own volume.
func TestPageSourceBoundsConnectionsPerPeer(t *testing.T) {
	s := newServed(t, nil, 4)
	source := s.migration.pageSource(t, vmmigrate.SourceConfig{MaxConnectionsPerPeer: 1})
	source.Serve("vm-2", vmmigrate.MemoryRegionPages(s.machine.MemoryRegions()))
	first, second := s.backing(t, source, "ram0"), s.backing(t, source, "ram0")
	data := make([]byte, 4*pageSize)
	if err := first.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if stats := first.Stats(); stats.PeerPages != 4 {
		t.Fatalf("the first connection was not served: %+v", stats)
	}
	// The first backing keeps its connection, so the second one's is over the
	// budget and closed before it is served.
	if err := second.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, make([]byte, len(data))) {
		t.Fatal("a refused connection did not read the volume")
	}
	// A connection the source refused is a source at its budget, not a source
	// that is gone: the destination retries it, reads its volume for this load
	// and asks again next time.
	if stats := second.Stats(); stats.FellBack || stats.VolumePages != 4 || stats.PeerPages != 0 {
		t.Fatalf("the refused backing: %+v", stats)
	}
	if stats := source.Stats(); stats.Refused == 0 {
		t.Fatal("the page source refused no connection")
	}
}

func TestPageSourceReusesConnectionBudgetAfterDisconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServed(t, nil, 4)
		source := s.migration.pageSource(t, vmmigrate.SourceConfig{MaxConnectionsPerPeer: 1})
		source.Serve("vm-2", vmmigrate.MemoryRegionPages(s.machine.MemoryRegions()))
		want := s.machine.snapshot()["ram0"][:4*pageSize]
		for attempt := range 4 {
			backing := s.backing(t, source, "ram0")
			data := make([]byte, len(want))
			if err := backing.Load(t.Context(), 0, data); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, want) {
				t.Fatalf("connection %d did not receive the source's private bytes", attempt)
			}
			if stats := backing.Stats(); stats.PeerPages != 4 || stats.VolumePages != 0 || stats.FellBack {
				t.Fatalf("connection %d: %+v", attempt, stats)
			}
			if err := backing.Close(); err != nil {
				t.Fatal(err)
			}
			// Let the server observe disconnection and release its reservation
			// before this same peer opens the next connection.
			synctest.Wait()
		}
		if refused := source.Stats().Refused; refused != 0 {
			t.Fatalf("sequential connections exhausted the budget: %d refusals", refused)
		}
	})
}

// TestBusySourceIsNotAFallback separates the two refusals: a source at its
// per-peer byte budget serves nothing this time, and is asked again next time,
// because being busy is not being gone.
func TestBusySourceIsNotAFallback(t *testing.T) {
	s := newServed(t, nil, 4)
	source := s.migration.pageSource(t, vmmigrate.SourceConfig{MaxBytesInFlightPerPeer: pageSize})
	source.Serve("vm-2", vmmigrate.MemoryRegionPages(s.machine.MemoryRegions()))
	backing := s.backing(t, source, "ram0")
	data := make([]byte, 4*pageSize)
	for range 2 {
		if err := backing.Load(t.Context(), 0, data); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(data, make([]byte, len(data))) {
		t.Fatal("a busy source did not send the destination to its volume")
	}
	stats := backing.Stats()
	if stats.FellBack || stats.Requests != 2 || stats.PeerPages != 0 || stats.VolumePages != 8 {
		t.Fatalf("a busy source was treated as a gone one: %+v", stats)
	}
	if refused := source.Stats().Refused; refused != 2 {
		t.Fatalf("page source refused %d requests", refused)
	}
}

// TestResidentListingNamesWhatTheSourceHolds is what a destination's bulk stream
// asks for before it faults anything in.
func TestResidentListingNamesWhatTheSourceHolds(t *testing.T) {
	s := newServed(t, nil, 4)
	backing := s.backing(t, nil, "ram0")
	runs, err := backing.Resident(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].First != 0 || runs[0].Count != 4 {
		t.Fatalf("resident runs: %+v", runs)
	}
	if stats := s.migration.pages.Stats(); stats.Listings != 1 {
		t.Fatalf("page source answered %d listings", stats.Listings)
	}
}
