// Package peer is the destination's half of a migration's page protocol: the
// pooled connections to the one host that still holds a region's pages, and the
// two requests a destination makes over them.
//
// It holds no policy. Whether to ask a busy source again, whether to stop
// asking it altogether, and which pages only the source has are the receiver's
// to decide; this package sends one request and reports what came back.
package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"sync/atomic"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/internal/latency"
	"github.com/semistrict/sproutfs/internal/platform"
	migratev1 "github.com/semistrict/sproutfs/internal/vmmigrate/internal/gen/sproutfs/migrate/v1"
	"github.com/semistrict/sproutfs/internal/vmmigrate/internal/wire"
	"google.golang.org/protobuf/proto"
)

var (
	// ErrNotServed reports a source that does not serve this VM any more.
	ErrNotServed = errors.New("vmmigrate: the source no longer serves this VM")
	// ErrBusy reports a source at its budget for this peer. Nothing is wrong
	// with it and nothing is wrong here: it served nothing this time. A page
	// request reports it as an answer rather than an error; a listing has
	// nothing to report and so returns this.
	ErrBusy = errors.New("vmmigrate: the source is at its budget for this peer")
	// ErrPageSize reports a source whose pages are not the size this host maps,
	// which no reply of its can be read as.
	ErrPageSize = errors.New("vmmigrate: the source serves pages of another size")
)

// Dialer opens one connection to a peer's page source. A production dialer is
// plain TCP on the deployment's trusted network.
type Dialer func(ctx context.Context, peer platform.Address) (platform.Conn, error)

// Run is a run of consecutive pages the source holds.
type Run struct {
	First uint64
	Count int
}

// Answer is what one page request came back with: which pages the source
// served, which of those are its own state that no checkpoint has, their bytes,
// and whether the source was simply at its budget for this peer.
type Answer struct {
	Present, Dirty, Payload []byte
	Busy                    bool
}

// Config names the host one region asks for its pages, and the budgets every
// request to it obeys. The caller has already checked them.
type Config struct {
	// Peer is the source host's page source, VM the migrated VM's identity and
	// Volume the region's volume. Both requests name all three.
	Peer   platform.Address
	VM     string
	Volume string
	// PageSize must be the source's, which the handoff reports. A source that
	// serves another size is refused rather than misread.
	PageSize int
	// MaxConnections bounds this region's requests in flight. The post-copy
	// stream may use all but one of them, so a guest fault always has one; with
	// a single connection the two share it.
	MaxConnections int
	// MaxRuns bounds one resident listing, so a destination walks a large
	// region's residency rather than asking for all of it in one reply.
	MaxRuns int
	Dial    Dialer
	// Clock times the requests for their latency histograms. Nil is the wall
	// clock.
	Clock platform.Clock
}

// Latency is how long this region's requests took, by kind: a guest fault's,
// and the post-copy stream's. Wait is the part spent waiting for a connection;
// the other histogram is the whole request, wait included.
type Latency struct {
	Fault, FaultWait, Stream, StreamWait latency.Snapshot
}

// Merge adds another region's latencies to these.
func (l Latency) Merge(other Latency) Latency {
	return Latency{Fault: l.Fault.Merge(other.Fault), FaultWait: l.FaultWait.Merge(other.FaultWait),
		Stream: l.Stream.Merge(other.Stream), StreamWait: l.StreamWait.Merge(other.StreamWait)}
}

// Source is one region's handle on the host that still holds its pages: the
// connections to it, and what every request over them names. Its methods are
// safe for concurrent use.
type Source struct {
	config Config
	// idle holds connections to reuse and slots bounds how many exist at once,
	// so a destination's concurrent faults pipeline without opening a socket per
	// page.
	idle  chan platform.Conn
	slots chan struct{}
	// stream bounds the stream's requests in flight to one fewer than the
	// connections, so a guest fault never queues behind the stream.
	stream chan struct{}
	clock  platform.Clock

	fault, faultWait, streamed, streamWait latency.Histogram

	closed atomic.Bool
	// inflight is how many requests this region has between admission and
	// answer. A region with none holds no connection: see call.
	inflight atomic.Int64
	nextID   atomic.Uint64
	requests atomic.Int64
}

