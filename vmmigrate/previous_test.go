package vmmigrate_test

import (
	"context"
	"testing"

	"github.com/semistrict/sproutfs/peer/peertest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// previousAddress is where a source of the release before this one serves.
const previousAddress platform.Address = "previous-pages"

// TestAVMHandedOverByThePreviousReleaseArrivesWhole is a rolling upgrade's
// drain: a host of the release before this one hands a running guest to a host
// of this one. The source speaks protocol 1 — no hello, one request at a time —
// and the destination fetches every page only the source holds from it, guest
// faults and the stream alike, and ends with the source's bytes.
func TestAVMHandedOverByThePreviousReleaseArrivesWhole(t *testing.T) {
	m := newMigration(t)
	m.machine.start(4)
	handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The same memory regions, served the way the previous release served them.
	listener, err := m.cluster.runtime.Network().Listen(previousAddress)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	served := make(map[string]peertest.PreviousPages)
	for name, region := range m.machine.MemoryRegions() {
		served[name] = region
	}
	_, stopped := peertest.ServePrevious(ctx, listener, map[string]map[string]peertest.PreviousPages{"vm-1": served})
	t.Cleanup(func() {
		stop()
		_ = listener.Close()
		<-stopped
	})
	handoff.Source = previousAddress
	model := m.machine.snapshot()
	resident := m.machine.residentPages()

	received, destination := m.receive(t, handoff)
	if err := received.Done(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := received.Streamed(t.Context()); err != nil {
		t.Fatal(err)
	}
	stats := received.Stats()
	if stats.PeerPages != int64(resident) || stats.Unpublished == 0 || stats.Fetched != stats.Unpublished {
		t.Fatalf("from the previous release: %d peer pages of the %d it held, %d of %d unpublished fetched",
			stats.PeerPages, resident, stats.Fetched, stats.Unpublished)
	}
	if err := destination.verify(t.Context(), model); err != nil {
		t.Fatal(err)
	}
	if served := m.pages.Stats(); served.Requests != 0 {
		t.Fatalf("this release's own server answered %d page requests, want none", served.Requests)
	}
}
