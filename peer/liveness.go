package peer

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Liveness is how a host decides that a peer is there, and how long it waits
// to learn that one is not. It comes from the connections themselves, never
// from how long a request took: a request that is merely slow says the peer's
// disk or link is slow, which is the caller's to weigh, and a request its
// caller gave up on says nothing about the peer at all.
//
// A connection that has heard nothing for PingInterval is pinged, and the
// server answers a ping as it reads it, behind no request. A connection that
// has heard no byte at all for DeadAfter is dead: it is closed, and every
// request on it ends at once. Bytes of a large reply still arriving count, so a
// connection busy with a slow transfer is not taken for a dead one.
//
// A peer is marked down only by a hard failure: a dial or a hello that fails,
// or a connection found dead. While it is down, a caller that can do without it
// skips it (Down), and the table probes it back, first after about ProbeFirst
// and then at intervals growing by half up to ProbeMax. A dial that succeeds,
// a probe's or any caller's, marks it up again.
const (
	defaultPingInterval   = time.Second
	defaultDeadAfter      = 4 * time.Second
	defaultConnectTimeout = 3 * time.Second
	defaultIdleTimeout    = 30 * time.Second
	defaultProbeFirst     = time.Second
	defaultProbeMax       = 10 * time.Second
	// serverSilence is how long a server keeps a connection of protocol 2 or
	// later that sends it nothing. Its dialer pings it every PingInterval it
	// hears nothing, so one silent this long has gone.
	serverSilence = 30 * time.Second
)

var (
	// ErrDown reports a peer marked down, which a request that can do without
	// it was not sent to.
	ErrDown = errors.New("peer: the peer is marked down")
	// errDead reports a connection that heard nothing for DeadAfter.
	errDead = fmt.Errorf("%w: the connection heard nothing for too long", platform.ErrUnavailable)
	// errConnectTimeout reports a dial and hello that did not finish in time.
	errConnectTimeout = fmt.Errorf("%w: no connection and hello in time", platform.ErrUnavailable)
	// errIdle reports a connection closed for carrying nothing.
	errIdle = fmt.Errorf("%w: the connection carried nothing for too long", ErrClosed)
)

// markDown marks the peer down after a hard failure, and starts probing it
// back. A peer already down stays down; its probe goes on as it was.
func (p *Peer) markDown(ctx context.Context, cause error) {
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		return
	}
	p.down, p.downCause = true, cause
	p.mu.Unlock()
	sim.Probe(ctx, ProbeMarkedDown)
	table := p.table
	table.mu.Lock()
	if !table.closed {
		table.wg.Go(p.probe)
	}
	table.mu.Unlock()
}

// markUp is a dial that reached the peer and had its hello answered.
func (p *Peer) markUp() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		p.down, p.downCause = false, nil
		close(p.up)
		p.up = make(chan struct{})
	}
	p.incompatible = nil
}

// markIncompatible records a peer of a release this host shares no version
// with. It is not down: asking it again changes nothing, and neither does
// waiting.
func (p *Peer) markIncompatible(err *IncompatibleError) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.incompatible = err
}

// Down reports a peer marked down. A request that can do without the peer — a
// page a checkpoint holds, a stripe another holder has — skips it. One that
// cannot, a page only that peer holds, asks anyway.
func (p *Peer) Down() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.down
}

// probe dials a down peer until a dial and hello succeed, waiting about
// ProbeFirst before the first and half as long again before each next, up to
// ProbeMax. The wait is spread by a hash of the peer and the attempt, so the
// hosts probing one peer do not all arrive at once.
func (p *Peer) probe() {
	table := p.table
	wait := table.config.ProbeFirst
	ctx := WithClass(table.ctx, Fault)
	for attempt := 1; ; attempt++ {
		p.mu.Lock()
		up := p.up
		down := p.down
		p.mu.Unlock()
		if !down {
			return
		}
		timer := table.clock.NewTimer(spread(p.address, attempt, wait))
		select {
		case <-table.ctx.Done():
			timer.Stop()
			return
		case <-up:
			timer.Stop()
			return
		case <-timer.C():
		}
		sim.Probe(ctx, ProbeProbed)
		opened, err := table.connect(ctx, p.address, Fault)
		if err == nil {
			_ = opened.Close()
			p.markUp()
			return
		}
		var incompatible *IncompatibleError
		if errors.As(err, &incompatible) {
			p.markIncompatible(incompatible)
		}
		wait = min(wait*3/2, table.config.ProbeMax)
	}
}