// New opens nothing: the first request dials the first connection.
func New(config Config) *Source {
	s := &Source{config: config,
		idle:   make(chan platform.Conn, config.MaxConnections),
		slots:  make(chan struct{}, config.MaxConnections),
		stream: make(chan struct{}, streamConnections(config.MaxConnections)),
		clock:  platform.ClockOr(config.Clock)}
	for range config.MaxConnections {
		s.slots <- struct{}{}
	}
	for range cap(s.stream) {
		s.stream <- struct{}{}
	}
	return s
}

// streamConnections is how many of a region's connections the post-copy stream
// may use: all but the one kept for guest faults, and never none.
func streamConnections(connections int) int { return max(1, connections-1) }

// Concurrency is how many requests the post-copy stream may have in flight at
// once, which is what it should run in parallel.
func (s *Source) Concurrency() int { return cap(s.stream) }

// Latency reports how long this region's requests have taken so far.
func (s *Source) Latency() Latency {
	return Latency{Fault: s.fault.Snapshot(), FaultWait: s.faultWait.Snapshot(),
		Stream: s.streamed.Snapshot(), StreamWait: s.streamWait.Snapshot()}
}

// Requests is every request sent to the source.
func (s *Source) Requests() int64 { return s.requests.Load() }

// Pages asks for one run of pages and reports the bitmaps and bytes the source
// answered with. A source at its budget for this peer served nothing and says
// so, which is an answer rather than a failure.
func (s *Source) Pages(ctx context.Context, first uint64, count int) (Answer, error) {
	response := new(migratev1.PageResponse)
	request := migratev1.PageRequest_builder{Vm: proto.String(s.config.VM),
		Volume: proto.String(s.config.Volume), FirstPage: proto.Uint64(first),
		Count: proto.Uint32(uint32(count)), PayloadFormat: proto.Uint32(1)}.Build()
	payload, err := s.call(ctx, request, response, int64(count)*int64(s.config.PageSize)+blob.HeaderSize)
	if err != nil {
		return Answer{}, err
	}
	switch status := response.GetStatus(); {
	case status == migratev1.Status_STATUS_OK:
	case status == migratev1.Status_STATUS_BUSY:
		// The source is at its budget for this peer. Nothing is wrong with it and
		// nothing is wrong here: it served nothing this time.
		return Answer{Present: make([]byte, (count+7)/8), Dirty: make([]byte, (count+7)/8), Busy: true}, nil
	default:
		return Answer{}, statusError(status)
	}
	answered := (int(response.GetCount()) + 7) / 8
	if response.GetPayloadFormat() != 1 || response.GetPageSize() != uint32(s.config.PageSize) || int(response.GetCount()) > count ||
		len(response.GetPresent()) != answered || len(response.GetDirty()) != answered {
		return Answer{}, fmt.Errorf("%w: malformed page response", wire.ErrMalformedFrame)
	}
	if n := response.GetCount(); n%8 != 0 && response.GetPresent()[n/8]>>uint(n%8) != 0 {
		return Answer{}, fmt.Errorf("%w: page bitmap exceeds response count", wire.ErrMalformedFrame)
	}
	// A source that answered for fewer pages than were asked for leaves the rest
	// to the caller, which a clear bit says.
	bitmap := make([]byte, (count+7)/8)
	copy(bitmap, response.GetPresent()[:answered])
	unpublished := make([]byte, (count+7)/8)
	copy(unpublished, response.GetDirty()[:answered])
	for index := int(response.GetCount()); index < count; index++ {
		bitmap[index/8] &^= 1 << (index % 8)
		unpublished[index/8] &^= 1 << (index % 8)
	}
	served := 0
	for position, bitsInByte := range bitmap {
		// A page the source says is its own state but did not serve has no bytes
		// to be dirty with.
		unpublished[position] &= bitsInByte
		served += bits.OnesCount8(bitsInByte)
	}
	decoded, err := blob.Decode(ctx, payload, served*s.config.PageSize)
	if err != nil || len(decoded) != served*s.config.PageSize {
		return Answer{}, errors.Join(wire.ErrMalformedFrame, err)
	}
	return Answer{Present: bitmap, Dirty: unpublished, Payload: decoded}, nil
}

