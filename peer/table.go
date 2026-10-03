package peer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/platform"
)

// Dialer opens one connection to another host's peer server. A production
// dialer is the host network's, over whatever transport the deployment runs.
type Dialer func(ctx context.Context, peer platform.Address) (platform.Conn, error)

// Connections is how many connections each class may hold to one peer.
type Connections struct {
	Fault, BulkRead, BulkWrite int
}

// DefaultConnections keeps two connections for faults, so one that waits on a
// slow request leaves another; two for bulk reads, which keep a link busy
// between them; and one for bulk writes, which are dropped rather than queued.
var DefaultConnections = Connections{Fault: 2, BulkRead: 2, BulkWrite: 1}

// Of is the connections of class.
func (c Connections) Of(class Class) int {
	switch class {
	case BulkRead:
		return c.BulkRead
	case BulkWrite:
		return c.BulkWrite
	default:
		return c.Fault
	}
}

// defaultClientInFlight is how many requests this end puts on one connection
// before a server's hello says how many it takes.
const defaultClientInFlight = 8

// TableConfig is how a host reaches its peers.
type TableConfig struct {
	// Dial opens one connection to a peer's server.
	Dial Dialer
	// Clock times the requests. Nil is the wall clock.
	Clock platform.Clock
	// Versions is the range of protocol versions this end speaks. Zero is this
	// release's; a test that stands in for another release narrows it.
	Versions Versions
	// Connections bounds each class's connections to one peer. Zero takes
	// DefaultConnections.
	Connections Connections
	// InFlight bounds the requests on one connection, and a server's hello can
	// lower it. Zero takes 8.
	InFlight int
	// Budgets is what this end lets each class of its requests to one peer
	// hold at that peer before the peer's hello says what it may. Zero takes
	// DefaultBudgets.
	Budgets Budgets
}

// Table is a host's peers: one Peer for every remote host, shared by every
// kind of request this host makes of it — a migration's faults and its stream,
// a fork child's claim, and the cluster cache's reads and fills. So every
// request to one host shares its connections, its budget and what is known of
// whether it is up, rather than each memory region learning that alone.
type Table struct {
	config TableConfig
	clock  platform.Clock
	// ctx is the table's life: every connection's reader runs under it, and
	// Close ends it.
	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	peers  map[platform.Address]*Peer
	closed bool
}

// NewTable starts a host's table of peers. It dials nothing until a request is
// made. ctx is what its connections live under; Close ends them.
func NewTable(ctx context.Context, config TableConfig) (*Table, error) {
	if config.Dial == nil {
		return nil, fmt.Errorf("%w: a table of peers needs a dialer", ErrInvalid)
	}
	config.Versions = config.Versions.orDefault()
	config.Budgets = config.Budgets.orDefault()
	if config.Connections == (Connections{}) {
		config.Connections = DefaultConnections
	}
	if config.InFlight == 0 {
		config.InFlight = defaultClientInFlight
	}
	if !config.Versions.valid() || config.InFlight < 1 || !config.Budgets.valid(1) ||
		config.Connections.Fault < 1 || config.Connections.BulkRead < 1 || config.Connections.BulkWrite < 1 {
		return nil, fmt.Errorf("%w: invalid table of peers", ErrInvalid)
	}
	tableCtx, cancel := context.WithCancelCause(ctx)
	return &Table{config: config, clock: platform.ClockOr(config.Clock), ctx: tableCtx, cancel: cancel,
		peers: make(map[platform.Address]*Peer)}, nil
}

// Peer is the host at address, made on first use.
func (t *Table) Peer(address platform.Address) *Peer {
	t.mu.Lock()
	defer t.mu.Unlock()
	if peer := t.peers[address]; peer != nil {
		return peer
	}
	peer := &Peer{table: t, address: address}
	for class := range classes {
		peer.pools[class] = &pool{peer: peer, class: class, limit: t.config.Connections.Of(class),
			budget: t.config.Budgets.Of(class), changed: make(chan struct{})}
	}
	t.peers[address] = peer
	return peer
}

