package peer_test

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/internal/platform"
	migratev1 "github.com/semistrict/sproutfs/internal/vmmigrate/internal/gen/sproutfs/migrate/v1"
	"github.com/semistrict/sproutfs/internal/vmmigrate/internal/peer"
	"github.com/semistrict/sproutfs/internal/vmmigrate/internal/wire"
	"google.golang.org/protobuf/proto"
)

const pageSize = 4096

// script is a page source that answers every request with what one test wrote
// for it, and counts the connections it was asked for and lost.
type script struct {
	answer func(incoming wire.Incoming) (proto.Message, []byte, error)
	dials  atomic.Int64
	closes atomic.Int64
	fail   atomic.Bool
}

func (s *script) dial(context.Context, platform.Address) (platform.Conn, error) {
	s.dials.Add(1)
	return &scriptedConn{script: s, pending: make(chan platform.ReceivedFrame, 1)}, nil
}

// source is a Source over this script, with the budgets a region takes by
// default.
func (s *script) source(connections int) *peer.Source {
	return peer.New(peer.Config{Peer: "source", VM: "vm", Volume: "ram0", PageSize: pageSize,
		MaxConnections: connections, MaxRuns: 2, Dial: s.dial})
}

type scriptedConn struct {
	script  *script
	pending chan platform.ReceivedFrame
}

func (c *scriptedConn) Send(_ context.Context, frame platform.Frame) error {
	incoming, err := wire.Decode(platform.ReceivedFrame{Header: frame.Header,
		Payload: io.NopCloser(bytes.NewReader(nil))})
	if err != nil {
		return err
	}
	if err := incoming.Payload.Close(); err != nil {
		return err
	}
	if c.script.fail.Load() {
		return errors.New("the connection broke")
	}
	message, payload, err := c.script.answer(incoming)
	if err != nil {
		return err
	}
	outgoing := wire.Outgoing{RequestID: incoming.RequestID, InReplyTo: incoming.RequestID, Message: message}
	if len(payload) > 0 {
		outgoing.Payload = wire.Payload{Body: bytes.NewReader(payload), Size: int64(len(payload)),
			Algorithm: wire.ChecksumCRC32C,
			Checksum:  wire.EncodeCRC32C(crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))}
	}
	reply, err := wire.Encode(outgoing)
	if err != nil {
		return err
	}
	c.pending <- platform.ReceivedFrame{Header: reply.Header,
		Payload: io.NopCloser(bytes.NewReader(payload)), PayloadSize: reply.PayloadSize}
	return nil
}

func (c *scriptedConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	select {
	case frame := <-c.pending:
		return frame, nil
	case <-ctx.Done():
		return platform.ReceivedFrame{}, context.Cause(ctx)
	}
}

func (c *scriptedConn) LocalAddress() platform.Address  { return "destination" }
func (c *scriptedConn) RemoteAddress() platform.Address { return "source" }
func (c *scriptedConn) Close() error                    { c.script.closes.Add(1); return nil }

func pageReply(t *testing.T, count int, present, dirty []byte, pages []byte) (proto.Message, []byte) {
	t.Helper()
	encoded, err := blob.Encode(t.Context(), pages)
	if err != nil {
		t.Fatal(err)
	}
	status := migratev1.Status_STATUS_OK
	return migratev1.PageResponse_builder{Status: &status, PageSize: proto.Uint32(pageSize),
		Count: proto.Uint32(uint32(count)), PayloadFormat: proto.Uint32(1),
		Present: present, Dirty: dirty}.Build(), encoded
}