// Resident asks which pages the source still holds, so the destination can
// stream them in behind its running guest. The listing is walked a bounded
// number of runs at a time.
func (s *Source) Resident(ctx context.Context) ([]Run, error) {
	var runs []Run
	first := uint64(0)
	for {
		response := new(migratev1.ResidentResponse)
		request := migratev1.ResidentRequest_builder{Vm: proto.String(s.config.VM),
			Volume: proto.String(s.config.Volume), FirstPage: proto.Uint64(first),
			MaxRuns: proto.Uint32(uint32(s.config.MaxRuns))}.Build()
		if _, err := s.call(ctx, request, response, 0); err != nil {
			return nil, err
		}
		if status := response.GetStatus(); status != migratev1.Status_STATUS_OK {
			return nil, statusError(status)
		}
		if response.GetPageSize() != uint32(s.config.PageSize) {
			return nil, fmt.Errorf("%w: the source serves %d byte pages, this host maps %d",
				ErrPageSize, response.GetPageSize(), s.config.PageSize)
		}
		for _, run := range response.GetRuns() {
			if run.GetCount() == 0 {
				continue
			}
			runs = append(runs, Run{First: run.GetFirstPage(), Count: int(run.GetCount())})
			first = run.GetFirstPage() + uint64(run.GetCount())
		}
		if !response.GetMore() || len(response.GetRuns()) == 0 {
			return runs, nil
		}
	}
}

// Close drops every connection this region holds. A later request dials again:
// stopping is the caller's decision, not this one's.
func (s *Source) Close() {
	s.closed.Store(true)
	s.drain()
}

// call sends one request over an idle or newly dialed connection and reads its
// reply. A connection that failed is dropped rather than reused.
//
// Making the request is admitted before a connection is taken. A pooled
// connection is taken without dialing and a dial refuses an already-cancelled
// context before it does anything, so this is the only point between a caller's
// cancellation and the wire that a controlled run can order; see Admitter.
//
// A region with no request in flight keeps one connection and gives the rest
// back. The pool is there so that concurrent requests pipeline rather than
// opening a socket per page, and a burst that is over needs one connection
// rather than the several it opened: the source bounds what one peer may hold
// at once, and connections a region is not using take that bound from the
// regions that are. Those regions ask for the pages no checkpoint holds, which
// exist nowhere else, so they ask for ever — and a region whose whole pass is
// over would never have given its connections back. Keeping one and dropping
// the rest is what makes the bound a queue rather than a deadlock, while a
// region asking one page at a time still pays one socket rather than one per
// page.
func (s *Source) call(ctx context.Context, request, response proto.Message, maxPayload int64) ([]byte, error) {
	if err := admit(ctx, s.config.VM+"/"+s.config.Volume); err != nil {
		return nil, err
	}
	began := s.clock.Now()
	total, wait := &s.fault, &s.faultWait
	if streaming(ctx) {
		total, wait = &s.streamed, &s.streamWait
		// The stream takes one of its own slots before a connection, so it
		// never holds the last connection a guest fault needs.
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-s.stream:
		}
		defer func() { s.stream <- struct{}{} }()
	}
	s.requests.Add(1)
	s.inflight.Add(1)
	defer func() {
		if s.inflight.Add(-1) == 0 {
			s.trim()
		}
	}()
	conn, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	wait.Observe(s.clock.Since(began))
	defer func() { total.Observe(s.clock.Since(began)) }()
	id := s.nextID.Add(1)
	payload, err := exchange(ctx, conn, id, request, response, maxPayload)
	if err != nil {
		s.discard(conn)
		return nil, err
	}
	s.recycle(conn)
	return payload, nil
}

