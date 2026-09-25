package sim

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

type NetworkConfig struct {
	Latency        time.Duration
	Jitter         time.Duration
	ConnectLatency time.Duration
	InboxSize      int
	MaxHeaderSize  int
	MaxPayloadSize int64
	BytesPerSecond int64
}

func (c NetworkConfig) withDefaults(defaults NetworkConfig) NetworkConfig {
	c.Latency = cmp.Or(c.Latency, defaults.Latency)
	c.Jitter = cmp.Or(c.Jitter, defaults.Jitter)
	c.ConnectLatency = cmp.Or(c.ConnectLatency, defaults.ConnectLatency)
	c.InboxSize = cmp.Or(c.InboxSize, defaults.InboxSize)
	c.MaxHeaderSize = cmp.Or(c.MaxHeaderSize, defaults.MaxHeaderSize)
	c.MaxPayloadSize = cmp.Or(c.MaxPayloadSize, defaults.MaxPayloadSize)
	c.BytesPerSecond = cmp.Or(c.BytesPerSecond, defaults.BytesPerSecond)
	return c
}

type LinkConfig struct {
	Latency time.Duration
	Jitter  time.Duration
	Blocked bool
}

type linkKey struct {
	from platform.Address
	to   platform.Address
}

type linkState struct {
	config        *LinkConfig
	sequence      uint64
	dropNext      int
	duplicateNext int
	corruptNext   int
	delayNext     []time.Duration
	// clogFrom and clogUntil are the simulated interval this link is blocked
	// for. A clog heals by the clock rather than by a call, so a swizzle is a
	// schedule and not a set of goroutines waiting to undo themselves.
	clogFrom  time.Time
	clogUntil time.Time
}

// Network is an in-memory, message-framed network. Link controls are
// directional so asymmetric partitions can be represented.
type Network struct {
	runtime *Runtime
	config  NetworkConfig

	mu        sync.Mutex
	listeners map[platform.Address]*listener
	links     map[linkKey]*linkState
	dials     map[linkKey]uint64
}

func newNetwork(runtime *Runtime, config NetworkConfig) *Network {
	return &Network{
		runtime:   runtime,
		config:    config,
		listeners: make(map[platform.Address]*listener),
		links:     make(map[linkKey]*linkState),
		dials:     make(map[linkKey]uint64),
	}
}

func (n *Network) Listen(address platform.Address) (platform.Listener, error) {
	if address == "" {
		return nil, platform.ErrInvalidPath
	}
	l := &listener{
		network:  n,
		address:  address,
		incoming: make(chan platform.Conn, n.config.InboxSize),
		done:     make(chan struct{}),
		pending:  make(map[platform.Conn]struct{}),
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, exists := n.listeners[address]; exists {
		return nil, platform.ErrAlreadyExists
	}
	n.listeners[address] = l
	n.runtime.trace.record(Event{Kind: "network", Resource: string(address), Operation: "listen", Outcome: "ok"})
	return l, nil
}

func (n *Network) Dial(ctx context.Context, from, to platform.Address) (platform.Conn, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if from == "" || to == "" {
		return nil, platform.ErrInvalidPath
	}
	if err := n.runtime.Admit(ctx, fmt.Sprintf("network/dial/%q/%q", from, to)); err != nil {
		return nil, err
	}
	key := linkKey{from: from, to: to}
	n.mu.Lock()
	n.dials[key]++
	dialID := n.dials[key]
	n.mu.Unlock()
	if n.runtime.wait != nil {
		// Missing or partitioned listeners fail before the latency wait. Their
		// admission must also be controlled, before the lookup decides outcome.
		if err := n.runtime.delay(ctx, fmt.Sprintf("network/dial-admit/%q/%q/%d", from, to, dialID), 0, 0, 0); err != nil {
			n.traceNetwork(key, "dial", "canceled", 0, dialID)
			return nil, err
		}
	}
	n.mu.Lock()
	l := n.listeners[to]
	blocked := n.linkLocked(key).blocked(n.config, n.runtime.Now())
	n.mu.Unlock()
	if l == nil {
		n.traceNetwork(key, "dial", "not_found", 0, dialID)
		return nil, platform.ErrNotFound
	}
	if blocked {
		n.traceNetwork(key, "dial", "blocked", 0, dialID)
		return nil, platform.ErrUnavailable
	}
	if err := n.runtime.ioDelay(ctx, fmt.Sprintf("network/dial/%q/%q/%d", from, to, dialID), n.config.ConnectLatency); err != nil {
		n.traceNetwork(key, "dial", "canceled", 0, dialID)
		return nil, err
	}

	pipe := &connectionPipe{done: make(chan struct{})}
	client := newConn(n, pipe, from, to)
	server := newConn(n, pipe, to, from)
	client.id = fmt.Sprintf("%q/%q/%d", from, to, dialID)
	server.id = client.id
	client.peer = server
	server.peer = client

	// A listener owns connections until Accept transfers them to the server.
	// Register under the same lock as Close so a dial delayed above cannot
	// enqueue an orphan after its listener has stopped accepting.
	n.mu.Lock()
	if n.listeners[to] != l {
		n.mu.Unlock()
		_ = client.Close()
		n.traceNetwork(key, "dial", "closed", 0, dialID)
		return nil, platform.ErrClosed
	}
	l.pending[server] = struct{}{}
	n.mu.Unlock()

	var err error
	select {
	case <-ctx.Done():
		err = context.Cause(ctx)
	case <-l.done:
		err = platform.ErrClosed
	case l.incoming <- server:
		n.traceNetwork(key, "dial", "ok", 0, dialID)
		return client, nil
	}
	_ = client.Close()
	n.mu.Lock()
	delete(l.pending, server)
	n.mu.Unlock()
	return nil, err
}

func (n *Network) SetLink(from, to platform.Address, config LinkConfig) {
	n.mu.Lock()
	defer n.mu.Unlock()
	copy := config
	n.linkLocked(linkKey{from: from, to: to}).config = &copy
}

func (n *Network) ClearLink(from, to platform.Address) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.linkLocked(linkKey{from: from, to: to}).config = nil
}