// A served page comes back with its bytes, the bit that says the source served
// it, and the bit that says no checkpoint holds it.
func TestPagesReportWhatTheSourceServed(t *testing.T) {
	first := bytes.Repeat([]byte{0xa5}, pageSize)
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		message, payload := pageReply(t, 2, []byte{0b01}, []byte{0b01}, first)
		return message, payload, nil
	}}
	answer, err := s.source(1).Pages(t.Context(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Busy {
		t.Fatal("a source that served a page reported itself busy")
	}
	if len(answer.Present) != 1 || answer.Present[0] != 0b01 || answer.Dirty[0] != 0b01 {
		t.Fatalf("bitmaps present=%v dirty=%v", answer.Present, answer.Dirty)
	}
	if !bytes.Equal(answer.Payload, first) {
		t.Fatalf("the payload is %d bytes, want the page's %d", len(answer.Payload), len(first))
	}
}

// A source at its budget for this peer served nothing and said so, which is an
// answer rather than an error: the caller decides whether to wait.
func TestPagesReportABusySourceAsAnAnswer(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		status := migratev1.Status_STATUS_BUSY
		return migratev1.PageResponse_builder{Status: &status, PageSize: proto.Uint32(pageSize)}.Build(), nil, nil
	}}
	answer, err := s.source(1).Pages(t.Context(), 0, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !answer.Busy || len(answer.Present) != 2 || len(answer.Dirty) != 2 {
		t.Fatalf("a busy reply came back as %+v", answer)
	}
	if answer.Present[0]|answer.Present[1]|answer.Dirty[0]|answer.Dirty[1] != 0 {
		t.Fatal("a busy source was recorded as having served a page")
	}
}

// A source that says it does not serve this VM is not going to serve it again.
func TestAnUnknownVMIsNotServed(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		status := migratev1.Status_STATUS_UNKNOWN_VM
		return migratev1.PageResponse_builder{Status: &status}.Build(), nil, nil
	}}
	if _, err := s.source(1).Pages(t.Context(), 0, 1); !errors.Is(err, peer.ErrNotServed) {
		t.Fatalf("a request for a VM the source dropped: %v", err)
	}
}

// A reply for more pages than were asked for is not this protocol's.
func TestPagesRejectAReplyThatOverrunsTheRequest(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		message, payload := pageReply(t, 4, []byte{0}, []byte{0}, nil)
		return message, payload, nil
	}}
	if _, err := s.source(1).Pages(t.Context(), 0, 2); !errors.Is(err, wire.ErrMalformedFrame) {
		t.Fatalf("a reply naming more pages than were asked for: %v", err)
	}
}

// A listing is walked a bounded number of runs at a time, and every reply's
// runs join the one the caller gets.
func TestResidentWalksTheWholeListing(t *testing.T) {
	var replies atomic.Int64
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		status := migratev1.Status_STATUS_OK
		more := replies.Add(1) == 1
		run := migratev1.PageRun_builder{FirstPage: proto.Uint64(uint64(replies.Load()-1) * 4),
			Count: proto.Uint32(4)}.Build()
		return migratev1.ResidentResponse_builder{Status: &status, PageSize: proto.Uint32(pageSize),
			Runs: []*migratev1.PageRun{run}, More: proto.Bool(more)}.Build(), nil, nil
	}}
	runs, err := s.source(1).Resident(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []peer.Run{{First: 0, Count: 4}, {First: 4, Count: 4}}
	if len(runs) != len(want) || runs[0] != want[0] || runs[1] != want[1] {
		t.Fatalf("the listing is %+v, want %+v", runs, want)
	}
}

// A source whose pages are another size cannot be read at all, however
// well-formed its reply is.
func TestResidentRefusesASourceOfAnotherPageSize(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		status := migratev1.Status_STATUS_OK
		return migratev1.ResidentResponse_builder{Status: &status,
			PageSize: proto.Uint32(pageSize * 2)}.Build(), nil, nil
	}}
	if _, err := s.source(1).Resident(t.Context()); !errors.Is(err, peer.ErrPageSize) {
		t.Fatalf("a source serving pages of another size: %v", err)
	}
}

