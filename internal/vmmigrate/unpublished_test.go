package vmmigrate_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	migratev1 "github.com/semistrict/sproutfs/internal/vmmigrate/internal/gen/sproutfs/migrate/v1"
	"github.com/semistrict/sproutfs/internal/vmmigrate/internal/wire"
	"github.com/semistrict/sproutfs/internal/volume"
	"google.golang.org/protobuf/proto"
)

// A source at its per-peer budget answers BUSY, which a drain makes its normal
// state. The pages no checkpoint holds are still only there: the destination
// has to ask again until it has them, and must never read its own volume for
// them, which holds the checkpoint the guest has already written past.
func TestBusySourceIsRetriedForThePagesNoCheckpointHolds(t *testing.T) {
	m := newMigration(t)
	if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
		t.Fatal(err)
	}
	// These four writes are the only bytes of this guest no checkpoint has.
	for page := range uint64(4) {
		m.machine.write("ram0", page)
	}
	model := m.machine.snapshot()
	handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})
	var refusals atomic.Int64
	destination, received := receiveDialing(t, m, handoff, busyUntil(m.cluster.dialer("dest"), gate, &refusals))
	done := make(chan error, 1)
	go func() { done <- received.Done(t.Context()) }()
	// The source refuses request after request. Done cannot report success while
	// a page no checkpoint holds is still only there.
	for refusals.Load() < 4 {
		select {
		case err := <-done:
			t.Fatalf("Done returned while the source was answering BUSY: %v", err)
		default:
		}
		runtime.Gosched()
		if err := t.Context().Err(); err != nil {
			t.Fatalf("the destination stopped asking a busy source: %v", err)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("Done returned while the source was answering BUSY: %v", err)
	default:
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stats := received.Stats()
	if stats.Unpublished != 4 || stats.Fetched != 4 {
		t.Fatalf("Done returned having fetched %d of %d unpublished pages, want 4 of 4",
			stats.Fetched, stats.Unpublished)
	}
	// The bytes the destination holds are the source's, not the checkpoint's.
	if err := destination.verify(t.Context(), model); err != nil {
		t.Fatalf("a busy source left the destination with stale bytes: %v", err)
	}
}

// A source this host cannot dial has not stopped holding the pages no
// checkpoint has. They exist nowhere else, so the volume cannot answer for them
// and no number of failed dials says they are gone: the destination keeps
// asking, and Done waits rather than letting the source release the only copy.
// Ending it is the orchestrator's, which discards the received VM — and only
// then is the guest's own fault on one of those pages a failure, loudly, rather
// than a read of the checkpoint it has already written past.
func TestUnreachableSourceLeavesThePagesNoCheckpointHoldsOutstanding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(4) {
			m.machine.write("ram0", page)
		}
		handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		destination, received := receiveDialing(t, m, handoff,
			func(context.Context, platform.Address) (platform.Conn, error) {
				return nil, errors.New("no route to the source")
			})
		done := make(chan error, 1)
		go func() { done <- received.Done(t.Context()) }()
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Done gave up on a source this host could not reach: %v", err)
		default:
		}
		if stats := received.Stats(); stats.Unpublished != 4 || stats.Fetched != 0 {
			t.Fatalf("an unreachable source was credited with %d of %d unpublished pages, want 0 of 4",
				stats.Fetched, stats.Unpublished)
		}
		// The orchestrator ends the migration, which is what discarding the
		// received VM comes to here.
		received.Close()
		err = <-done
		if !errors.Is(err, vmmigrate.ErrClosed) {
			t.Fatalf("Done after the received VM was discarded = %v, want ErrClosed", err)
		}
		if _, err := destination.read(t.Context(), "ram0", 0); !errors.Is(err, vmmigrate.ErrUnpublishedLost) {
			t.Fatalf("a page no checkpoint holds read as %v, want ErrUnpublishedLost", err)
		}
	})
}

// Done is about the pages that exist nowhere else. The rest of the source's
// resident set is an optimization no source waits on, so Done returns while
// that stream is still running.
func TestDoneReturnsBeforeTheBulkStreamCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(2) {
			m.machine.write("ram0", page)
		}
		handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		_, received := receiveDialing(t, m, handoff, holdResidentReplies(m.cluster.dialer("dest"), gate))
		if err := received.Done(t.Context()); err != nil {
			t.Fatal(err)
		}
		stats := received.Stats()
		if stats.Unpublished != 2 || stats.Fetched != 2 {
			t.Fatalf("Done returned having fetched %d of %d unpublished pages, want 2 of 2",
				stats.Fetched, stats.Unpublished)
		}
		if stats.Complete {
			t.Fatal("Done waited for the source's whole resident set rather than the pages no checkpoint holds")
		}
		close(gate)
		received.Close()
	})
}

