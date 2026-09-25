package vmmigrate_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// The source host may exit only when every page it holds is either on the
// destination or in object storage. That is what Done reports: the destination
// fetches every page no checkpoint has, its own next checkpoint publishes them,
// and the source's page server can then be taken away entirely.
func TestDoneMeansTheSourceMayStopServing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		// Publish once, so some pages are the checkpoint's and the rest are this
		// host's alone: only the second kind may hold Done up.
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(4) {
			m.machine.write("ram0", page)
			m.machine.write("disk", page)
		}
		model := m.machine.snapshot()

		handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		unpublished := 0
		for _, memoryRegion := range handoff.MemoryRegions {
			for _, run := range memoryRegion.Unpublished {
				unpublished += run.Count
			}
		}
		if unpublished != 8 {
			t.Fatalf("the handoff named %d unpublished pages, want the 8 the guest wrote since the checkpoint", unpublished)
		}

		received, destination := m.receive(t, handoff)
		if err := received.Done(t.Context()); err != nil {
			t.Fatal(err)
		}
		stats := received.Stats()
		if stats.Unpublished != 8 || stats.Fetched != 8 {
			t.Fatalf("Done returned having fetched %d of %d unpublished pages, want 8 of 8", stats.Fetched, stats.Unpublished)
		}
		// Every fetched page is dirty here and reaches this host's next checkpoint.
		if err := destination.checkpoint(t.Context(), received.VM()); err != nil {
			t.Fatal(err)
		}

		// The source host exits: its page server is gone and its pages with it.
		if err := m.pages.Release(handoff.VMID); err != nil {
			t.Fatalf("releasing a VM the destination reported done: %v", err)
		}
		if err := m.pages.Close(); err != nil {
			t.Fatal(err)
		}
		m.machine.close()

		// The destination serves the whole guest from what it now owns: the
		// pages it published and everything the checkpoint already had.
		destination.close()
		restarted, err := newMachine(t, m.destPager, received.VM(), nil, handoff.State)
		if err != nil {
			t.Fatal(err)
		}
		if err := restarted.verify(t.Context(), model); err != nil {
			t.Fatalf("the destination lost the migrated guest once the source was gone: %v", err)
		}
	})
}

// Done must not report completion while a page no checkpoint has is still only
// on the source: releasing the source then would lose the guest's writes.
func TestDoneWaitsForEveryUnpublishedPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(8) {
			m.machine.write("ram0", page)
		}
		handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		// Hold the source's replies. Done cannot return while an unpublished
		// page is still only there.
		gate := make(chan struct{})
		var destination *machine
		received, err := vmmigrate.Receive(t.Context(), m.destination, handoff, holdPageReplies(m.cluster.dialer("dest"), gate),
			func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (vmmigrate.Runtime, error) {
				built, err := newMachine(t, m.destPager, vm, backings, state)
				if err != nil {
					return nil, err
				}
				destination = built
				return built, built.Resume(ctx)
			}, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- received.Done(t.Context()) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Done returned with unpublished pages still on the source: %v", err)
		default:
		}
		if stats := received.Stats(); stats.Fetched == stats.Unpublished && stats.Unpublished != 0 {
			t.Fatalf("every unpublished page was reported fetched while the source was held: %+v", stats)
		}
		close(gate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if stats := received.Stats(); stats.Unpublished == 0 || stats.Fetched != stats.Unpublished {
			t.Fatalf("Done returned having fetched %d of %d unpublished pages", stats.Fetched, stats.Unpublished)
		}
		if destination == nil {
			t.Fatal("the destination never started")
		}
		// Done left the rest of the source's resident set still arriving.
		if err := received.Streamed(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// holdPageReplies stalls every reply that carries pages until gate is closed,
// which is a source that has not answered yet rather than one that is gone.
func holdPageReplies(dial vmmigrate.Dialer, gate chan struct{}) vmmigrate.Dialer {
	return func(ctx context.Context, address platform.Address) (platform.Conn, error) {
		conn, err := dial(ctx, address)
		if err != nil {
			return nil, err
		}
		return &heldReply{Conn: conn, gate: gate}, nil
	}
}

type heldReply struct {
	platform.Conn
	gate chan struct{}
}

func (c *heldReply) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	frame, err := c.Conn.Receive(ctx)
	if err != nil || frame.PayloadSize == 0 {
		return frame, err
	}
	select {
	case <-c.gate:
		return frame, nil
	case <-ctx.Done():
		_ = frame.Payload.Close()
		return platform.ReceivedFrame{}, context.Cause(ctx)
	}
}
