package peer

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
)

// defaultInFlight is how many requests one connection may carry at once. A
// peer is told it in the hello, and asks no more.
const defaultInFlight = 16

// ServerConfig supplies the network one host serves its peers on. Every peer
// the listener accepts is served: refusing a peer that is not a host is the
// transport's job, or over plain TCP the network policy's. One peer is one
// remote host, which is what the budgets below are counted per.
type ServerConfig struct {
	// Network opens the listener at Address. A caller that has already opened
	// one supplies it as Listener instead.
	Network  platform.Network
	Listener platform.Listener
	Address  platform.Address
	// PageSize is the largest page this server serves, which every budget must
	// hold at least one of. What a reply is counted in is the page of the
	// volume it answers for — a host's two kinds of memory region need not
	// agree. Zero selects the largest page a volume may be published in.
	PageSize int
	// MaxPagesPerRequest caps one reply whatever volume it answers for. Zero
	// takes the cap from the volume's own page instead, so that a reply of small
	// pages carries as many of them as one of a large page carries bytes.
	MaxPagesPerRequest int
	// Budgets is what one remote host's requests of each class may hold here
	// at once, over all of that host's connections. A request over it is
	// answered BUSY, never by closing a connection. Zero takes DefaultBudgets.
	Budgets Budgets
	// MaxInFlight is how many requests one connection may carry at once. Zero
	// takes 16.
	MaxInFlight int
	// Versions is the range of protocol versions this server speaks. Zero is
	// this release's; a test that stands in for another release narrows it.
	Versions Versions
	// Cache is this host's disk cache, which answers the cache's requests. Nil
	// is a host that keeps none: every cache request is answered not me.
	Cache Cache
	// StripeBytesPerSecond is this host's serving bandwidth for stripes: the
	// bytes of stripe replies it sends all its peers each second, with a burst
	// of a tenth of a second of it. A read that finds it spent is answered
	// BUSY, and its reader asks another holder. The tail of reads from the
	// cluster follows the bytes each host serves well before a link's rate
	// (docs/measurements/gce-stripes-tail-2026-10-03.md), so a deployment
	// keeps it under about 40 % of its NIC's. Zero leaves it unbounded.
	StripeBytesPerSecond int64
	// Clock measures the serving bandwidth. Nil is the wall clock.
	Clock platform.Clock
}

// ServerStats reports what this host has served.
type ServerStats struct {
	// Requests is every page request answered, Served and Absent the pages they
	// found and did not find.
	Requests, Served, Absent int64
	// Refused counts the requests answered BUSY because their peer's class was
	// at its budget, which a destination answers by waiting or by reading its
	// own volume. No connection is ever refused.
	Refused int64
	// Listings is every resident listing answered.
	Listings int64
	// Incompatible counts the hellos answered INCOMPATIBLE: peers of a release
	// that shares no protocol version with this one.
	Incompatible int64
	// StripeReads is every read of the cache's stripes answered with them,
	// Stripes and StripeBytes the stripes and bytes those replies carried, and
	// StripesBusy the reads answered BUSY because the serving bandwidth was
	// spent.
	StripeReads, Stripes, StripeBytes, StripesBusy int64
}

// Server is a host's peer server. It answers every other host: the pages of the
// VMs this host holds memory for on another host's behalf — the ones it has
// migrated away, and the children it has forked onto another host — and,
// through a Cache, the stripes of its disk cache. A VM registers its pages when
// it is handed over and gives them up when the destination reports that it has
// them all.
type Server struct {
	config   ServerConfig
	listener platform.Listener
	ctx      context.Context
	cancel   context.CancelCauseFunc
	wg       sync.WaitGroup

	handoffs handoffs
	budgets  serverBudgets
	// serving is the bandwidth stripe replies may take.
	serving *servingBudget

	requests, servedPages, absentPages, refused, listings, incompatible atomic.Int64
	stripeReads, stripes, stripeBytes, stripesBusy                      atomic.Int64
	closeOnce                                                           sync.Once
	closeErr                                                            error
}

// NewServer starts serving on address until Close. It serves nothing until a
// migration registers a VM's memory regions.
func NewServer(ctx context.Context, config ServerConfig) (*Server, error) {
	if (config.Network == nil && config.Listener == nil) || config.Address == "" {
		return nil, fmt.Errorf("%w: a peer server needs a listener or a network, and an address", ErrInvalid)
	}
	if config.PageSize == 0 {
		config.PageSize = MaxPageSize
	}
	if config.MaxInFlight == 0 {
		config.MaxInFlight = defaultInFlight
	}
	config.Budgets = config.Budgets.orDefault()
	config.Versions = config.Versions.orDefault()
	if !config.Versions.valid() {
		return nil, fmt.Errorf("%w: protocol versions %d to %d", ErrInvalid, config.Versions.Min, config.Versions.Max)
	}
	if config.StripeBytesPerSecond < 0 || config.PageSize < 512 || config.PageSize > blob.MaxSize || config.MaxPagesPerRequest < 0 ||
		config.MaxPagesPerRequest > blob.MaxSize/config.PageSize || config.MaxInFlight < 1 ||
		!config.Budgets.valid(int64(config.PageSize)) {
		return nil, fmt.Errorf("%w: invalid peer server budgets", ErrInvalid)
	}
	listener := config.Listener
	if listener == nil {
		opened, err := config.Network.Listen(config.Address)
		if err != nil {
			return nil, err
		}
		listener = opened
	}
	serverCtx, cancel := context.WithCancelCause(ctx)
	s := &Server{config: config, listener: listener, ctx: serverCtx, cancel: cancel,
		handoffs: newHandoffs(), budgets: serverBudgets{held: make(map[budgetKey]int64)},
		serving: newServingBudget(platform.ClockOr(config.Clock), config.StripeBytesPerSecond)}
	s.wg.Go(s.accept)
	return s, nil
}