// The destination's guest runs from the moment its machine starts, which is
// before the stream behind it has fetched a page, and its first touch of a page
// only the source holds can be a store. The source serves that store exactly as
// it serves the stream — those are its own pages either way — so the page is
// here and the source no longer holds the only copy of it. A destination that
// counts only what its stream fetched declares the guest's memory part missing
// and throws away a VM every page of which is present.
func TestAGuestStoreCountsThePageTheSourceServedItForIt(t *testing.T) {
	m := newMigration(t)
	if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
		t.Fatal(err)
	}
	// These four writes are the only bytes of this guest no checkpoint has.
	for page := range uint64(4) {
		m.machine.write("ram0", page)
	}
	model := m.machine.snapshot()
	handoff, err := vmmigrate.Migrate(t.Context(), m.vm, m.machine, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}

	var destination *machine
	received, err := vmmigrate.Receive(t.Context(), m.destination, handoff, m.cluster.dialer("dest"),
		func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (vmmigrate.Runtime, error) {
			built, err := newMachine(t, m.destPager, vm, backings, state)
			if err != nil {
				return nil, err
			}
			built.adopt(model)
			destination = built
			if err := built.Resume(ctx); err != nil {
				return nil, err
			}
			// The guest's own store, made while the stream behind it has
			// fetched nothing: Receive starts that stream once this returns.
			built.write("ram0", 0)
			return built, nil
		}, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(received.Close)
	if destination == nil {
		t.Fatal("the destination never started")
	}
	if err := received.Done(t.Context()); err != nil {
		t.Fatalf("Done after the guest stored into a page the source served it: %v", err)
	}
	stats := received.Stats()
	if stats.Unpublished != 4 || stats.Fetched != 4 {
		t.Fatalf("Done returned having fetched %d of %d unpublished pages, want 4 of 4",
			stats.Fetched, stats.Unpublished)
	}
	// The source is the one thing that knows which pages it answered for, and
	// it answered for this one: it may stop serving.
	if err := m.pages.Release(m.vm.ID()); err != nil {
		t.Fatalf("the source refused to release a VM it had served every page of: %v", err)
	}
	// Every other page the source held is here as the source's bytes, and the
	// page the guest stored into holds the guest's own store on top of them.
	if err := destination.verify(t.Context(), destination.snapshot()); err != nil {
		t.Fatalf("the destination's memory is not what its guest wrote: %v", err)
	}
	if bytes.Equal(destination.snapshot()["ram0"][:pageSize], model["ram0"][:pageSize]) {
		t.Fatal("the destination's guest never stored into the page the source served it")
	}
}

// receiveDialing is migration.receive over a dialer of the test's own, which is
// how a test decides what the source answers.
func receiveDialing(t *testing.T, m *migration, handoff vmmigrate.Handoff, dial vmmigrate.Dialer) (*machine, *vmmigrate.Received) {
	t.Helper()
	var destination *machine
	received, err := vmmigrate.Receive(t.Context(), m.destination, handoff, dial,
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
	if destination == nil {
		t.Fatal("the destination never started")
	}
	return destination, received
}

// busyUntil answers every page reply with the refusal a source at its per-peer
// byte budget sends, until gate is closed. Nothing is wrong with either host:
// the source is simply at its budget for this peer.
func busyUntil(dial vmmigrate.Dialer, gate <-chan struct{}, refusals *atomic.Int64) vmmigrate.Dialer {
	return func(ctx context.Context, address platform.Address) (platform.Conn, error) {
		conn, err := dial(ctx, address)
		if err != nil {
			return nil, err
		}
		return &busyConn{Conn: conn, gate: gate, refusals: refusals}, nil
	}
}

type busyConn struct {
	platform.Conn
	gate     <-chan struct{}
	refusals *atomic.Int64
}

func (c *busyConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	frame, err := c.Conn.Receive(ctx)
	if err != nil {
		return frame, err
	}
	select {
	case <-c.gate:
		return frame, nil
	default:
	}
	incoming, err := wire.Decode(frame)
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	if !incoming.Message.MessageIs(new(migratev1.PageResponse)) {
		return platform.ReceivedFrame{Header: frame.Header, Payload: incoming.Payload,
			PayloadSize: frame.PayloadSize}, nil
	}
	_, err = io.Copy(io.Discard, incoming.Payload)
	closeErr := incoming.Payload.Close()
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	if closeErr != nil {
		return platform.ReceivedFrame{}, closeErr
	}
	c.refusals.Add(1)
	status := migratev1.Status_STATUS_BUSY
	busy, err := wire.Encode(wire.Outgoing{RequestID: incoming.InReplyTo, InReplyTo: incoming.InReplyTo,
		Message: migratev1.PageResponse_builder{Status: &status, PageSize: proto.Uint32(pageSize)}.Build()})
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	return platform.ReceivedFrame{Header: busy.Header, Payload: io.NopCloser(bytes.NewReader(nil))}, nil
}

// holdResidentReplies stalls the listing the bulk pass walks until gate is
// closed, which is a source whose resident set is still on its way after the
// pages no checkpoint holds have arrived.
func holdResidentReplies(dial vmmigrate.Dialer, gate chan struct{}) vmmigrate.Dialer {
	return func(ctx context.Context, address platform.Address) (platform.Conn, error) {
		conn, err := dial(ctx, address)
		if err != nil {
			return nil, err
		}
		return &heldListing{Conn: conn, gate: gate}, nil
	}
}

type heldListing struct {
	platform.Conn
	gate chan struct{}
}

func (c *heldListing) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	frame, err := c.Conn.Receive(ctx)
	if err != nil {
		return frame, err
	}
	incoming, err := wire.Decode(frame)
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	passed := platform.ReceivedFrame{Header: frame.Header, Payload: incoming.Payload, PayloadSize: frame.PayloadSize}
	if !incoming.Message.MessageIs(new(migratev1.ResidentResponse)) {
		return passed, nil
	}
	select {
	case <-c.gate:
		return passed, nil
	case <-ctx.Done():
		_ = incoming.Payload.Close()
		return platform.ReceivedFrame{}, context.Cause(ctx)
	}
}
