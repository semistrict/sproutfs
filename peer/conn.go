package peer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
)

// conn is one connection of a pool. Requests on it are numbered, and one
// reader matches each reply to its request by number, so several are in flight
// at once and a caller that gives up does not leave the next request reading
// its reply.
type conn struct {
	pool *pool
	platform.Conn
	version     uint32
	maxInFlight int
	// inflight is guarded by pool.mu.
	inflight int

	// send serializes this end's frames, as a channel so a request waiting
	// on another's send is a wait a simulation sees. It also guards next.
	send chan struct{}
	next uint64

	mu      sync.Mutex
	pending map[uint64]*call
	failed  error
	done    chan struct{}
}

// call is one request in flight on a connection.
type call struct {
	// bytes is what the request holds of its pool's budget, and maxPayload
	// the largest payload its reply may carry.
	bytes, maxPayload int64
	// reply has room for the one result the reader delivers.
	reply chan result
	// abandoned marks a call its caller gave up on: its reply, when it comes,
	// still gives back what the request held.
	abandoned bool
}

// result is what came back for one request.
type result struct {
	incoming wire.Incoming
	payload  *payloadBuffer
	err      error
}

func newConn(p *pool, c platform.Conn, version uint32, maxInFlight int) *conn {
	opened := &conn{pool: p, Conn: c, version: version, maxInFlight: maxInFlight,
		send: make(chan struct{}, 1), pending: make(map[uint64]*call), done: make(chan struct{})}
	opened.send <- struct{}{}
	return opened
}

// roundTrip sends one request and waits for its reply. The caller has taken a
// slot on this connection and bytes of its pool's budget; both are given back
// when the reply arrives or the connection fails, whichever is first, and not
// when the caller gives up: the server holds them until it has answered.
func (c *conn) roundTrip(ctx context.Context, request proto.Message, bytes, maxPayload int64) (result, error) {
	waiting := &call{bytes: bytes, maxPayload: maxPayload, reply: make(chan result, 1)}
	select {
	case <-ctx.Done():
		c.pool.release(c, bytes)
		return result{}, context.Cause(ctx)
	case <-c.done:
		c.pool.release(c, bytes)
		return result{}, c.err()
	case <-c.send:
	}
	c.next++
	id := c.next
	c.mu.Lock()
	if c.failed != nil {
		c.mu.Unlock()
		c.send <- struct{}{}
		c.pool.release(c, bytes)
		return result{}, c.failed
	}
	c.pending[id] = waiting
	c.mu.Unlock()
	frame, err := wire.Encode(wire.Outgoing{Version: c.version, RequestID: id, Message: request})
	if err == nil {
		err = c.Send(ctx, frame)
	}
	c.send <- struct{}{}
	if err != nil {
		if c.forget(id) {
			c.pool.release(c, bytes)
		}
		if ctx.Err() == nil {
			// A send that failed for any reason but its caller's leaves a
			// stream the next request cannot trust.
			c.fail(err)
		}
		return result{}, err
	}
	select {
	case got := <-waiting.reply:
		return got, got.err
	case <-ctx.Done():
		c.mu.Lock()
		if _, outstanding := c.pending[id]; outstanding {
			waiting.abandoned = true
			c.mu.Unlock()
			return result{}, context.Cause(ctx)
		}
		c.mu.Unlock()
		// The reply arrived as the caller gave up: it has been delivered,
		// and nobody will read it.
		got := <-waiting.reply
		got.payload.release()
		return result{}, context.Cause(ctx)
	}
}

// forget takes a request out of the pending table, and reports whether it was
// still there to take.
func (c *conn) forget(id uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, outstanding := c.pending[id]; !outstanding {
		return false
	}
	delete(c.pending, id)
	return true
}

// read is the connection's one reader. It reads each reply's payload into a
// buffer of the length the frame states and hands it to the request it
// answers. A reply to no request in flight — one already delivered, which a
// duplicating link sends twice — is read and dropped.
func (c *conn) read() {
	ctx, stop := context.WithCancelCause(c.pool.peer.table.ctx)
	defer stop(nil)
	go func() {
		select {
		case <-c.done:
			stop(c.err())
		case <-ctx.Done():
		}
	}()
	for {
		received, err := c.Receive(ctx)
		if err != nil {
			c.fail(err)
			return
		}
		incoming, err := wire.Decode(received)
		if err != nil {
			c.fail(err)
			return
		}
		if incoming.Version != c.version {
			_ = incoming.Payload.Close()
			c.fail(fmt.Errorf("%w: a reply of version %d on a connection of version %d",
				wire.ErrMalformedFrame, incoming.Version, c.version))
			return
		}
		c.mu.Lock()
		waiting := c.pending[incoming.InReplyTo]
		delete(c.pending, incoming.InReplyTo)
		c.mu.Unlock()
		if waiting == nil {
			if err := incoming.Payload.Close(); err != nil {
				c.fail(err)
				return
			}
			continue
		}
		payload, err := readPayload(incoming, waiting.maxPayload)
		c.pool.release(c, waiting.bytes)
		if err != nil {
			// The payload was not read whole, so the next frame's start is
			// not known: nothing more can be read from here.
			waiting.reply <- result{err: err}
			c.fail(err)
			return
		}
		c.mu.Lock()
		abandoned := waiting.abandoned
		c.mu.Unlock()
		if abandoned {
			probeLateReply(ctx)
			payload.release()
			continue
		}
		waiting.reply <- result{incoming: incoming, payload: payload}
	}
}

// fail ends the connection: every request in flight on it ends with err and
// gives back what it held, and the pool stops using it.
func (c *conn) fail(err error) {
	c.mu.Lock()
	if c.failed != nil {
		c.mu.Unlock()
		return
	}
	if err == nil {
		err = ErrClosed
	}
	c.failed = err
	pending := c.pending
	c.pending = make(map[uint64]*call)
	close(c.done)
	c.mu.Unlock()
	_ = c.Close()
	c.pool.remove(c)
	for _, waiting := range pending {
		c.pool.release(c, waiting.bytes)
		waiting.reply <- result{err: err}
	}
}

func (c *conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed
}

// busyFrom reports the BUSY a reply carries, if it is one.
func busyFrom(incoming wire.Incoming) (*BusyError, bool) {
	busy := new(peerv1.Busy)
	if !incoming.Message.MessageIs(busy) {
		return nil, false
	}
	if err := incoming.UnmarshalTo(busy); err != nil {
		return nil, false
	}
	return &BusyError{Class: classFromWire(busy.GetClass()), Held: int64(busy.GetHeldBytes()),
		Budget: int64(busy.GetBudgetBytes()), Asked: int64(busy.GetAskedBytes())}, true
}

// ErrBusy reports a peer at its budget for this host's class of request.
// Nothing is wrong with it and nothing is wrong here: it did nothing this time,
// and the request may be made again.
var ErrBusy = errors.New("peer: the peer is at its budget for this host")

// BusyError says how busy: what this host's requests of the class hold at the
// peer, what they may, and what the refused request asked for. A server of the
// release before says only that it is busy.
type BusyError struct {
	Class               Class
	Held, Budget, Asked int64
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("%v: %s holds %d of %d bytes there, and asked for %d", ErrBusy, e.Class, e.Held, e.Budget, e.Asked)
}

func (e *BusyError) Unwrap() error { return ErrBusy }
