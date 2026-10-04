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

	// The faults below are what FoundationDB's simulator models and this one
	// once did not. Each is off at its zero value, so a world that asks for
	// none of them runs exactly as it always did.

	// TailEvery and TailLatency are a heavy latency tail: about one hop in
	// TailEvery takes up to TailLatency longer than its link's own latency.
	TailEvery   int
	TailLatency time.Duration
	// SlowPairPerMille and SlowPairLatency are links that stay slow: when two
	// hosts first connect, that many pairs in a thousand are given an extra
	// latency of up to SlowPairLatency, which every hop between them pays for
	// the rest of the run.
	SlowPairPerMille int
	SlowPairLatency  time.Duration
	// LinkBytesPerSecond is the bandwidth of each direction between two hosts,
	// shared by every connection between them: a frame is transmitted behind
	// the bytes sent before it on that pair. Zero charges each frame
	// BytesPerSecond on its own instead.
	LinkBytesPerSecond int64
	// SendBufferBytes bounds what one direction of a connection holds that its
	// reader has not taken, so a sender whose peer stops reading stalls rather
	// than growing memory. Each connection draws its own bound between half
	// this and this. Zero bounds only by InboxSize frames.
	SendBufferBytes int64
	// HangDeadDials makes a dial to an address nobody listens at wait until its
	// caller gives up, as a dial to a machine that is gone waits out its SYN
	// retries, instead of being refused at once.
	HangDeadDials bool
	// HostOf names the host an address belongs to, which slow pairs and shared
	// bandwidth are counted per. Nil makes every address a host of its own.
	HostOf func(platform.Address) string
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
	// holdFrom and holdUntil are the interval this link holds what is sent
	// over it: a clog that delays rather than refuses, as a real partition
	// does. What is sent arrives once the hold ends.
	holdFrom  time.Time
	holdUntil time.Time
}

// pairKey is one direction between two hosts.
type pairKey struct {
	from, to string
}

// pairState is what one direction between two hosts carries for the whole run:
// its lasting extra latency, and when the bytes already sent over it will
// have been transmitted.
type pairState struct {
	slow time.Duration
	free time.Time
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
	pairs     map[pairKey]*pairState
	streams   streamListeners
	// tailEnded says EndTail ended the heavy tail.
	tailEnded bool
}

