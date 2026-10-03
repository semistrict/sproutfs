package peer

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"sync"

	migratev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/migrate/v1"
	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"google.golang.org/protobuf/proto"
)

var payloadTable = crc32.MakeTable(crc32.Castagnoli)

// session is one connection a peer opened: the version it speaks, the class of
// traffic it carries, and the order its replies leave in.
//
// A session of version 2 reads its next request while the ones before it are
// still being answered, so a request that waits on a disk does not stop the
// connection from being read. Its replies leave in the order the requests
// arrived: each waits for the one before it. That keeps what goes over one
// connection a function of what came in rather than of which answer happened
// to finish first, and a peer that wants one request not to wait on another
// sends it on another connection of its pool.
type session struct {
	conn    platform.Conn
	peer    string
	version uint32
	class   Class
	// send serializes the frames this side writes. It is a channel rather
	// than a mutex, so a reply waiting on another is a wait a simulation sees.
	send chan struct{}
	// turn is closed once the reply to the request before the next one has
	// been sent.
	turn chan struct{}
	// inflight is how many requests are being answered, bounded by
	// ServerConfig.MaxInFlight.
	inflight chan struct{}
	handlers sync.WaitGroup
}

// answer is what a request is answered with: a message, a payload beside it,
// and what is to happen once the reply has left or failed to.
type answer struct {
	message proto.Message
	payload []byte
	// checked says the payload carries a CRC32C of its own in the header. A
	// payload whose own format is checked goes without one.
	checked bool
	// sent, when set, runs once the reply has been sent, with whether it was.
	sent func(sent bool)
}

func (s *Server) serveConn(conn platform.Conn) {
	defer conn.Close()
	session, first, err := s.open(conn)
	if err != nil {
		return
	}
	defer session.handlers.Wait()
	if session.version < 2 {
		s.serveOneAtATime(session, first)
		return
	}
	for {
		incoming, err := s.receive(session)
		if err != nil {
			return
		}
		if err := s.admit(session, incoming); err != nil {
			return
		}
	}
}

// serveOneAtATime is version 1: each request is answered before the next is
// read, which is what the release before this one asks of a connection.
func (s *Server) serveOneAtATime(session *session, first *wire.Incoming) {
	incoming := *first
	for {
		answer, err := s.answer(session, incoming)
		if err != nil {
			return
		}
		if err := s.reply(session, incoming.RequestID, answer); err != nil {
			return
		}
		if incoming, err = s.receive(session); err != nil {
			return
		}
	}
}

// open reads a connection's first frame and settles the version it speaks. A
// hello is answered with the version both ends share, or with INCOMPATIBLE and
// this server's range, after which the connection closes. Any other first frame
// is a dialer of the release before this one, which sent no hello: if this
// server still speaks version 1, that frame is its first request.
func (s *Server) open(conn platform.Conn) (*session, *wire.Incoming, error) {
	opened := &session{conn: conn, peer: peerKey(conn.RemoteAddress()), version: helloVersion,
		send: make(chan struct{}, 1), turn: make(chan struct{}),
		inflight: make(chan struct{}, s.config.MaxInFlight)}
	opened.send <- struct{}{}
	close(opened.turn)
	received, err := conn.Receive(s.ctx)
	if err != nil {
		return nil, nil, err
	}
	incoming, err := wire.Decode(received)
	if err != nil {
		return nil, nil, err
	}
	hello := new(peerv1.Hello)
	if !incoming.Message.MessageIs(hello) {
		if incoming.Version != 1 || s.config.Versions.Min > 1 {
			_ = incoming.Payload.Close()
			return nil, nil, fmt.Errorf("%w: a first frame of version %d that is not a hello", wire.ErrMalformedFrame, incoming.Version)
		}
		// The release before kept one budget for all of a peer's requests,
		// and its destination asked for faults and the stream alike over every
		// connection: they count as the stream does.
		opened.version, opened.class = 1, BulkRead
		return opened, &incoming, nil
	}
	if err := drain(incoming); err != nil {
		return nil, nil, err
	}
	if err := incoming.UnmarshalTo(hello); err != nil {
		return nil, nil, err
	}
	opened.class = classFromWire(hello.GetClass())
	reply, version, ok := answerHello(s.config.Versions, hello)
	reply.SetBudgetBytes(uint64(s.config.Budgets.Of(opened.class)))
	reply.SetMaxInFlight(uint32(s.config.MaxInFlight))
	if err := s.reply(opened, incoming.RequestID, answer{message: reply}); err != nil {
		return nil, nil, err
	}
	if !ok {
		s.incompatible.Add(1)
		sim.Probe(s.ctx, ProbeIncompatible)
		slog.WarnContext(s.ctx, "peer: a peer speaks no version this server speaks", "peer", opened.peer,
			"min_version", hello.GetMinVersion(), "max_version", hello.GetMaxVersion())
		return nil, nil, &IncompatibleError{Min: hello.GetMinVersion(), Max: hello.GetMaxVersion()}
	}
	opened.version = version
	return opened, nil, nil
}

// receive reads the next request of a session. A frame of another version than
// the one the session settled on is a dialer that does not keep to its own
// hello, and ends the connection.
func (s *Server) receive(session *session) (wire.Incoming, error) {
	received, err := session.conn.Receive(s.ctx)
	if err != nil {
		return wire.Incoming{}, err
	}
	incoming, err := wire.Decode(received)
	if err != nil {
		return wire.Incoming{}, err
	}
	if incoming.Version != session.version {
		_ = incoming.Payload.Close()
		return wire.Incoming{}, fmt.Errorf("%w: a frame of version %d on a connection of version %d",
			wire.ErrMalformedFrame, incoming.Version, session.version)
	}
	return incoming, nil
}

