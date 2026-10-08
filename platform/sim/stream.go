package sim

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/framer"
)

// The network's own fault-injection sites. Like every site they are off unless
// the runtime's buggify switch is on, and then activated per run by the seed.
const (
	// SiteRandomClose closes a connection under a frame, before or after it
	// arrives.
	SiteRandomClose = "sim/network/random-close"
	// SiteHeaderBitFlip flips one bit of a frame's header on a framed link.
	SiteHeaderBitFlip = "sim/network/header-bit-flip"
	// SiteStreamBitFlip flips one bit of the leading bytes of a write to a
	// byte stream, which is where a frame's prefix and header are.
	SiteStreamBitFlip = "sim/network/stream-bit-flip"
	// SiteListenFails refuses a listen, as an address another process holds
	// or a process out of descriptors refuses one.
	SiteListenFails = "sim/network/listen-fails"
	// SiteDialRefused refuses a dial to a listening address, as a full
	// backlog or a lost SYN refuses or times one out.
	SiteDialRefused = "sim/network/dial-refused"
	// SiteAcceptFails fails an accept with a connection waiting, as a process
	// out of descriptors fails one: the connection waits for the next.
	SiteAcceptFails = "sim/network/accept-fails"
	// SiteSendTimedOut and SiteReceiveTimedOut end a connection as the kernel
	// ends one whose peer stopped answering: the sent bytes went
	// unacknowledged past the user timeout, or the keepalive probes did.
	SiteSendTimedOut    = "sim/network/send-timed-out"
	SiteReceiveTimedOut = "sim/network/receive-timed-out"
)

// The chance that each of the network's failure sites fires on one call, once
// a seed has activated it.
const (
	listenFailsProbability = 0.05
	dialRefusedProbability = 0.02
	acceptFailsProbability = 0.02
	timedOutProbability    = 0.005
)

// The errors the failure sites return. A stream's are the socket errors a real
// one returns, which the framer turns into the platform's as it does those.
var (
	errListenFails  = fmt.Errorf("%w: listen: address already in use", platform.ErrUnavailable)
	errDialRefused  = fmt.Errorf("%w: dial: connection refused", platform.ErrUnavailable)
	errAcceptFails  = fmt.Errorf("%w: accept: too many open files", platform.ErrUnavailable)
	errConnTimedOut = fmt.Errorf("%w: the connection timed out", platform.ErrUnavailable)
)

// streamFlipSpan is how far into a write the stream bit-flip site reaches: a
// frame's 20-byte prefix and the start of its header.
const streamFlipSpan = 64

// streamListeners is the byte-stream listeners by address.
type streamListeners map[platform.Address]*streamListener

// Framed is this network as a host's real network is: byte streams over the
// simulated links, framed by the same code the TCP adapter runs. A write is split
// into pieces of seeded lengths, down to a byte, each crossing the link on its
// own, and a read returns a seeded part of what has arrived, so every frame
// crosses in pieces and the framer reassembles it. A refused link resets the
// stream; a held one holds its bytes. The one-shot drops, duplicates and delays
// are faults of a framed link and do not reach a stream.
//
// Framed connections do not take part in a Scheduler's completion order: a
// controlled run uses the framed network itself.
func (n *Network) Framed() platform.Network { return framedNetwork{network: n} }

type framedNetwork struct{ network *Network }

func (f framedNetwork) config() framer.Config {
	return framer.Config{MaxHeaderSize: uint32(f.network.config.MaxHeaderSize),
		MaxPayloadSize: uint64(f.network.config.MaxPayloadSize)}
}

func (f framedNetwork) Listen(address platform.Address) (platform.Listener, error) {
	listener, err := f.network.listenStream(address)
	if err != nil {
		return nil, err
	}
	return framedListener{streamListener: listener, config: f.config()}, nil
}

func (f framedNetwork) Dial(ctx context.Context, from, to platform.Address) (platform.Conn, error) {
	stream, err := f.network.dialStream(ctx, from, to)
	if err != nil {
		return nil, err
	}
	return framer.NewConn(stream, f.config()), nil
}

type framedListener struct {
	*streamListener
	config framer.Config
}

func (l framedListener) Accept(ctx context.Context) (platform.Conn, error) {
	stream, err := l.accept(ctx)
	if err != nil {
		return nil, err
	}
	return framer.NewConn(stream, l.config), nil
}