// ClearFaults drops the one-shot faults armed on a link and not yet spent: the
// drops, duplicates, corruptions and delays a caller asked for. A campaign
// whose faults have a window needs it — a drop still queued when the fault
// ended is a fault that outlived its window and lands on whatever the link
// carries next, which is somebody else's operation.
func (n *Network) ClearFaults(from, to platform.Address) {
	n.mu.Lock()
	defer n.mu.Unlock()
	state := n.linkLocked(linkKey{from: from, to: to})
	state.dropNext, state.duplicateNext, state.corruptNext, state.delayNext = 0, 0, 0, nil
}

func (n *Network) Partition(from, to platform.Address) {
	n.mu.Lock()
	defer n.mu.Unlock()
	state := n.linkLocked(linkKey{from: from, to: to})
	config := state.effective(n.config)
	config.Blocked = true
	state.config = &config
}

func (n *Network) PartitionBoth(a, b platform.Address) {
	n.Partition(a, b)
	n.Partition(b, a)
}

func (n *Network) Heal(from, to platform.Address) {
	n.mu.Lock()
	defer n.mu.Unlock()
	state := n.linkLocked(linkKey{from: from, to: to})
	config := state.effective(n.config)
	config.Blocked = false
	state.config = &config
	// Healing a link ends whatever is blocking it, a clog included: a caller
	// that has decided this link works again should not have to know why it
	// did not.
	state.clogFrom, state.clogUntil = time.Time{}, time.Time{}
}

// Clog blocks the link from -> to until the simulated instant until, after
// which it carries traffic again with nothing further called. A dial or a send
// over a clogged link is refused with platform.ErrUnavailable, exactly as a
// partition refuses it; the difference is that a clog ends by itself, which is
// what makes a timeout the thing under test rather than the test's own healing.
//
// The instant is read through the runtime's clock, so an until computed from
// the same clock inside a testing/synctest bubble means what it says.
func (n *Network) Clog(from, to platform.Address, until time.Time) {
	n.clogBetween(from, to, n.runtime.Now(), until)
}

func (n *Network) clogBetween(from, to platform.Address, start, until time.Time) {
	n.mu.Lock()
	state := n.linkLocked(linkKey{from: from, to: to})
	state.clogFrom, state.clogUntil = start, until
	n.mu.Unlock()
	n.runtime.trace.record(Event{Kind: "network", Resource: string(from) + "->" + string(to),
		Operation: "clog", Outcome: "ok", Bytes: int(until.Sub(start))})
}