// Close drops every connection to every peer. A request in flight ends with
// ErrClosed, and so does every later one.
func (t *Table) Close() error {
	t.mu.Lock()
	t.closed = true
	peers := slices.Collect(maps.Values(t.peers))
	t.mu.Unlock()
	t.cancel(ErrClosed)
	for _, peer := range peers {
		for _, pool := range peer.pools {
			pool.close()
		}
	}
	t.wg.Wait()
	return nil
}

// PeerStatus is what a table knows of one peer.
type PeerStatus struct {
	Address platform.Address
	// Version is the protocol version the peer's connections last settled
	// on, zero before any has.
	Version uint32
	// Connections is how many connections each class holds to it now.
	Connections Connections
}

// Status reports every peer this table has asked anything of, in address order.
func (t *Table) Status() []PeerStatus {
	t.mu.Lock()
	peers := slices.SortedFunc(maps.Values(t.peers), func(a, b *Peer) int {
		return compareAddresses(a.address, b.address)
	})
	t.mu.Unlock()
	statuses := make([]PeerStatus, 0, len(peers))
	for _, peer := range peers {
		statuses = append(statuses, peer.status())
	}
	return statuses
}

func compareAddresses(a, b platform.Address) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Peer is one remote host as this host sees it: a small pool of connections for
// each class of request, and what each class may hold there.
type Peer struct {
	table   *Table
	address platform.Address
	pools   [classes]*pool
}

// Address is where the peer's server listens.
func (p *Peer) Address() platform.Address { return p.address }

func (p *Peer) status() PeerStatus {
	status := PeerStatus{Address: p.address}
	for _, pool := range p.pools {
		pool.mu.Lock()
		count := len(pool.conns)
		if pool.version > status.Version {
			status.Version = pool.version
		}
		pool.mu.Unlock()
		switch pool.class {
		case Fault:
			status.Connections.Fault = count
		case BulkRead:
			status.Connections.BulkRead = count
		case BulkWrite:
			status.Connections.BulkWrite = count
		}
	}
	return status
}

// pool is one class's connections to one peer, and the bytes its requests hold
// there. A request takes room in the budget and a slot on a connection before
// it is sent, and gives both back when its reply arrives, which is when the
// server gives its own back.
type pool struct {
	peer  *Peer
	class Class
	// limit is how many connections this class may hold.
	limit int

	mu      sync.Mutex
	conns   []*conn
	dialing int
	// held is the bytes this class's requests in flight hold at the server, and
	// budget what they may: the table's own, until a hello says the server's.
	held, budget int64
	// version is what the last connection settled on, and heard says a hello
	// has said what this class may hold at the peer.
	version uint32
	heard   bool
	closed  bool
	// changed is closed and replaced whenever room is given back, which is
	// what a request waiting for room waits on.
	changed chan struct{}
}

// acquire waits for room for a request of bytes and a slot on a connection, and
// dials one if every connection is full and the class may hold another.
func (p *pool) acquire(ctx context.Context, bytes int64) (*conn, error) {
	overBudget := false
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		// A class asks nothing more of a peer whose budget it has not heard
		// than the one request whose dial will hear it: the others would be
		// counted against a budget the peer has not given.
		heard := p.heard || p.dialing == 0
		// A request larger than the whole budget is sent alone, so a budget
		// never stops a request for ever.
		if heard && (p.held+bytes <= p.budget || p.held == 0) {
			if c := p.leastLoaded(); c != nil {
				c.inflight++
				p.held += bytes
				p.mu.Unlock()
				return c, nil
			}
			if len(p.conns)+p.dialing < p.limit {
				p.dialing++
				p.held += bytes
				p.mu.Unlock()
				c, err := p.dial(ctx)
				p.mu.Lock()
				p.dialing--
				if err != nil {
					p.held -= bytes
					p.signal()
					p.mu.Unlock()
					return nil, err
				}
				c.inflight++
				p.conns = append(p.conns, c)
				p.signal()
				p.mu.Unlock()
				return c, nil
			}
		} else if heard && !overBudget {
			overBudget = true
			probeWaitedForBudget(ctx)
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-changed:
		}
	}
}