// connection takes an idle connection or dials one under this region's bound.
func (s *Source) connection(ctx context.Context) (platform.Conn, error) {
	select {
	case conn := <-s.idle:
		return conn, nil
	default:
	}
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case conn := <-s.idle:
		return conn, nil
	case <-s.slots:
	}
	// A slot can fall free at the same moment the caller gives up, and a select
	// picks either. Dialing a peer for a request nobody is waiting for costs a
	// connection this region's bound then has to take back.
	if err := context.Cause(ctx); err != nil {
		s.slots <- struct{}{}
		return nil, err
	}
	conn, err := s.config.Dial(ctx, s.config.Peer)
	if err != nil {
		s.slots <- struct{}{}
		return nil, err
	}
	return conn, nil
}

// recycle returns a usable connection, closing it when this region has stopped
// asking or already holds its share of idle connections.
func (s *Source) recycle(conn platform.Conn) {
	if s.closed.Load() {
		s.discard(conn)
		return
	}
	select {
	case s.idle <- conn:
	default:
		s.discard(conn)
	}
}

// discard closes one connection and returns the slot it held.
func (s *Source) discard(conn platform.Conn) {
	_ = conn.Close()
	s.slots <- struct{}{}
}

// trim gives back every pooled connection but one, which is what a region with
// nothing in flight keeps. It is not a drain: a region that asks one page at a
// time goes on reusing the connection it kept.
func (s *Source) trim() {
	for {
		if len(s.idle) <= 1 {
			return
		}
		select {
		case conn := <-s.idle:
			s.discard(conn)
		default:
			return
		}
	}
}

func (s *Source) drain() {
	for {
		select {
		case conn := <-s.idle:
			s.discard(conn)
		default:
			return
		}
	}
}

func statusError(status migratev1.Status) error {
	switch status {
	case migratev1.Status_STATUS_UNKNOWN_VM:
		return ErrNotServed
	case migratev1.Status_STATUS_UNKNOWN_VOLUME:
		return fmt.Errorf("%w: the source does not serve that volume", ErrNotServed)
	case migratev1.Status_STATUS_BUSY:
		return ErrBusy
	default:
		return fmt.Errorf("vmmigrate: the source refused a page request: %s", status)
	}
}

// exchange sends one request and reads the reply that answers it.
func exchange(ctx context.Context, conn platform.Conn, id uint64, request, response proto.Message, maxPayload int64) ([]byte, error) {
	frame, err := wire.Encode(wire.Outgoing{RequestID: id, Message: request})
	if err != nil {
		return nil, err
	}
	if err := conn.Send(ctx, frame); err != nil {
		return nil, err
	}
	received, err := conn.Receive(ctx)
	if err != nil {
		return nil, err
	}
	incoming, err := wire.Decode(received)
	if err != nil {
		return nil, err
	}
	if incoming.InReplyTo != id {
		_ = incoming.Payload.Close()
		return nil, wire.ErrMalformedFrame
	}
	payload, err := readPayload(incoming, maxPayload)
	if err != nil {
		return nil, err
	}
	if err := incoming.UnmarshalTo(response); err != nil {
		return nil, err
	}
	return payload, nil
}

func readPayload(incoming wire.Incoming, maximum int64) ([]byte, error) {
	defer incoming.Payload.Close()
	if incoming.PayloadSize < 0 || incoming.PayloadSize > maximum {
		return nil, platform.ErrMessageTooLarge
	}
	payload, err := io.ReadAll(io.LimitReader(incoming.Payload, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) != incoming.PayloadSize {
		return nil, io.ErrUnexpectedEOF
	}
	return payload, nil
}