// streamListener hands dialed streams to Accept.
type streamListener struct {
	network  *Network
	address  platform.Address
	incoming chan *streamConn
	done     chan struct{}
	once     sync.Once

	mu sync.Mutex
	// held is a connection an accept failed with waiting, which the next
	// accept takes first, and closed says Close has run.
	held   *streamConn
	closed bool
}

func (n *Network) listenStream(address platform.Address) (*streamListener, error) {
	if address == "" {
		return nil, platform.ErrInvalidPath
	}
	if n.runtime.buggifyHere(SiteListenFails, listenFailsProbability) {
		n.runtime.trace.record(Event{Kind: "network", Resource: string(address), Operation: "listen_stream", Outcome: "refused"})
		return nil, &net.OpError{Op: "listen", Net: "sim", Err: syscall.EADDRINUSE}
	}
	l := &streamListener{network: n, address: address,
		incoming: make(chan *streamConn, n.config.InboxSize), done: make(chan struct{})}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, exists := n.streams[address]; exists {
		return nil, platform.ErrAlreadyExists
	}
	n.streams[address] = l
	n.runtime.trace.record(Event{Kind: "network", Resource: string(address), Operation: "listen_stream", Outcome: "ok"})
	return l, nil
}

func (l *streamListener) accept(ctx context.Context) (*streamConn, error) {
	l.mu.Lock()
	held := l.held
	l.held = nil
	l.mu.Unlock()
	if held != nil {
		return held, nil
	}
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-l.done:
		return nil, platform.ErrClosed
	case conn := <-l.incoming:
		if l.network.runtime.buggifyHere(SiteAcceptFails, acceptFailsProbability) {
			// The connection waits for the next accept, as one left in the
			// backlog does.
			l.mu.Lock()
			l.held = conn
			closed := l.closed
			l.mu.Unlock()
			if closed {
				_ = conn.Close()
			}
			l.network.runtime.trace.record(Event{Kind: "network", Resource: string(l.address), Operation: "accept_stream", Outcome: "failed"})
			return nil, &net.OpError{Op: "accept", Net: "sim", Err: syscall.EMFILE}
		}
		return conn, nil
	}
}

func (l *streamListener) Address() platform.Address { return l.address }

func (l *streamListener) Close() error {
	l.once.Do(func() {
		l.network.mu.Lock()
		close(l.done)
		if l.network.streams[l.address] == l {
			delete(l.network.streams, l.address)
		}
		l.network.mu.Unlock()
		l.mu.Lock()
		l.closed = true
		if l.held != nil {
			_ = l.held.Close()
			l.held = nil
		}
		l.mu.Unlock()
		for {
			select {
			case conn := <-l.incoming:
				_ = conn.Close()
			default:
				return
			}
		}
	})
	return nil
}