func newNetwork(runtime *Runtime, config NetworkConfig) *Network {
	return &Network{
		runtime:   runtime,
		config:    config,
		listeners: make(map[platform.Address]*listener),
		links:     make(map[linkKey]*linkState),
		dials:     make(map[linkKey]uint64),
		pairs:     make(map[pairKey]*pairState),
		streams:   make(streamListeners),
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
	if err := n.waitOutHold(ctx, key); err != nil {
		n.traceNetwork(key, "dial", "canceled", 0, dialID)
		return nil, err
	}
	n.mu.Lock()
	l := n.listeners[to]
	blocked := n.linkLocked(key).blocked(n.config, n.runtime.Now())
	n.mu.Unlock()
	if l == nil && n.config.HangDeadDials {
		// Nothing answers, and nothing says so: the dial waits for its caller.
		n.traceNetwork(key, "dial", "hanging", 0, dialID)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
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
	client := newConn(n, pipe, from, to, n.sendBuffer(key, dialID, "client"))
	server := newConn(n, pipe, to, from, n.sendBuffer(key, dialID, "server"))
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
	// Healing a link ends whatever is blocking it, a clog or a hold included:
	// a caller that has decided this link works again should not have to know
	// why it did not.
	state.clogFrom, state.clogUntil = time.Time{}, time.Time{}
	state.holdFrom, state.holdUntil = time.Time{}, time.Time{}
}

// Hold clogs the link from -> to the way a real partition does: what is sent
// over it is held rather than refused, and arrives once the hold ends at until,
// and a dial over it waits for the end as well. Nothing tells the sender
// anything, which is what makes the receiver's own check of whether bytes are
// arriving the thing under test. Clog, which refuses, stays as it was.
func (n *Network) Hold(from, to platform.Address, until time.Time) {
	n.holdBetween(from, to, n.runtime.Now(), until)
}

// HoldBoth holds both directions between two addresses until the same instant.
func (n *Network) HoldBoth(a, b platform.Address, until time.Time) {
	n.Hold(a, b, until)
	n.Hold(b, a, until)
}

// EndTail ends the heavy latency tail for the rest of the run: no hop after it
// takes longer for the tail. A campaign calls it when its faults stop, because
// the tail is one of them. A frame crosses a byte stream in pieces, and each
// piece can draw the tail, so one frame can take several TailLatency longer.
// That is a silence a liveness check is right to act on.
func (n *Network) EndTail() {
	n.mu.Lock()
	n.tailEnded = true
	n.mu.Unlock()
	n.runtime.trace.record(Event{Kind: "network", Operation: "end_tail", Outcome: "ok"})
}

func (n *Network) holdBetween(from, to platform.Address, start, until time.Time) {
	n.mu.Lock()
	state := n.linkLocked(linkKey{from: from, to: to})
	state.holdFrom, state.holdUntil = start, until
	n.mu.Unlock()
	n.runtime.trace.record(Event{Kind: "network", Resource: string(from) + "->" + string(to),
		Operation: "hold", Outcome: "ok", Bytes: int(until.Sub(start))})
}

// heldUntil is when a hold on the link ends, zero when none holds it now.
// Caller holds n.mu.
func (s *linkState) heldUntil(now time.Time) time.Time {
	if s.holdUntil.IsZero() || now.Before(s.holdFrom) || !now.Before(s.holdUntil) {
		return time.Time{}
	}
	return s.holdUntil
}

// waitOutHold waits for a hold on the link to end, as a dial over a silent link
// does.
func (n *Network) waitOutHold(ctx context.Context, key linkKey) error {
	n.mu.Lock()
	until := n.linkLocked(key).heldUntil(n.runtime.Now())
	n.mu.Unlock()
	if until.IsZero() {
		return nil
	}
	return n.runtime.sleep(ctx, until.Sub(n.runtime.Now()))
}

// hostOf names the host an address belongs to.
func (n *Network) hostOf(address platform.Address) string {
	if n.config.HostOf == nil {
		return string(address)
	}
	return n.config.HostOf(address)
}

// pairLocked is one direction between the hosts of a link, made the first time
// they talk. Its lasting extra latency is drawn then, from the seed and the
// two hosts alone, the same in both directions. Caller holds n.mu.
func (n *Network) pairLocked(key linkKey) *pairState {
	from, to := n.hostOf(key.from), n.hostOf(key.to)
	pair := pairKey{from: from, to: to}
	state := n.pairs[pair]
	if state != nil {
		return state
	}
	state = &pairState{}
	if n.config.SlowPairPerMille > 0 && n.config.SlowPairLatency > 0 {
		low, high := min(from, to), max(from, to)
		r := n.runtime.Random("network/slow-pair")
		if r.Intn(low+"\x00"+high+"/slow", 1000) < n.config.SlowPairPerMille {
			state.slow = r.Duration(low+"\x00"+high+"/latency", n.config.SlowPairLatency)
		}
	}
	n.pairs[pair] = state
	return state
}

// sendBuffer draws one direction of a connection's buffer, zero for none.
func (n *Network) sendBuffer(key linkKey, dialID uint64, side string) int64 {
	if n.config.SendBufferBytes <= 0 {
		return 0
	}
	half := max(n.config.SendBufferBytes/2, 1)
	id := fmt.Sprintf("%s/%s/%d/%s", key.from, key.to, dialID, side)
	return half + int64(n.runtime.Random("network/send-buffer").Uint64(id)%uint64(half))
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
	n.swizzle(addrs, window, r, n.clogBetween)
}

// SwizzleHolding is Swizzle with holds: every link among addrs holds what is
// sent over it for an interval of its own instead of refusing it, as FoundationDB's
// clogs do.
func (n *Network) SwizzleHolding(addrs []platform.Address, window time.Duration, r Random) {
	if window <= 0 || len(addrs) < 2 {
		return
	}
	n.swizzle(addrs, window, r, n.holdBetween)
}

func (n *Network) swizzle(addrs []platform.Address, window time.Duration, r Random,
	block func(from, to platform.Address, start, until time.Time)) {
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
			block(from, to, start, until)
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

func (n *Network) planSend(key linkKey, bytes int) sendPlan {
	n.mu.Lock()
	defer n.mu.Unlock()
	state := n.linkLocked(key)
	state.sequence++
	config := state.effective(n.config)
	now := n.runtime.Now()
	plan := sendPlan{sequence: state.sequence, blocked: state.blocked(n.config, now)}
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
	// What the faults of this run add to the hop: a hold the frame waits out,
	// a lasting slow pair, a rare long hop, and the bytes ahead of it on the
	// link between the two hosts.
	extra := n.extraLocked(key, state, plan.sequence, bytes, now)
	plan.minimum += extra
	plan.maximum += extra
	plan.delay += extra
	return plan
}

// extraLocked is what this run's faults add to one hop. Caller holds n.mu.
func (n *Network) extraLocked(key linkKey, state *linkState, sequence uint64, bytes int, now time.Time) time.Duration {
	var extra time.Duration
	if until := state.heldUntil(now); !until.IsZero() {
		extra += until.Sub(now)
	}
	if n.config.TailEvery > 0 && n.config.TailLatency > 0 && !n.tailEnded {
		r := n.runtime.Random("network/tail")
		id := fmt.Sprintf("%s/%s/%d", key.from, key.to, sequence)
		if r.Intn(id, n.config.TailEvery) == 0 {
			extra += r.Duration(id+"/latency", n.config.TailLatency)
		}
	}
	if n.config.SlowPairPerMille == 0 && n.config.LinkBytesPerSecond == 0 {
		return extra
	}
	pair := n.pairLocked(key)
	extra += pair.slow
	if n.config.LinkBytesPerSecond > 0 {
		// The frame is transmitted once the bytes ahead of it on this pair
		// have been, and takes its own share of the link after them.
		start := now.Add(extra)
		if pair.free.After(start) {
			start = pair.free
		}
		transmit := time.Duration(int64(bytes) * int64(time.Second) / n.config.LinkBytesPerSecond)
		pair.free = start.Add(transmit)
		extra = pair.free.Sub(now)
	}
	return extra
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
	// buffer bounds the bytes this end's inbox holds that it has not received,
	// zero for no bound beyond InboxSize frames; queued is what it holds, and
	// room is closed and replaced whenever a receive makes room.
	buffer int64
	bufMu  sync.Mutex
	queued int64
	room   chan struct{}
}

func newConn(network *Network, pipe *connectionPipe, local, remote platform.Address, buffer int64) *conn {
	c := &conn{
		network: network,
		pipe:    pipe,
		local:   local,
		remote:  remote,
		inbox:   make(chan receivedFrameData, network.config.InboxSize),
		send:    make(chan struct{}, 1),
		buffer:  buffer,
		room:    make(chan struct{}),
	}
	c.send <- struct{}{}
	return c
}

// reserve waits for room for size bytes in this end's inbox, which is the
// sender's buffer filling: a frame larger than the whole buffer goes when the
// inbox is empty.
func (c *conn) reserve(ctx context.Context, size int64) error {
	if c.buffer <= 0 {
		return nil
	}
	for {
		c.bufMu.Lock()
		if c.queued == 0 || c.queued+size <= c.buffer {
			c.queued += size
			c.bufMu.Unlock()
			return nil
		}
		room := c.room
		c.bufMu.Unlock()
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-c.pipe.done:
			return platform.ErrDisconnected
		case <-room:
		}
	}
}

// taken gives back what a received frame held of this end's buffer.
func (c *conn) taken(size int64) {
	if c.buffer <= 0 {
		return
	}
	c.bufMu.Lock()
	c.queued -= size
	close(c.room)
	c.room = make(chan struct{})
	c.bufMu.Unlock()
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
	c.network.runtime.shake.yield()
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
	plan := c.network.planSend(key, totalBytes)
	if plan.blocked {
		// A frame sent into a cut link is still a frame's time on the wire
		// before its sender learns it went nowhere. Failing at the instant it
		// was sent would race whatever arrives at the other end at that
		// instant, and the Go scheduler would decide which came first.
		if err := c.network.runtime.delay(ctx, fmt.Sprintf("network/send/%q/%q/%d", key.from, key.to, plan.sequence),
			plan.minimum, plan.maximum, plan.delay); err != nil {
			c.network.traceNetwork(key, "send", "canceled", totalBytes, plan.sequence)
			return err
		}
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
	bytesPerSecond := c.network.config.BytesPerSecond
	if c.network.config.LinkBytesPerSecond > 0 {
		// The link between the two hosts already charged this frame its share.
		bytesPerSecond = 0
	}
	if err := c.network.runtime.delay(ctx, fmt.Sprintf("network/send/%q/%q/%d", key.from, key.to, plan.sequence),
		operationLatency(plan.minimum, totalBytes, bytesPerSecond),
		operationLatency(plan.maximum, totalBytes, bytesPerSecond),
		operationLatency(plan.delay, totalBytes, bytesPerSecond)); err != nil {
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
	if c.network.runtime.buggifyHere(SiteRandomClose, 0.01) {
		// A connection closes under a frame, as FoundationDB's do at random:
		// half the time once the frame has arrived, half the time before.
		delivered := c.network.runtime.Random("network/random-close").Chance(
			fmt.Sprintf("%s/%s/%d", key.from, key.to, plan.sequence), 0.5)
		c.network.traceNetwork(key, "send", "closed", totalBytes, plan.sequence)
		if delivered {
			defer c.Close()
		} else {
			_ = c.Close()
			return platform.ErrDisconnected
		}
	}
	header := append([]byte(nil), frame.Header...)
	if len(header) > 0 && c.network.runtime.buggifyHere(SiteHeaderBitFlip, 0.01) {
		// One bit of the header flipped on the way, which only a checksum of
		// the header catches.
		bit := c.network.runtime.Random("network/header-bit-flip").Intn(
			fmt.Sprintf("%s/%s/%d", key.from, key.to, plan.sequence), len(header)*8)
		header[bit/8] ^= 1 << (bit % 8)
		c.network.traceNetwork(key, "send", "header_flipped", totalBytes, plan.sequence)
	}
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
		if err := c.peer.reserve(ctx, int64(totalBytes)); err != nil {
			c.network.traceNetwork(key, "send", connectionOutcome(err), totalBytes, plan.sequence)
			return err
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
		c.taken(int64(len(frame.header) + len(frame.payload)))
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