// spread is wait moved by up to a tenth either way, by a hash of the peer and
// the attempt.
func spread(address platform.Address, attempt int, wait time.Duration) time.Duration {
	hash := fnv.New64a()
	_, _ = fmt.Fprintf(hash, "%s/%d", address, attempt)
	tenth := int64(wait / 10)
	if tenth <= 0 {
		return wait
	}
	return wait - time.Duration(tenth) + time.Duration(int64(hash.Sum64()%uint64(2*tenth+1)))
}

// connect dials the peer and says hello, for no longer than ConnectTimeout. A
// failure the caller did not cause is a hard failure: it marks the peer down,
// or incompatible.
func (t *Table) connect(ctx context.Context, address platform.Address, class Class) (*dialed, error) {
	dialCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	over := t.clock.AfterFunc(t.config.ConnectTimeout, func() { cancel(errConnectTimeout) })
	defer over.Stop()
	opened, err := dialPeer(dialCtx, t.config.Dial, address, t.config.Versions, class)
	if err != nil && ctx.Err() == nil {
		if cause := context.Cause(dialCtx); cause != nil {
			err = cause
		}
	}
	return opened, err
}

// heard records that bytes arrived on the connection.
func (c *conn) heard() {
	c.mu.Lock()
	c.lastHeard = c.pool.peer.table.clock.Now()
	c.mu.Unlock()
}

// silence is how long the connection has heard nothing, and how long it has
// carried no request.
func (c *conn) silence() (heard, used time.Duration, pinging bool) {
	clock := c.pool.peer.table.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	used = clock.Since(c.lastUsed)
	for _, waiting := range c.pending {
		// A ping is not use: a connection that carries only pings is idle.
		if !waiting.ping {
			used = 0
		}
	}
	return clock.Since(c.lastHeard), used, c.pinging
}

// monitor watches one connection of protocol 2 or later: it pings a quiet
// connection, closes a dead one and marks its peer down, and closes one that
// has carried nothing for IdleTimeout.
func (c *conn) monitor() {
	table := c.pool.peer.table
	ticker := table.clock.NewTicker(table.config.PingInterval)
	defer ticker.Stop()
	ctx := table.ctx
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C():
		}
		heard, used, pinging := c.silence()
		switch {
		case heard >= table.config.DeadAfter:
			sim.Probe(ctx, ProbeDeadConnection)
			c.fail(errDead)
			c.pool.peer.markDown(ctx, errDead)
			return
		case used >= table.config.IdleTimeout:
			c.fail(errIdle)
			return
		case heard >= table.config.PingInterval && !pinging:
			c.mu.Lock()
			c.pinging = true
			c.mu.Unlock()
			table.wg.Go(c.ping)
		}
	}
}

// ping sends one ping and waits for its pong. What the pong shows is that
// bytes arrived, which the reader records; a ping that is never answered is
// what the monitor finds dead.
func (c *conn) ping() {
	defer func() {
		c.mu.Lock()
		c.pinging = false
		c.mu.Unlock()
	}()
	waiting := &call{ping: true, reply: make(chan result, 1)}
	select {
	case <-c.done:
		return
	case <-c.send:
	}
	c.next++
	id := c.next
	c.mu.Lock()
	if c.failed != nil {
		c.mu.Unlock()
		c.send <- struct{}{}
		return
	}
	c.pending[id] = waiting
	c.mu.Unlock()
	frame, err := wire.Encode(wire.Outgoing{Version: c.version, RequestID: id, Message: &peerv1.Ping{}})
	if err == nil {
		err = c.Send(c.pool.peer.table.ctx, frame)
	}
	c.send <- struct{}{}
	if err != nil {
		c.forget(id)
		c.fail(err)
		return
	}
	select {
	case got := <-waiting.reply:
		got.payload.release()
	case <-c.done:
	}
}

// progress is a payload reader that records every read that brought bytes, so
// a connection receiving a large reply slowly is heard the whole time.
type progress struct {
	reader interface{ Read([]byte) (int, error) }
	conn   *conn
}

func (p progress) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	if n > 0 {
		p.conn.heard()
	}
	return n, err
}