// Clogged reports whether the link from -> to is blocked at this instant, by a
// partition or by a clog. It is how a resource the simulated network does not
// carry — object storage, most of all — can still be taken away by a swizzle
// that names it as an endpoint.
func (n *Network) Clogged(from, to platform.Address) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.linkLocked(linkKey{from: from, to: to}).blocked(n.config, n.runtime.Now())
}

// Swizzle blocks every link among addrs at its own seeded offset inside the
// first half of window and heals each of them at its own seeded offset inside
// the second half, so the order links come back in is not the order they went
// away in. It is FoundationDB's champion bug finder: every pair is separated
// for an overlapping but different interval, which is how two writers end up
// believing different things at the same time.
//
// The schedule is decided here and read by the clock, so Swizzle returns
// immediately and nothing has to be waited on or undone. Every link is carrying
// traffic again once window has passed.
func (n *Network) Swizzle(addrs []platform.Address, window time.Duration, r Random) {
	if window <= 0 || len(addrs) < 2 {
		return
	}
	base := n.runtime.Now()
	half := window / 2
	for _, from := range addrs {
		for _, to := range addrs {
			if from == to {
				continue
			}
			id := string(from) + "->" + string(to)
			start := base.Add(r.Duration(id+"/clog", half))
			until := base.Add(half + r.Duration(id+"/heal", half))
			n.clogBetween(from, to, start, until)
		}
	}
}

func (n *Network) HealBoth(a, b platform.Address) {
	n.Heal(a, b)
	n.Heal(b, a)
}

func (n *Network) DropNext(from, to platform.Address, count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.linkLocked(linkKey{from: from, to: to}).dropNext += max(count, 0)
}

func (n *Network) DuplicateNext(from, to platform.Address, count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.linkLocked(linkKey{from: from, to: to}).duplicateNext += max(count, 0)
}

func (n *Network) CorruptNext(from, to platform.Address, count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.linkLocked(linkKey{from: from, to: to}).corruptNext += max(count, 0)
}

func (n *Network) DelayNext(from, to platform.Address, delay time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	state := n.linkLocked(linkKey{from: from, to: to})
	state.delayNext = append(state.delayNext, delay)
}

func (n *Network) linkLocked(key linkKey) *linkState {
	state := n.links[key]
	if state == nil {
		state = &linkState{}
		n.links[key] = state
	}
	return state
}

func (s *linkState) effective(defaults NetworkConfig) LinkConfig {
	if s.config != nil {
		return *s.config
	}
	return LinkConfig{Latency: defaults.Latency, Jitter: defaults.Jitter}
}

func (s *linkState) clogged(now time.Time) bool {
	return !s.clogUntil.IsZero() && !now.Before(s.clogFrom) && now.Before(s.clogUntil)
}

func (s *linkState) blocked(defaults NetworkConfig, now time.Time) bool {
	return s.effective(defaults).Blocked || s.clogged(now)
}

type sendPlan struct {
	minimum, maximum time.Duration
	sequence         uint64
	delay            time.Duration
	drop             bool
	duplicate        bool
	corrupt          bool
	blocked          bool
}

func (n *Network) planSend(key linkKey) sendPlan {
	n.mu.Lock()
	defer n.mu.Unlock()
	state := n.linkLocked(key)
	state.sequence++
	config := state.effective(n.config)
	plan := sendPlan{sequence: state.sequence, blocked: state.blocked(n.config, n.runtime.Now())}
	plan.minimum = max(0, config.Latency-config.Jitter)
	plan.maximum = max(0, config.Latency+config.Jitter)
	plan.delay = config.Latency + jitter(n.runtime.sample(fmt.Sprintf("network/%s/%s/%d", key.from, key.to, plan.sequence)), config.Jitter)
	if plan.delay < 0 {
		plan.delay = 0
	}
	if len(state.delayNext) > 0 {
		plan.delay += state.delayNext[0]
		plan.minimum += state.delayNext[0]
		plan.maximum += state.delayNext[0]
		state.delayNext = state.delayNext[1:]
	}
	if state.dropNext > 0 {
		state.dropNext--
		plan.drop = true
	}
	if state.duplicateNext > 0 {
		state.duplicateNext--
		plan.duplicate = true
	}
	if state.corruptNext > 0 {
		state.corruptNext--
		plan.corrupt = true
	}
	return plan
}