// Address is where peers reach this peer server.
func (s *Server) Address() platform.Address { return s.config.Address }

// PageSize is the largest page this server serves, which is what its budgets
// are sized against. A reply is counted in the page of the volume it answers
// for, which both hosts read out of that volume's durable geometry.
func (s *Server) PageSize() int { return s.config.PageSize }

func (s *Server) Stats() ServerStats {
	return ServerStats{Requests: s.requests.Load(), Served: s.servedPages.Load(),
		Absent: s.absentPages.Load(), Refused: s.refused.Load(), Listings: s.listings.Load(),
		Incompatible: s.incompatible.Load(), StripeReads: s.stripeReads.Load(), Stripes: s.stripes.Load(),
		StripeBytes: s.stripeBytes.Load(), StripesBusy: s.stripesBusy.Load()}
}

// Close stops accepting and drops every connection. It does not release the
// memory regions: their pages belong to the VMM process that owns them.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.cancel(ErrClosed)
		s.closeErr = s.listener.Close()
		s.wg.Wait()
	})
	return s.closeErr
}

func (s *Server) accept() {
	for {
		conn, err := s.listener.Accept(s.ctx)
		if err != nil {
			if context.Cause(s.ctx) == nil {
				slog.WarnContext(s.ctx, "peer: the server stopped accepting", "address", s.config.Address, "error", err)
			}
			return
		}
		s.wg.Go(func() { s.serveConn(conn) })
	}
}

// peerKey is the identity a budget is counted against: the host a connection
// came from, without the ephemeral port it happens to have been given. A peer
// opens several connections and dials again whenever one breaks, so counting
// connections by their full address counts each of them as a peer of its own:
// no budget ever binds, and the table of budgets grows with every reconnect for
// as long as this host serves. A simulated address is a logical name with no
// port and is its own key.
func peerKey(address platform.Address) string {
	host, _, err := net.SplitHostPort(string(address))
	if err != nil {
		return string(address)
	}
	return host
}

// budgetKey is one remote host's class.
type budgetKey struct {
	peer  string
	class Class
}

// serverBudgets is what each remote host's requests of each class hold here.
type serverBudgets struct {
	mu   sync.Mutex
	held map[budgetKey]int64
}

// reserve takes bytes from a peer's class, and reports what it holds when it
// cannot. A request larger than the whole budget is admitted alone, so a budget
// never refuses a request for ever.
func (b *serverBudgets) reserve(key budgetKey, bytes, budget int64) (held int64, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	held = b.held[key]
	if held+bytes > budget && held > 0 {
		return held, false
	}
	b.held[key] = held + bytes
	return held, true
}

func (b *serverBudgets) release(key budgetKey, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held[key] -= bytes
	if b.held[key] <= 0 {
		delete(b.held, key)
	}
}

// servingBudget is a host's serving bandwidth for stripes: a token bucket of
// bytes a second, with a burst of a tenth of a second of it. A reply is
// admitted while the bucket is not in debt, and its bytes are taken once the
// cache has said how many it holds, so a reply larger than what is left
// leaves the bucket in debt rather than being refused for ever.
type servingBudget struct {
	clock platform.Clock
	rate  float64
	burst float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// newServingBudget is a budget of bytesPerSecond, nil for none.
func newServingBudget(clock platform.Clock, bytesPerSecond int64) *servingBudget {
	if bytesPerSecond <= 0 {
		return nil
	}
	rate := float64(bytesPerSecond)
	return &servingBudget{clock: clock, rate: rate, burst: rate / 10, tokens: rate / 10, last: clock.Now()}
}

// refill adds what the time since the last refill earned. Caller holds b.mu.
func (b *servingBudget) refill() {
	now := b.clock.Now()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+b.rate*elapsed.Seconds())
		b.last = now
	}
}

// admit reports whether a reply may be sent now, and what the bucket holds.
func (b *servingBudget) admit() (bool, int64) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	return b.tokens > 0, int64(b.tokens)
}

// spend takes the bytes of a reply admitted.
func (b *servingBudget) spend(bytes int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	b.tokens -= float64(bytes)
}