// A connection that answered is reused, one that failed is dropped, and Close
// drops what is left — so a region's requests cost one socket rather than one
// per page.
func TestConnectionsAreReusedUntilOneFails(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		status := migratev1.Status_STATUS_BUSY
		return migratev1.PageResponse_builder{Status: &status, PageSize: proto.Uint32(pageSize)}.Build(), nil, nil
	}}
	source := s.source(1)
	for range 3 {
		if _, err := source.Pages(t.Context(), 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	if s.dials.Load() != 1 || s.closes.Load() != 0 {
		t.Fatalf("three requests over %d connections, %d of them dropped", s.dials.Load(), s.closes.Load())
	}
	s.fail.Store(true)
	if _, err := source.Pages(t.Context(), 0, 1); err == nil {
		t.Fatal("a broken connection answered")
	}
	if s.dials.Load() != 1 || s.closes.Load() != 1 {
		t.Fatalf("after a failure: %d connections, %d dropped", s.dials.Load(), s.closes.Load())
	}
	// The slot the failed connection held came back, so the next request dials.
	s.fail.Store(false)
	if _, err := source.Pages(t.Context(), 0, 1); err != nil {
		t.Fatal(err)
	}
	if s.dials.Load() != 2 {
		t.Fatalf("a failed connection did not return its slot: %d dials", s.dials.Load())
	}
	source.Close()
	if s.closes.Load() != 2 {
		t.Fatalf("closing the source left %d of its connections open", s.dials.Load()-s.closes.Load())
	}
	if got := source.Requests(); got != 5 {
		t.Fatalf("the source counted %d requests, want 5", got)
	}
}

// Every request is offered to the run's admitter before a connection is taken,
// so a controlled run orders the decision to ask the source against the
// cancellation that would stop it. A pooled connection is taken without
// dialing, so without this there is nothing between the two.
func TestEveryRequestIsAdmittedBeforeItTakesAConnection(t *testing.T) {
	var sends atomic.Int64
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		sends.Add(1)
		status := migratev1.Status_STATUS_BUSY
		return migratev1.PageResponse_builder{Status: &status, PageSize: proto.Uint32(pageSize)}.Build(), nil, nil
	}}
	refused := errors.New("the stream was closed")
	var regions []string
	ctx := peer.WithAdmission(t.Context(), func(_ context.Context, region string) error {
		regions = append(regions, region)
		if len(regions) == 1 {
			return nil
		}
		return refused
	})
	source := s.source(1)
	if _, err := source.Pages(ctx, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Pages(ctx, 0, 1); !errors.Is(err, refused) {
		t.Fatalf("the second request was not refused by the admitter: %v", err)
	}
	if want := []string{"vm/ram0", "vm/ram0"}; !slices.Equal(regions, want) {
		t.Fatalf("the admitter saw %v, want %v", regions, want)
	}
	// The connection the first request left idle is the one a refused request
	// would have reached the wire over.
	if s.dials.Load() != 1 || sends.Load() != 1 {
		t.Fatalf("a refused request reached the wire: %d dials, %d sends", s.dials.Load(), sends.Load())
	}
	if got := source.Requests(); got != 1 {
		t.Fatalf("the source counted %d requests, want 1", got)
	}
}

// An already-canceled caller still reaches the admitter. That is what makes the
// decision the controller's rather than the Go runtime's: the request is
// refused because the run said so at a point it chose, not because a select
// happened to see a canceled context first.
func TestAnAlreadyCanceledRequestIsStillAdmitted(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		t.Error("a canceled request reached the source")
		return nil, nil, errors.New("unreachable")
	}}
	closed := errors.New("the stream was closed")
	var admitted int
	ctx, cancel := context.WithCancelCause(t.Context())
	ctx = peer.WithAdmission(ctx, func(inner context.Context, _ string) error {
		admitted++
		return context.Cause(inner)
	})
	cancel(closed)
	if _, err := s.source(1).Resident(ctx); !errors.Is(err, closed) {
		t.Fatalf("a canceled listing returned %v", err)
	}
	if admitted != 1 {
		t.Fatalf("the admitter saw %d requests, want 1", admitted)
	}
	if s.dials.Load() != 0 {
		t.Fatalf("a canceled listing dialed %d connections", s.dials.Load())
	}
}