func (n *Network) traceNetwork(key linkKey, operation, outcome string, bytes int, localID uint64) {
	n.runtime.trace.record(Event{
		Kind:      "network",
		Resource:  string(key.from) + "->" + string(key.to),
		Operation: operation,
		Outcome:   outcome,
		Bytes:     bytes,
		LocalID:   localID,
	})
}

type listener struct {
	network  *Network
	address  platform.Address
	incoming chan platform.Conn
	done     chan struct{}
	once     sync.Once
	// pending is guarded by network.mu; accepted connections belong to their
	// caller and are deliberately not closed with the listener.
	pending map[platform.Conn]struct{}
}

func (l *listener) Accept(ctx context.Context) (platform.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-l.done:
		return nil, platform.ErrClosed
	case accepted := <-l.incoming:
		l.network.mu.Lock()
		delete(l.pending, accepted)
		select {
		case <-l.done:
			l.network.mu.Unlock()
			_ = accepted.Close()
			return nil, platform.ErrClosed
		default:
		}
		l.network.mu.Unlock()
		if l.network.runtime.wait != nil {
			if err := l.network.runtime.delay(ctx, "network/accept/"+accepted.(*conn).id, 0, 0, 0); err != nil {
				_ = accepted.Close()
				return nil, err
			}
		}
		return accepted, nil
	}
}

func (l *listener) Address() platform.Address { return l.address }

func (l *listener) Close() error {
	l.once.Do(func() {
		l.network.mu.Lock()
		close(l.done)
		if l.network.listeners[l.address] == l {
			delete(l.network.listeners, l.address)
		}
		for conn := range l.pending {
			_ = conn.Close()
		}
		clear(l.pending)
		l.network.mu.Unlock()
		l.network.runtime.trace.record(Event{Kind: "network", Resource: string(l.address), Operation: "close_listener", Outcome: "ok"})
	})
	return nil
}

type connectionPipe struct {
	done chan struct{}
	once sync.Once
}

type conn struct {
	sendSequence uint64
	id           string
	receives     atomic.Uint64
	network      *Network
	pipe         *connectionPipe
	local        platform.Address
	remote       platform.Address
	peer         *conn
	inbox        chan receivedFrameData
	send         chan struct{}
}

func newConn(network *Network, pipe *connectionPipe, local, remote platform.Address) *conn {
	c := &conn{
		network: network,
		pipe:    pipe,
		local:   local,
		remote:  remote,
		inbox:   make(chan receivedFrameData, network.config.InboxSize),
		send:    make(chan struct{}, 1),
	}
	c.send <- struct{}{}
	return c
}

type receivedFrameData struct {
	header  []byte
	payload []byte
}