// dialStream opens a byte stream from -> to, under the same link faults as a
// framed dial: a refused link refuses it, a held one holds it, and a dial to
// nobody is refused or hangs as the network is configured.
func (n *Network) dialStream(ctx context.Context, from, to platform.Address) (*streamConn, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if from == "" || to == "" {
		return nil, platform.ErrInvalidPath
	}
	key := linkKey{from: from, to: to}
	n.mu.Lock()
	n.dials[key]++
	dialID := n.dials[key]
	n.mu.Unlock()
	if err := n.waitOutHold(ctx, key); err != nil {
		return nil, err
	}
	n.mu.Lock()
	l := n.streams[to]
	blocked := n.linkLocked(key).blocked(n.config, n.runtime.Now())
	n.mu.Unlock()
	if l == nil && n.config.HangDeadDials {
		n.traceNetwork(key, "dial_stream", "hanging", 0, dialID)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	if l == nil {
		n.traceNetwork(key, "dial_stream", "not_found", 0, dialID)
		return nil, platform.ErrNotFound
	}
	if blocked {
		n.traceNetwork(key, "dial_stream", "blocked", 0, dialID)
		return nil, platform.ErrUnavailable
	}
	if err := n.runtime.sleep(ctx, n.config.ConnectLatency); err != nil {
		return nil, err
	}
	if n.runtime.buggifyHere(SiteDialRefused, dialRefusedProbability) {
		n.traceNetwork(key, "dial_stream", "refused", 0, dialID)
		return nil, &net.OpError{Op: "dial", Net: "sim", Err: syscall.ECONNREFUSED}
	}
	pipe := &connectionPipe{done: make(chan struct{})}
	id := fmt.Sprintf("%q/%q/%d", from, to, dialID)
	client := newStreamConn(n, pipe, id, from, to, n.sendBuffer(key, dialID, "stream-client"))
	server := newStreamConn(n, pipe, id, to, from, n.sendBuffer(key, dialID, "stream-server"))
	client.peer, server.peer = server, client
	select {
	case <-ctx.Done():
		_ = client.Close()
		return nil, context.Cause(ctx)
	case <-l.done:
		_ = client.Close()
		return nil, platform.ErrClosed
	case l.incoming <- server:
	}
	n.traceNetwork(key, "dial_stream", "ok", 0, dialID)
	return client, nil
}

// streamConn is one end of a simulated byte stream.
type streamConn struct {
	network *Network
	pipe    *connectionPipe
	id      string
	local   platform.Address
	remote  platform.Address
	peer    *streamConn
	// buffer bounds what this end holds that it has not read, zero for none.
	buffer int64
	// writes serializes this end's writes and numbers them.
	writes  chan struct{}
	written uint64

	mu sync.Mutex
	// arrived is what has crossed to this end and not been read, and changed
	// is closed and replaced whenever it or a deadline moves.
	arrived       []byte
	reads         uint64
	changed       chan struct{}
	readDeadline  time.Time
	writeDeadline time.Time
}

func newStreamConn(n *Network, pipe *connectionPipe, id string, local, remote platform.Address, buffer int64) *streamConn {
	c := &streamConn{network: n, pipe: pipe, id: id, local: local, remote: remote, buffer: buffer,
		writes: make(chan struct{}, 1), changed: make(chan struct{})}
	c.writes <- struct{}{}
	return c
}

// signal wakes whatever waits on this end. Caller holds c.mu.
func (c *streamConn) signal() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// timeoutError is a deadline passing, as the net package reports one.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
func (timeoutError) Unwrap() error   { return os.ErrDeadlineExceeded }

// wait blocks until changed closes, the connection closes or deadline passes.
func (c *streamConn) wait(changed <-chan struct{}, deadline time.Time) error {
	var expired <-chan time.Time
	if !deadline.IsZero() {
		remaining := deadline.Sub(c.network.runtime.Now())
		if remaining <= 0 {
			return timeoutError{}
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-changed:
		return nil
	case <-c.pipe.done:
		return nil
	case <-expired:
		return timeoutError{}
	}
}

func (c *streamConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if c.network.runtime.buggifyHere(SiteReceiveTimedOut, timedOutProbability) {
		c.network.traceNetwork(linkKey{from: c.remote, to: c.local}, "read", "timed_out", 0, 0)
		_ = c.Close()
		return 0, &net.OpError{Op: "read", Net: "sim", Err: syscall.ETIMEDOUT}
	}
	for {
		c.mu.Lock()
		if len(c.arrived) > 0 {
			// A read returns a seeded part of what has arrived, as a socket
			// returns what the kernel happens to hold.
			c.reads++
			most := min(len(b), len(c.arrived))
			n := 1 + c.network.runtime.Random("network/stream-read").Intn(
				fmt.Sprintf("%s/%s/%d", c.id, c.local, c.reads), most)
			copy(b, c.arrived[:n])
			c.arrived = c.arrived[n:]
			c.signal()
			c.mu.Unlock()
			return n, nil
		}
		select {
		case <-c.pipe.done:
			c.mu.Unlock()
			return 0, net.ErrClosed
		default:
		}
		changed, deadline := c.changed, c.readDeadline
		c.mu.Unlock()
		if err := c.wait(changed, deadline); err != nil {
			return 0, err
		}
	}
}

func (c *streamConn) Write(b []byte) (int, error) {
	select {
	case <-c.pipe.done:
		return 0, net.ErrClosed
	case <-c.writes:
	}
	defer func() { c.writes <- struct{}{} }()
	c.written++
	key := linkKey{from: c.local, to: c.remote}
	if c.network.runtime.buggifyHere(SiteSendTimedOut, timedOutProbability) {
		c.network.traceNetwork(key, "write", "timed_out", len(b), c.written)
		_ = c.Close()
		return 0, &net.OpError{Op: "write", Net: "sim", Err: syscall.ETIMEDOUT}
	}
	random := c.network.runtime.Random("network/stream-write")
	sent := 0
	for piece := 0; sent < len(b); piece++ {
		id := fmt.Sprintf("%s/%s/%d/%d", c.id, c.local, c.written, piece)
		// A write ends short at random, down to a byte, as FoundationDB's
		// simulated streams end theirs: half the time to under a thousand
		// bytes, and otherwise to under 64 KiB.
		limit := min(len(b)-sent, 64<<10)
		if random.Chance(id+"/short", 0.5) {
			limit = min(limit, 1000)
		}
		size := 1 + random.Intn(id+"/size", limit)
		piece := append([]byte(nil), b[sent:sent+size]...)
		if sent == 0 && c.network.runtime.buggifyHere(SiteStreamBitFlip, 0.01) {
			bit := random.Intn(id+"/flip", min(len(piece), streamFlipSpan)*8)
			piece[bit/8] ^= 1 << (bit % 8)
			c.network.traceNetwork(key, "write", "flipped", size, c.written)
		}
		plan := c.network.planSend(key, size)
		if plan.blocked {
			// A refused link resets the stream, both ends of it.
			c.network.traceNetwork(key, "write", "reset", size, c.written)
			_ = c.Close()
			return sent, platform.ErrUnavailable
		}
		bytesPerSecond := c.network.config.BytesPerSecond
		if c.network.config.LinkBytesPerSecond > 0 {
			bytesPerSecond = 0
		}
		if err := c.cross(operationLatency(plan.delay, size, bytesPerSecond), piece); err != nil {
			return sent, err
		}
		sent += size
	}
	c.network.traceNetwork(key, "write", "delivered", len(b), c.written)
	return sent, nil
}

// cross carries one piece over the link: it arrives at the other end after
// delay, once that end has room for it.
func (c *streamConn) cross(delay time.Duration, piece []byte) error {
	arrive := c.network.runtime.Now().Add(delay)
	for {
		now := c.network.runtime.Now()
		if !now.Before(arrive) {
			break
		}
		select {
		case <-c.pipe.done:
			// A piece on the wire when the connection closes never arrives.
			return net.ErrClosed
		default:
		}
		c.mu.Lock()
		changed, deadline := c.changed, c.writeDeadline
		c.mu.Unlock()
		// The piece is on the wire until it arrives, and a deadline that
		// passes first — the one a cancelled send sets — ends the write.
		wake := arrive
		if !deadline.IsZero() && deadline.Before(wake) {
			wake = deadline
		}
		if err := c.wait(changed, wake); err != nil && !wake.Equal(arrive) {
			return err
		}
	}
	c.mu.Lock()
	deadline := c.writeDeadline
	c.mu.Unlock()
	peer := c.peer
	for {
		peer.mu.Lock()
		select {
		case <-c.pipe.done:
			peer.mu.Unlock()
			return net.ErrClosed
		default:
		}
		if peer.buffer <= 0 || len(peer.arrived) == 0 || int64(len(peer.arrived)+len(piece)) <= peer.buffer {
			peer.arrived = append(peer.arrived, piece...)
			peer.signal()
			peer.mu.Unlock()
			return nil
		}
		changed := peer.changed
		peer.mu.Unlock()
		if err := c.wait(changed, deadline); err != nil {
			return err
		}
	}
}

func (c *streamConn) Close() error {
	c.pipe.once.Do(func() { close(c.pipe.done) })
	return nil
}

func (c *streamConn) LocalAddr() net.Addr  { return streamAddr(c.local) }
func (c *streamConn) RemoteAddr() net.Addr { return streamAddr(c.remote) }

func (c *streamConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline, c.writeDeadline = t, t
	c.signal()
	return nil
}

func (c *streamConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.signal()
	return nil
}

func (c *streamConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	c.signal()
	return nil
}

// streamAddr is a simulated address as the net package names one.
type streamAddr platform.Address

func (a streamAddr) Network() string { return "sim" }
func (a streamAddr) String() string  { return string(a) }

var _ net.Conn = (*streamConn)(nil)
var _ platform.Network = framedNetwork{}