// leastLoaded is the open connection with the fewest requests in flight and
// room for another, the earliest dialed of equals. Caller holds p.mu.
func (p *pool) leastLoaded() *conn {
	var best *conn
	for _, c := range p.conns {
		if c.inflight < c.maxInFlight && (best == nil || c.inflight < best.inflight) {
			best = c
		}
	}
	return best
}

// release gives back what one request held, once its reply has arrived or its
// connection has gone.
func (p *pool) release(c *conn, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c.inflight--
	p.held -= bytes
	p.signal()
}

// remove takes a connection that has failed out of the pool.
func (p *pool) remove(c *conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns = slices.DeleteFunc(p.conns, func(other *conn) bool { return other == c })
	p.signal()
}

// signal wakes everything waiting for room. Caller holds p.mu.
func (p *pool) signal() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// dial opens one connection of this class and settles what it may carry.
func (p *pool) dial(ctx context.Context) (*conn, error) {
	table := p.peer.table
	opened, err := dialPeer(ctx, table.config.Dial, p.peer.address, table.config.Versions, p.class)
	if err != nil {
		return nil, err
	}
	maxInFlight := table.config.InFlight
	if opened.version < 2 {
		// The release before answers one request at a time on a connection.
		maxInFlight = 1
	} else if opened.maxInFlight > 0 {
		maxInFlight = min(maxInFlight, opened.maxInFlight)
	}
	p.mu.Lock()
	if opened.budget > 0 {
		p.budget = min(table.config.Budgets.Of(p.class), opened.budget)
	}
	p.version, p.heard = opened.version, true
	p.mu.Unlock()
	c := newConn(p, opened.Conn, opened.version, maxInFlight)
	table.mu.Lock()
	closed := table.closed
	if !closed {
		table.wg.Go(c.read)
	}
	table.mu.Unlock()
	if closed {
		_ = opened.Close()
		return nil, ErrClosed
	}
	return c, nil
}

func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	conns := slices.Clone(p.conns)
	p.signal()
	p.mu.Unlock()
	for _, c := range conns {
		c.fail(ErrClosed)
	}
}

// dialed is a connection whose hello has been answered: the version it speaks
// and, from version 2 on, what the server lets it carry.
type dialed struct {
	platform.Conn
	version     uint32
	budget      int64
	maxInFlight int
}

// dialPeer opens a connection and settles its version. A dialer that speaks
// version 2 says hello. A server that closes the connection in answer is one of
// the release before this one, which speaks only version 1 and cannot read a
// hello, so a dialer that still speaks version 1 dials again without one.
func dialPeer(ctx context.Context, dial Dialer, address platform.Address, speaks Versions, class Class) (*dialed, error) {
	conn, err := dial(ctx, address)
	if err != nil {
		return nil, err
	}
	if speaks.Max < 2 {
		return &dialed{Conn: conn, version: 1}, nil
	}
	answer, err := sayHello(ctx, conn, speaks, class)
	if err == nil {
		return &dialed{Conn: conn, version: answer.GetVersion(), budget: int64(answer.GetBudgetBytes()),
			maxInFlight: int(answer.GetMaxInFlight())}, nil
	}
	_ = conn.Close()
	if !errors.Is(err, errNoHello) || speaks.Min > 1 {
		return nil, err
	}
	probeFellBack(ctx)
	conn, err = dial(ctx, address)
	if err != nil {
		return nil, err
	}
	return &dialed{Conn: conn, version: 1}, nil
}