func (c *conn) Send(ctx context.Context, frame platform.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if len(frame.Header) > c.network.config.MaxHeaderSize || frame.PayloadSize > c.network.config.MaxPayloadSize || frame.PayloadSize > int64(maxInt()) {
		return platform.ErrMessageTooLarge
	}
	totalBytes := len(frame.Header) + int(frame.PayloadSize)
	if err := c.connectionError(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.pipe.done:
		return platform.ErrDisconnected
	case <-c.send:
	}
	defer func() { c.send <- struct{}{} }()
	if err := c.connectionError(ctx); err != nil {
		return err
	}

	// Sending on different connections can race after one completion wakes
	// multiple callers. Admit by connection and direction before consuming the
	// link's shared fault/timing sequence, so that assignment is also controlled.
	if c.network.runtime.wait != nil {
		c.sendSequence++ // this direction owns c.send
		id := fmt.Sprintf("network/send-admit/%s/%q/%d", c.id, c.local, c.sendSequence)
		if err := c.network.runtime.delay(ctx, id, 0, 0, 0); err != nil {
			return err
		}
	}
	key := linkKey{from: c.local, to: c.remote}
	plan := c.network.planSend(key)
	if plan.blocked {
		c.network.traceNetwork(key, "send", "blocked", totalBytes, plan.sequence)
		return platform.ErrUnavailable
	}
	payload := make([]byte, int(frame.PayloadSize))
	if frame.PayloadSize > 0 {
		if _, err := io.ReadFull(io.NewSectionReader(frame.Payload, 0, frame.PayloadSize), payload); err != nil {
			c.network.traceNetwork(key, "send", "read_error", 0, plan.sequence)
			return fmt.Errorf("read frame payload: %w", err)
		}
	}
	if err := c.network.runtime.delay(ctx, fmt.Sprintf("network/send/%q/%q/%d", key.from, key.to, plan.sequence),
		operationLatency(plan.minimum, totalBytes, c.network.config.BytesPerSecond),
		operationLatency(plan.maximum, totalBytes, c.network.config.BytesPerSecond),
		operationLatency(plan.delay, totalBytes, c.network.config.BytesPerSecond)); err != nil {
		c.network.traceNetwork(key, "send", "canceled", totalBytes, plan.sequence)
		return err
	}
	// A closed pipe and a writable buffered inbox can both be ready. Check the
	// prior close explicitly so select cannot turn a disconnected send into a
	// successful delivery. A close concurrent with the enqueue may still race.
	if err := c.connectionError(ctx); err != nil {
		c.network.traceNetwork(key, "send", connectionOutcome(err), totalBytes, plan.sequence)
		return err
	}
	if plan.drop {
		c.network.traceNetwork(key, "send", "dropped", totalBytes, plan.sequence)
		return nil
	}
	header := append([]byte(nil), frame.Header...)
	if plan.corrupt && totalBytes > 0 {
		index := c.network.runtime.sample(fmt.Sprintf("network-corrupt/%s/%s/%d", key.from, key.to, plan.sequence)) % uint64(totalBytes)
		if index < uint64(len(header)) {
			header[index] ^= 0x01
		} else {
			payload[index-uint64(len(header))] ^= 0x01
		}
	}
	copies := 1
	if plan.duplicate {
		copies = 2
	}
	for range copies {
		if err := c.connectionError(ctx); err != nil {
			c.network.traceNetwork(key, "send", connectionOutcome(err), totalBytes, plan.sequence)
			return err
		}
		clone := receivedFrameData{
			header:  append([]byte(nil), header...),
			payload: append([]byte(nil), payload...),
		}
		select {
		case <-ctx.Done():
			c.network.traceNetwork(key, "send", "canceled", totalBytes, plan.sequence)
			return context.Cause(ctx)
		case <-c.pipe.done:
			c.network.traceNetwork(key, "send", "disconnected", totalBytes, plan.sequence)
			return platform.ErrDisconnected
		case c.peer.inbox <- clone:
		}
	}
	outcome := "delivered"
	if plan.duplicate {
		outcome = "duplicated"
	} else if plan.corrupt {
		outcome = "corrupted"
	}
	c.network.traceNetwork(key, "send", outcome, totalBytes, plan.sequence)
	return nil
}

func (c *conn) connectionError(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	select {
	case <-c.pipe.done:
		return platform.ErrDisconnected
	default:
		return nil
	}
}

func connectionOutcome(err error) string {
	if err == platform.ErrDisconnected {
		return "disconnected"
	}
	return "canceled"
}

func (c *conn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	select {
	case <-ctx.Done():
		return platform.ReceivedFrame{}, context.Cause(ctx)
	case <-c.pipe.done:
		return platform.ReceivedFrame{}, platform.ErrDisconnected
	case frame := <-c.inbox:
		if c.network.runtime.wait != nil {
			if err := c.network.runtime.delay(ctx, fmt.Sprintf("network/receive/%s/%q/%d", c.id, c.local, c.receives.Add(1)), 0, 0, 0); err != nil {
				return platform.ReceivedFrame{}, err
			}
		}
		return platform.ReceivedFrame{
			Header:      frame.header,
			Payload:     io.NopCloser(bytes.NewReader(frame.payload)),
			PayloadSize: int64(len(frame.payload)),
		}, nil
	}
}

func (c *conn) LocalAddress() platform.Address  { return c.local }
func (c *conn) RemoteAddress() platform.Address { return c.remote }

func (c *conn) Close() error {
	c.pipe.once.Do(func() { close(c.pipe.done) })
	return nil
}

var _ platform.Network = (*Network)(nil)
var _ platform.Listener = (*listener)(nil)
var _ platform.Conn = (*conn)(nil)