// admit starts answering one request of a version 2 session, and goes back to
// reading. The answer is sent once every reply before it has been.
func (s *Server) admit(session *session, incoming wire.Incoming) error {
	if err := drain(incoming); err != nil {
		return err
	}
	previous, next := session.turn, make(chan struct{})
	session.turn = next
	refused := false
	select {
	case session.inflight <- struct{}{}:
	default:
		// The peer asked for more than its hello allowed. It is told it is
		// busy, as it would be over its budget, rather than left unread.
		refused = true
	}
	session.handlers.Go(func() {
		defer close(next)
		var reply answer
		if refused {
			reply = answer{message: s.busy(session, 0, 0)}
		} else {
			defer func() { <-session.inflight }()
			var err error
			if reply, err = s.answer(session, incoming); err != nil {
				slog.WarnContext(s.ctx, "peer: a request this server cannot answer", "peer", session.peer, "error", err)
				_ = session.conn.Close()
				return
			}
		}
		select {
		case <-previous:
		case <-s.ctx.Done():
			if reply.sent != nil {
				reply.sent(false)
			}
			return
		}
		if err := s.reply(session, incoming.RequestID, reply); err != nil {
			_ = session.conn.Close()
		}
	})
	return nil
}

// answer dispatches one request to what answers it.
func (s *Server) answer(session *session, incoming wire.Incoming) (answer, error) {
	pageRequest, residentRequest, claimRequest := new(migratev1.PageRequest), new(migratev1.ResidentRequest),
		new(migratev1.ClaimRequest)
	switch {
	case incoming.Message.MessageIs(pageRequest):
		if err := incoming.UnmarshalTo(pageRequest); err != nil {
			return answer{}, err
		}
		return s.answerPages(session, pageRequest), nil
	case incoming.Message.MessageIs(residentRequest):
		if err := incoming.UnmarshalTo(residentRequest); err != nil {
			return answer{}, err
		}
		return s.answerResident(residentRequest), nil
	case incoming.Message.MessageIs(claimRequest):
		if err := incoming.UnmarshalTo(claimRequest); err != nil {
			return answer{}, err
		}
		return s.answerClaim(claimRequest), nil
	default:
		return answer{}, fmt.Errorf("%w: a request of a kind this server does not answer: %s",
			wire.ErrMalformedFrame, incoming.Message.GetTypeUrl())
	}
}

// reply sends one answer, at the session's version, and reports to the answer
// whether it left.
func (s *Server) reply(session *session, requestID uint64, reply answer) error {
	err := s.write(session, requestID, reply)
	if reply.sent != nil {
		reply.sent(err == nil)
	}
	return err
}

func (s *Server) write(session *session, requestID uint64, reply answer) error {
	payload := wire.Payload{Body: platform.Bytes(reply.payload), Size: int64(len(reply.payload))}
	if reply.checked || session.version < 2 {
		// The release before checks every payload it is sent.
		payload.Algorithm = wire.ChecksumCRC32C
		payload.Checksum = wire.EncodeCRC32C(crc32.Checksum(reply.payload, payloadTable))
	}
	frame, err := wire.Encode(wire.Outgoing{Version: session.version, RequestID: requestID,
		InReplyTo: requestID, Message: reply.message, Payload: payload})
	if err != nil {
		return err
	}
	select {
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	case <-session.send:
	}
	defer func() { session.send <- struct{}{} }()
	return session.conn.Send(s.ctx, frame)
}

// reserve takes bytes of the session's class from its peer's budget, and
// releases them with the function it returns. A peer at its budget gets the
// Busy it is answered with instead.
func (s *Server) reserve(session *session, bytes int64) (func(), *peerv1.Busy) {
	key := budgetKey{peer: session.peer, class: session.class}
	budget := s.config.Budgets.Of(session.class)
	if sim.Buggify(s.ctx, busySite, 0.5) {
		return nil, s.busy(session, bytes, budget)
	}
	if held, ok := s.budgets.reserve(key, bytes, budget); !ok {
		return nil, s.busy(session, bytes, held)
	}
	return func() { s.budgets.release(key, bytes) }, nil
}

// busy is the answer to a request that would take its peer's class past its
// budget: how much that class holds and may hold, and what was asked.
func (s *Server) busy(session *session, asked, held int64) *peerv1.Busy {
	s.refused.Add(1)
	s.probeBusy()
	class := classToWire(session.class)
	return peerv1.Busy_builder{Class: &class, HeldBytes: proto.Uint64(uint64(held)),
		BudgetBytes: proto.Uint64(uint64(s.config.Budgets.Of(session.class))),
		AskedBytes:  proto.Uint64(uint64(asked))}.Build()
}

// drain consumes a request's payload. No request this server answers carries
// one, so anything here is a malformed frame and the connection is dropped.
func drain(incoming wire.Incoming) error {
	defer incoming.Payload.Close()
	if incoming.PayloadSize != 0 {
		return platform.ErrMessageTooLarge
	}
	_, err := io.Copy(io.Discard, incoming.Payload)
	return err
}

func classToWire(class Class) peerv1.Class {
	switch class {
	case BulkRead:
		return peerv1.Class_CLASS_BULK_READ
	case BulkWrite:
		return peerv1.Class_CLASS_BULK_WRITE
	default:
		return peerv1.Class_CLASS_FAULT
	}
}

func classFromWire(class peerv1.Class) Class {
	switch class {
	case peerv1.Class_CLASS_BULK_READ:
		return BulkRead
	case peerv1.Class_CLASS_BULK_WRITE:
		return BulkWrite
	default:
		return Fault
	}
}
