package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// Reading from the cluster. Inside the share the cluster cache is turned on
// for, a page this host holds in neither its memory tier nor its pager's arena
// is read from the disks the membership ranks for its window before the store
// (plans/disk-cache-2026-10-02.md, "Reading a page").
//
// A read is made under one generation of the membership, which every request
// it sends names. A holder ahead of it answers that it is stale; the read then
// reads the membership and reads the window again under the newer one, so no
// stripe is read under a membership the reader and its holder do not both
// hold. A disk no member serves has no route, and is passed over as a holder
// marked down is.
//
// A read takes the stripes of the window this host's own disk holds, which
// cost no request. If they do not make k distinct indices of each page, it
// asks k+1 of the window's first k+m ranks, less its own, for every stripe of
// the pages they hold, of any index: a join or a leave near the top of a
// window's ranks moves every holder below it, so a holder seldom holds the
// index its rank puts on it. Which ranks it asks first is chosen by a hash of
// the reader and the window (rank.Pick), so the readers of one window spread
// over all its holders. A holder that answers with nothing, BUSY, or not at
// all is replaced at once by the next rank not yet asked: that is a miss, not
// a hedge. If k stripes of every page have not arrived after a delay, about
// the 95th percentile of this reader's recent times to k stripes and no less
// than a floor, it asks the rest, paid from a budget as FoundationDB's load
// balancer pays for its second requests: a read that had its stripes within
// the delay adds a twentieth of a request, and a second request takes one. So
// when every holder is slow at once the budget runs out, and the reader waits
// rather than doubling every holder's load.
//
// Each stripe's key and checksum are checked as it arrives, and a page is
// rebuilt from any k distinct indices (stripe.Join) and checked as a page
// from the store is. A stripe that fails a check, or that Join finds is not
// the envelope's, is not used, and its holder is told to drop it. A read that
// waits past a bound, set from the reader's own latencies and a few
// milliseconds at least, reads the store as well and takes whichever answers
// first, within a token bucket of a twentieth of the reads that asked the
// cluster; past the bucket it waits for the stripes. A page fewer than k
// stripes of which exist anywhere it asked is read from the store, and that
// read fills the cluster behind it.
//
// A window is read under the code it was stored under. The read tries the
// list's code first. What it could not rebuild under it, it reads again under
// each code the deployment used before, newest first, from the window's ranks
// under that code, which are the first of its ranks under the widest. Each
// rebuild takes stripes of one code alone. A repair is only ever under the
// list's code: a window rebuilt under an earlier code is filled under the
// list's code instead, as a read of the store fills it. So a deliberate change
// of the code leaves every earlier window readable until it ages out. The
// store is read only for a page no code rebuilt.
//
// Nothing a read sends waits on the read: a request goes on until it is
// answered or times out, whether or not the read still needs it, and how it
// ended is what marks a host down (clusterdown.go). Drops and repairs are
// background work, done one at a time behind the fills (fill.go).

// Defaults of a cache's reads of the cluster.
const (
	// DefaultClusterHedgeFloor is the least a read waits for k stripes of a
	// window before it asks the rest of the window's ranks.
	DefaultClusterHedgeFloor = 500 * time.Microsecond
	// DefaultClusterBound is the least a read waits for stripes before it
	// reads the store as well.
	DefaultClusterBound = 10 * time.Millisecond
	// DefaultClusterStripeTimeout is how long one stripe request waits for
	// its answer before it is a timeout of the host it asked.
	DefaultClusterStripeTimeout = time.Second
	// DefaultHeadCheckEvery is how many hits of the disk tier go by between
	// two checks that the part behind a hit still exists.
	DefaultHeadCheckEvery = 10000
)

// staleRetries is how many times a read or a fill told it is stale reads the
// membership and tries again. Each time is under a newer generation, so this
// bounds only a membership that changes faster than a request crosses the
// network.
const staleRetries = 3

const (
	// hedgeWindow is how many recent reads the delay is drawn from, and
	// hedgeEvery how many pass between two updates of it.
	hedgeWindow = 256
	hedgeEvery  = 32
	// hedgeEarn: a read that had its stripes within the delay earns
	// 1/hedgeEarn of a second request; hedgeMax is the most the budget holds.
	hedgeEarn = 20
	hedgeMax  = 5
	// boundFactor is the bound in delays: a read that has waited this many
	// times its usual 95th percentile is in a tail the store may be quicker
	// than.
	boundFactor = 4
	// storeHedgeEarn: a read that asked the cluster earns 1/storeHedgeEarn
	// of a read of the store; storeHedgeMax is the most the bucket holds.
	storeHedgeEarn = 20
	storeHedgeMax  = 5
)

// ReadStats is what a cache's reads of the cluster did, in envelopes unless
// they say otherwise.
type ReadStats struct {
	// Hits counts the envelopes rebuilt from the cluster, OwnHits those
	// among them this host's own disk rebuilt alone, with no request, and
	// EarlierHits those rebuilt under a code the deployment used before its
	// own. Misses counts the envelopes the cluster could not rebuild, which
	// the store served.
	Hits, OwnHits, EarlierHits, Misses uint64
	// Requests counts the stripe requests sent, Replaced the holders replaced
	// at once for answering with nothing, BUSY or an error, SecondRequests
	// the reads that asked the rest of the ranks after the delay, and
	// Refused the reads whose second request the budget refused.
	Requests, Replaced, SecondRequests, Refused uint64
	// StoreHedges counts the reads past the bound that read the store as
	// well, StoreHedgesWon those the store answered first, and
	// StoreHedgesRefused those the token bucket refused.
	StoreHedges, StoreHedgesWon, StoreHedgesRefused uint64
	// WrongStripes counts the stripes found wrong, by their checks or by
	// rebuilding, and DropsSent the drops sent to their holders for them.
	WrongStripes, DropsSent uint64
	// Repairs counts the stripes handed over to repair a window: indices no
	// rank held, for ranks that held fewer than the code puts on them.
	Repairs uint64
	// Timeouts counts the stripe requests that timed out. MarkedDown counts
	// the hosts marked down, Capped the marks refused for the bound on how
	// many may be down, Cleared the marks a probe cleared, and Down the hosts
	// marked down now.
	Timeouts, MarkedDown, Capped, Cleared uint64
	Down                                  int
	// HeadChecks counts the hits whose part was checked with a HEAD, and
	// HeadMissing those whose part the store no longer had.
	HeadChecks, HeadMissing uint64
	// Delay and Bound are the reader's delay before a second request and its
	// bound before a read of the store, now.
	Delay, Bound time.Duration
	// Prefetches counts the reads of a window a prefetch made, which ask no
	// second request, never read the store as a hedge, and leave the delay
	// alone.
	Prefetches uint64
}

// The probes reads of the cluster mark.
const (
	// ProbeClusterHit is an envelope rebuilt from stripes some peer sent.
	ProbeClusterHit = "checkpoint/cluster-hit"
	// ProbeClusterOwnHit is an envelope this host's own stripes rebuilt.
	ProbeClusterOwnHit = "checkpoint/cluster-own-hit"
	// ProbeClusterMiss is an envelope the cluster could not rebuild.
	ProbeClusterMiss = "checkpoint/cluster-miss"
	// ProbeClusterEarlierCode is an envelope rebuilt under a code the
	// deployment used before its own.
	ProbeClusterEarlierCode = "checkpoint/cluster-earlier-code"
	// ProbeClusterParity is an envelope rebuilt with a data stripe missing.
	ProbeClusterParity = "checkpoint/cluster-rebuilt-from-parity"
	// ProbeClusterReplaced is a holder replaced at once for its answer.
	ProbeClusterReplaced = "checkpoint/cluster-holder-replaced"
	// ProbeClusterSecondRequest is a read that asked the rest after the
	// delay, and ProbeClusterRefused one the budget refused.
	ProbeClusterSecondRequest = "checkpoint/cluster-second-request"
	ProbeClusterRefused       = "checkpoint/cluster-second-request-refused"
	// ProbeClusterStoreHedge is a read past the bound that read the store as
	// well, ProbeClusterStoreHedgeWon one the store answered first, and
	// ProbeClusterStoreHedgeRefused one the token bucket refused.
	ProbeClusterStoreHedge        = "checkpoint/cluster-store-hedge"
	ProbeClusterStoreHedgeWon     = "checkpoint/cluster-store-hedge-won"
	ProbeClusterStoreHedgeRefused = "checkpoint/cluster-store-hedge-refused"
	// ProbeClusterWrongStripe is a stripe found wrong, and ProbeClusterDrop
	// a drop sent to its holder.
	ProbeClusterWrongStripe = "checkpoint/cluster-wrong-stripe-found"
	ProbeClusterDrop        = "checkpoint/cluster-drop-sent"
	// ProbeClusterRepair is a repair handed over.
	ProbeClusterRepair = "checkpoint/cluster-repair"
	// ProbeClusterTimeout is a stripe request that timed out.
	ProbeClusterTimeout = "checkpoint/cluster-timeout"
	// ProbeClusterMarkedDown is a host marked down, ProbeClusterCapped a mark
	// the bound refused, and ProbeClusterCleared a mark a probe cleared.
	ProbeClusterMarkedDown = "checkpoint/cluster-marked-down"
	ProbeClusterCapped     = "checkpoint/cluster-mark-capped"
	ProbeClusterCleared    = "checkpoint/cluster-mark-cleared"
	// ProbeClusterHeadCheck is a hit whose part was checked, and
	// ProbeClusterHeadMissing one whose part was gone.
	ProbeClusterHeadCheck   = "checkpoint/cluster-head-check"
	ProbeClusterHeadMissing = "checkpoint/cluster-head-missing"
)

// The fault-injection sites of reads of the cluster.
const (
	// buggifyClusterWrongStripe hands a read a stripe whose checksum held and
	// whose bytes are not the envelope's, as a peer that answers with a wrong
	// stripe does.
	buggifyClusterWrongStripe = "checkpoint/cluster-wrong-stripe"
	// buggifyClusterDamagedItem damages an item a peer sent, so it fails its
	// checksum.
	buggifyClusterDamagedItem = "checkpoint/cluster-damaged-item"
	// buggifyClusterLoseAnswer loses a holder's answer, as a reply that never
	// came does.
	buggifyClusterLoseAnswer = "checkpoint/cluster-lose-answer"
	// buggifyClusterStoreHedgeNow has a read reach its bound at once.
	buggifyClusterStoreHedgeNow = "checkpoint/cluster-store-hedge-now"
	// buggifyClusterFalseTimeout counts an answer as a timeout of its host.
	buggifyClusterFalseTimeout = "checkpoint/cluster-false-timeout"
)

var (
	// errStripeTimeout ends a stripe request that went unanswered too long.
	errStripeTimeout = fmt.Errorf("%w: a stripe request timed out", platform.ErrUnavailable)
	// errNoPeers answers a request of a cache that reaches no peer.
	errNoPeers = errors.New("checkpoint: the cache reaches no peer")
	// errAnswerLost is an answer the lose-answer site took.
	errAnswerLost = errors.New("checkpoint: the answer was lost")
	// errNoRoute answers a request of a disk no member serves.
	errNoRoute = errors.New("checkpoint: no member serves the disk")
)

// clusterSettings is how a cache reads the cluster.
type clusterSettings struct {
	hedgeFloor, bound, stripeTimeout time.Duration
	// probeFirst and probeMax are how a host marked down is probed back.
	probeFirst, probeMax time.Duration
	// headEvery is the hits between two HEAD checks, none when not positive.
	headEvery int
}

// clusterReader is one cache's reads of the cluster: its delay and budget for
// second requests, its bucket of reads of the store, the hosts it has marked
// down, and the requests it has in flight.
type clusterReader struct {
	disk     *cacheDisk
	filler   *filler
	peers    *peer.Table
	clock    platform.Clock
	settings clusterSettings

	// ctx is the reads' life: every request and every probe runs under it,
	// and close ends it.
	ctx    context.Context
	cancel context.CancelFunc
	group  sync.WaitGroup

	hedge hedger
	marks downMarks

	mu     sync.Mutex
	closed bool
	// lingering counts the reads that hear their last answers behind their
	// callers, and quiet is closed and replaced each time it falls to zero.
	lingering int
	quiet     chan struct{}
	stats     ReadStats
	// tokens is the store hedge's bucket, in twentieths of a read.
	tokens int
	// hits counts the disk tier's hits, which the HEAD check samples.
	hits uint64
}

// newClusterReader starts a cache's reads of the cluster under ctx. Its waits
// are on the clock of the table of peers, which is the network's; with no
// table, the wall clock's.
func newClusterReader(ctx context.Context, disk *cacheDisk, filler *filler, peers *peer.Table,
	settings clusterSettings) *clusterReader {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	clock := platform.ClockOr(nil)
	if peers != nil {
		clock = peers.Clock()
	}
	r := &clusterReader{disk: disk, filler: filler, peers: peers, clock: clock, settings: settings, ctx: ctx,
		cancel: cancel, tokens: storeHedgeMax * storeHedgeEarn,
		// Both budgets start full, as a bucket does: a reader with no history
		// may still hedge its first reads.
		hedge: hedger{floor: settings.hedgeFloor, wait: settings.hedgeFloor, budget: hedgeMax * hedgeEarn}}
	r.quiet = make(chan struct{})
	r.marks = downMarks{reader: r, marks: make(map[rank.Identity]*downMark)}
	filler.down = r.marks.isDown
	return r
}

// close ends every request and probe, and waits for them.
func (r *clusterReader) close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	r.group.Wait()
}

// spawn runs work under the reads' life, counted until it returns. It reports
// false once the reads have closed, and runs nothing.
func (r *clusterReader) spawn(work func(context.Context)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.group.Go(func() { work(r.ctx) })
	return true
}

// behind counts reads that go on hearing answers behind their callers.
func (r *clusterReader) behind(change int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lingering += change
	if r.lingering == 0 {
		close(r.quiet)
		r.quiet = make(chan struct{})
	}
}

// settle returns once no read is hearing answers behind its caller, and so
// every repair a read will make has been handed to the fills.
func (r *clusterReader) settle(ctx context.Context) error {
	for {
		r.mu.Lock()
		lingering, quiet := r.lingering, r.quiet
		r.mu.Unlock()
		if lingering == 0 {
			return nil
		}
		select {
		case <-quiet:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// count changes the stats under the lock.
func (r *clusterReader) count(change func(stats *ReadStats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	change(&r.stats)
}

// statistics is what the reads did.
func (r *clusterReader) statistics() ReadStats {
	r.mu.Lock()
	stats := r.stats
	r.mu.Unlock()
	stats.Down = r.marks.down()
	stats.Delay = r.hedge.delay()
	stats.Bound = r.bound()
	return stats
}

// bug reports whether the in-tree bug id is on for the cache's run, probe
// marks name reached on it, and buggify fires the site id there. Each asks
// the context the cache was made under, which carries the run, whatever
// context the read that reaches it carries.
func (r *clusterReader) bug(id string) bool                { return sim.Bug(r.ctx, id) }
func (r *clusterReader) probe(name string)                 { sim.Probe(r.ctx, name) }
func (r *clusterReader) buggify(id string, p float64) bool { return sim.Buggify(r.ctx, id, p) }

// on reports whether key is read from the cluster: its window is inside the
// share, on a disk that follows a membership.
func (r *clusterReader) on(key diskKey) bool {
	_, ok := r.disk.placedBy(key, false)
	return ok
}

// bound is how long a read waits for stripes before it reads the store as
// well: boundFactor delays, and no less than the configured bound.
func (r *clusterReader) bound() time.Duration {
	return max(r.settings.bound, boundFactor*r.hedge.delay())
}

// earn adds one read that asked the cluster to the store hedge's bucket.
func (r *clusterReader) earn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = min(r.tokens+1, storeHedgeMax*storeHedgeEarn)
}

// takeStoreHedge spends one read of the store from the bucket, if it holds
// one.
func (r *clusterReader) takeStoreHedge() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tokens < storeHedgeEarn && !r.bug("cluster-store-unbounded") {
		r.stats.StoreHedgesRefused++
		r.probe(ProbeClusterStoreHedgeRefused)
		return false
	}
	r.tokens = max(r.tokens-storeHedgeEarn, 0)
	r.stats.StoreHedges++
	r.probe(ProbeClusterStoreHedge)
	return true
}

// sampleHit counts one hit of the disk tier, and reports whether it is the one
// in headEvery whose part is checked.
func (r *clusterReader) sampleHit() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits++
	return r.settings.headEvery > 0 && r.hits%uint64(r.settings.headEvery) == 0
}

// clusterWant is one envelope a read wants of the cluster: its key, the most
// its decoded bytes may hold, and what they must be.
type clusterWant struct {
	key     diskKey
	maximum int
	valid   func([]byte) bool
}

// storeHedge reads the wants at the given positions from the store, decoded.
type storeHedge func(ctx context.Context, ats []int) ([][]byte, error)

// read reads wants from the cluster, each window's at once, and returns the
// decoded bytes of each, nil for one the cluster could not rebuild, and the
// envelopes it rebuilt under an earlier code, in the order of their windows:
// the caller fills the cluster with them under the deployment's code once
// its callers have their pages, as it fills what the store served. Every
// want must be one the cluster is on for. Past the bound, hedge, when not
// nil, reads the wants still waited on from the store, at most once and
// within the bucket, and whichever of the two answers first is taken; a read
// the store answered refills nothing.
func (r *clusterReader) read(ctx context.Context, codecs *blob.Codecs, wants []clusterWant,
	hedge storeHedge) ([][]byte, []envelope, error) {
	out := make([][]byte, len(wants))
	groups := r.byWindow(wants)
	results := make(chan windowResult, len(groups))
	finished := make([]bool, len(groups))
	refills := make([][]envelope, len(groups))
	for at, g := range groups {
		if !r.spawn(func(context.Context) { results <- windowResult{group: at, out: r.readWindow(ctx, codecs, g)} }) {
			results <- windowResult{group: at, out: windowOut{data: make([][]byte, len(g.wants))}}
		}
	}
	wait := r.bound()
	if r.buggify(buggifyClusterStoreHedgeNow, 0.1) {
		wait = 0
	}
	bound := r.clock.NewTimer(wait)
	defer bound.Stop()
	hedgeCtx, cancelHedge := context.WithCancel(ctx)
	defer cancelHedge()
	var hedged chan hedgeResult
	var hedging []int
	for pending := len(groups); pending > 0; {
		select {
		case result := <-results:
			pending--
			finished[result.group] = true
			refills[result.group] = result.out.refill
			for at, data := range result.out.data {
				if position := groups[result.group].ats[at]; out[position] == nil {
					out[position] = data
				}
			}
		case <-bound.C():
			if hedge == nil {
				continue
			}
			for g, group := range groups {
				if !finished[g] {
					hedging = append(hedging, group.ats...)
				}
			}
			if len(hedging) == 0 || !r.takeStoreHedge() {
				hedging = nil
				continue
			}
			hedged = make(chan hedgeResult, 1)
			ats := slices.Clone(hedging)
			go func() {
				data, err := hedge(hedgeCtx, ats)
				hedged <- hedgeResult{data: data, err: err}
			}()
		case result := <-hedged:
			hedged = nil
			if result.err != nil {
				slog.DebugContext(ctx, "checkpoint: a read of the store past the bound failed; waiting for stripes",
					"error", result.err)
				continue
			}
			for at, position := range hedging {
				if out[position] == nil {
					out[position] = result.data[at]
				}
			}
			r.count(func(stats *ReadStats) { stats.StoreHedgesWon++ })
			r.probe(ProbeClusterStoreHedgeWon)
			return out, nil, nil
		case <-ctx.Done():
			return nil, nil, context.Cause(ctx)
		}
	}
	return out, slices.Concat(refills...), nil
}

// windowResult is what the read of one group of wants rebuilt.
type windowResult struct {
	group int
	out   windowOut
}

// hedgeResult is what a read of the store past the bound read.
type hedgeResult struct {
	data [][]byte
	err  error
}

// windowGroup is the wants of one window, and where they are among a read's,
// and the membership they are read under.
type windowGroup struct {
	window rank.Window
	m      membership.Membership
	ats    []int
	wants  []clusterWant
}

// byWindow puts wants together by window, in the order their windows first
// come, each placed by the membership the disk follows now.
func (r *clusterReader) byWindow(wants []clusterWant) []windowGroup {
	var groups []windowGroup
	at := make(map[rank.Window]int)
	for position, want := range wants {
		window := want.key.rankWindow()
		index, seen := at[window]
		if !seen {
			m, _ := r.disk.placedBy(want.key, false)
			index = len(groups)
			at[window] = index
			groups = append(groups, windowGroup{window: window, m: m})
		}
		groups[index].ats = append(groups[index].ats, position)
		groups[index].wants = append(groups[index].wants, want)
	}
	return groups
}

// heldStripe is one stripe a read has in hand, and the cache it came from.
type heldStripe struct {
	stripe stripe.Stripe
	from   rank.Cache
	// own is where this host's disk holds it, for a stripe of its own.
	own *diskLocation
}

// stripeAnswer is one holder's answer to a stripe request.
type stripeAnswer struct {
	cache rank.Cache
	reply peer.StripesReply
	err   error
}

// windowRead is one read of the cluster for the wants of one window, under
// one code, under one generation of the membership.
type windowRead struct {
	r      *clusterReader
	codecs *blob.Codecs
	window rank.Window
	m      membership.Membership
	// code is the code the read takes stripes of. earlier marks a code the
	// deployment used before its own, which the read does not repair under.
	code    rank.Code
	earlier bool
	// own says this host's own stripes rebuilt every want, with no request,
	// and cut that the read's caller gave up on it before it ended.
	own, cut bool
	wants    []clusterWant
	// owned says this host serves its disk under m, so its own stripes are
	// part of the read.
	owned bool
	// newer is the newest generation a holder answered that the read is stale
	// with, zero for none.
	newer uint64
	// pages is each want's page of the window.
	pages []uint32
	// ranks is the window's first k+m ranks, and holders the cache each index
	// of the code goes on.
	ranks, holders []rank.Cache
	self           rank.Cache

	// held is the stripes in hand of each want, tried how many were in hand
	// at its last rebuild that failed, out what each rebuilt to and envelopes
	// the envelope it was rebuilt from.
	held      [][]heldStripe
	tried     []int
	out       [][]byte
	envelopes [][]byte
	// answered is, for each rank that answered, the indices of each want it
	// holds.
	answered map[rank.Identity][][]int
	// spares is the ranks not yet asked, in the order they are asked,
	// askedOf the ranks asked, and pending the requests not yet answered.
	spares  []rank.Cache
	askedOf map[rank.Identity]bool
	pending int

	events   chan stripeAnswer
	mu       sync.Mutex
	finished bool
}

// windowOut is what the read of one window's wants rebuilt: the decoded bytes
// of each, nil for one it could not rebuild, how many it rebuilt under an
// earlier code, and their envelopes, which the deployment's code is filled
// with.
type windowOut struct {
	data    [][]byte
	earlier int
	refill  []envelope
}

// readWindow reads the wants of one window from the cluster, under one
// generation of the membership: under its code, then what it could not
// rebuild under each earlier code in turn. A read that a holder ahead of it
// answers stale reads the membership and reads the window again under the
// newer generation.
func (r *clusterReader) readWindow(ctx context.Context, codecs *blob.Codecs, g windowGroup) windowOut {
	for tries := 0; ; tries++ {
		out, own, newer, cut := r.readCodes(ctx, codecs, g)
		if cut {
			// A read its caller gave up on counts nothing.
			return out
		}
		if newer <= g.m.Generation() || tries == staleRetries {
			r.counted(out, own)
			return out
		}
		next, err := r.disk.catch(ctx, newer)
		if err != nil || next.Generation() <= g.m.Generation() {
			slog.DebugContext(ctx, "checkpoint: a read told it is stale could not read the membership",
				"generation", g.m.Generation(), "holder", newer, "error", err)
			r.counted(out, own)
			return out
		}
		r.probe(membership.ProbeSenderCaughtUp)
		g.m = next
	}
}

// readCodes reads the wants of g's window under each of its membership's
// codes in turn, and reports what they rebuilt, whether this host's own
// stripes rebuilt every want under the membership's code, the generation of
// a holder that answered the read is stale, and whether the read's caller
// gave up on it.
func (r *clusterReader) readCodes(ctx context.Context, codecs *blob.Codecs, g windowGroup) (windowOut, bool,
	uint64, bool) {
	out := windowOut{data: make([][]byte, len(g.wants))}
	own := false
	for at, code := range g.m.List().Codes() {
		var missing []int
		for position, data := range out.data {
			if data == nil {
				missing = append(missing, position)
			}
		}
		if len(missing) == 0 || ctx.Err() != nil || at > 0 && r.bug("cluster-current-code-only") {
			break
		}
		wants := make([]clusterWant, len(missing))
		for want, position := range missing {
			wants[want] = g.wants[position]
		}
		w := r.windowRead(codecs, g.window, g.m, code, wants, at > 0)
		w.run(ctx)
		if w.cut {
			return out, own, 0, true
		}
		if w.stale() {
			return out, own, w.newer, false
		}
		own = own || at == 0 && w.own
		for want, position := range missing {
			if w.out[want] == nil {
				continue
			}
			out.data[position] = w.out[want]
			if at > 0 {
				out.earlier++
				if !r.bug("cluster-no-refill") {
					out.refill = append(out.refill, envelope{key: wants[want].key, data: w.envelopes[want]})
				}
			}
		}
	}
	return out, own, 0, false
}

// windowRead starts the read of wants of window under m, of the stripes of
// code, from the window's ranks under that code. earlier marks a code the
// deployment used before its own.
func (r *clusterReader) windowRead(codecs *blob.Codecs, window rank.Window, m membership.Membership, code rank.Code,
	wants []clusterWant, earlier bool) *windowRead {
	list := m.List().Under(code)
	ranks := list.Ranks(window)
	w := &windowRead{r: r, codecs: codecs, window: window, m: m, code: code, earlier: earlier, wants: wants,
		owned: r.disk.owns(m), ranks: ranks, holders: list.Holders(window), self: rank.Cache{Identity: r.disk.identity},
		held: make([][]heldStripe, len(wants)), tried: make([]int, len(wants)), out: make([][]byte, len(wants)),
		envelopes: make([][]byte, len(wants)), answered: make(map[rank.Identity][][]int),
		askedOf: make(map[rank.Identity]bool),
		events:  make(chan stripeAnswer, len(ranks))}
	for _, want := range wants {
		w.pages = append(w.pages, uint32(want.key.Page-window.Page(0)))
	}
	return w
}

// stale reports a read a holder ahead of it answered stale.
func (w *windowRead) stale() bool { return w.newer > w.m.Generation() }

// routed reports whether some member serves cache under the read's
// membership, so a request has somewhere to go.
func (w *windowRead) routed(cache rank.Cache) bool {
	_, ok := w.m.Route(cache.Identity)
	return ok
}

// run is the read: this host's own stripes, then k+1 of the ranks, then the
// rest after the delay, until every want is rebuilt or nothing is left to ask.
func (w *windowRead) run(ctx context.Context) {
	handedOver := false
	defer func() {
		if !handedOver {
			w.finish()
		}
	}()
	r := w.r
	began := r.clock.Now()
	// A prefetch is background work: nothing waits on it, so it asks no
	// second request after the delay, and the delay, an estimate of what a
	// fault's read takes, is not drawn from it. See prefetch.go.
	prefetch := Prefetching(ctx)
	own := w.owned && w.readOwn(ctx)
	w.join(ctx)
	if w.complete() {
		w.own = true
		for _, want := range w.wants {
			r.disk.served(want.key, w.code)
		}
		w.repair(ctx)
		return
	}
	if prefetch {
		r.count(func(stats *ReadStats) { stats.Prefetches++ })
	} else if !w.earlier {
		// One read of a window earns once, whatever codes it tries.
		r.earn()
	}
	var others []rank.Cache
	for _, cache := range w.ranks {
		if cache.Identity != w.self.Identity && w.routed(cache) && !r.marks.isDown(cache.Identity) &&
			!w.tableDown(cache) {
			others = append(others, cache)
		}
	}
	want := w.code.K + 1
	if own && w.ranked(w.self.Identity) {
		// This host is one of the k+1, and its stripes are in hand.
		want--
	}
	order := rank.Pick(others, w.self.Identity, w.window, want)
	switch {
	case r.bug("cluster-ask-every-holder"):
		want = len(others)
	case r.bug("cluster-same-holders-for-every-reader"):
		order = others
	}
	first := min(want, len(order))
	w.spares = order[first:]
	for _, cache := range order[:first] {
		w.ask(ctx, cache)
	}
	var delay <-chan time.Time
	if len(w.spares) > 0 && !prefetch && !r.bug("cluster-no-second-request") {
		timer := r.clock.NewTimer(r.hedge.delay())
		defer timer.Stop()
		delay = timer.C()
	}
	waited := false
	for !w.complete() {
		if w.pending == 0 {
			if !w.askNext(ctx) {
				break
			}
			continue
		}
		select {
		case answer := <-w.events:
			w.pending--
			if w.take(ctx, answer) {
				if w.stale() {
					// A holder is ahead of the read: it starts again under
					// the holder's generation.
					return
				}
				w.replace(ctx)
			}
			if w.join(ctx) {
				// k stripes that rebuild nothing: one more tells which is
				// wrong.
				w.replace(ctx)
			}
		case <-delay:
			delay, waited = nil, true
			if len(w.spares) == 0 {
				continue
			}
			if !r.hedge.take() && !r.bug("cluster-hedge-unbudgeted") {
				r.count(func(stats *ReadStats) { stats.Refused++ })
				w.r.probe(ProbeClusterRefused)
				continue
			}
			r.count(func(stats *ReadStats) { stats.SecondRequests++ })
			w.r.probe(ProbeClusterSecondRequest)
			for w.askNext(ctx) {
			}
		case <-ctx.Done():
			w.cut = true
			return
		}
	}
	if w.complete() && !prefetch {
		r.hedge.done(r.clock.Since(began), waited)
	}
	if w.complete() && w.pending > 0 && !w.earlier && w.mayRepair() {
		// The read has its pages. Whether a rank lacks a stripe no rank
		// holds is known only once every rank has answered, and the rest
		// answer behind the read rather than in front of it.
		r.behind(1)
		handedOver = r.spawn(func(life context.Context) {
			defer r.behind(-1)
			defer w.finish()
			w.hearRest(life)
			w.repair(life)
		})
		if handedOver {
			return
		}
		r.behind(-1)
	}
	w.repair(ctx)
}

// mayRepair reports whether the read could still learn what each rank of the
// window holds: every rank has been asked, so once the requests in flight are
// answered, an index no rank holds is known.
func (w *windowRead) mayRepair() bool {
	if len(w.spares) > 0 {
		return false
	}
	for _, cache := range w.ranks {
		if _, heard := w.answered[cache.Identity]; !heard && !w.askedOf[cache.Identity] {
			return false
		}
	}
	return true
}

// hearRest takes the answers of every request still in flight.
func (w *windowRead) hearRest(ctx context.Context) {
	for w.pending > 0 {
		select {
		case answer := <-w.events:
			w.pending--
			w.take(ctx, answer)
		case <-ctx.Done():
			return
		}
	}
}

// ranked reports whether the cache of identity is among the window's ranks.
func (w *windowRead) ranked(identity rank.Identity) bool {
	return slices.ContainsFunc(w.ranks, func(cache rank.Cache) bool { return cache.Identity == identity })
}

// tableDown reports a host the table of peers has marked down for a hard
// failure, which a read that can do without it does not ask.
func (w *windowRead) tableDown(cache rank.Cache) bool {
	return w.r.peers != nil && w.r.peers.Peer(cache.Address).Down()
}

// complete reports every want rebuilt.
func (w *windowRead) complete() bool {
	return !slices.ContainsFunc(w.out, func(data []byte) bool { return data == nil })
}

// readOwn takes the stripes of the window this host's own disk holds, of any
// index, and reports whether there were any. Where this cache is ranked for
// the window, they are its answer.
func (w *windowRead) readOwn(ctx context.Context) bool {
	indices := make([][]int, len(w.wants))
	found := false
	for at, want := range w.wants {
		stripes, locations, _ := w.r.disk.ownStripes(ctx, want.key, w.code)
		for index, s := range stripes {
			w.held[at] = append(w.held[at], heldStripe{stripe: s, from: w.self, own: &locations[index]})
			indices[at] = append(indices[at], s.Index)
			found = true
		}
	}
	if w.ranked(w.self.Identity) {
		w.answered[w.self.Identity] = indices
	}
	return found
}

// maxBytes is the most a reply of cache may hold: for each want, a stripe of
// the largest envelope it may be, as an item, for each index the code puts on
// cache and one more.
func (w *windowRead) maxBytes(cache rank.Cache) int64 {
	indices := 1
	for _, holder := range w.holders {
		if holder.Identity == cache.Identity {
			indices++
		}
	}
	indices = min(indices, w.code.Width())
	total := int64(0)
	for _, want := range w.wants {
		item := itemHeaderBytes(want.key) + int64(stripe.Size(w.code, want.maximum+blob.HeaderSize))
		total += int64(indices) * item
	}
	return min(total, int64(platform.MaxFrameBytes))
}

// ask sends one rank a request for its stripes of the window's wants. The
// request runs under the reads' life, not the read's: it goes on until it is
// answered or times out, and how it ended is what the rank's mark is kept by.
func (w *windowRead) ask(ctx context.Context, cache rank.Cache) {
	r := w.r
	w.pending++
	w.askedOf[cache.Identity] = true
	r.count(func(stats *ReadStats) { stats.Requests++ })
	read := peer.StripeRead{Window: w.window, Pages: w.pages, Code: w.code, MaxBytes: w.maxBytes(cache)}
	route, routed := w.m.Route(cache.Identity)
	if !r.spawn(func(life context.Context) {
		answer := stripeAnswer{cache: cache}
		switch {
		case r.peers == nil:
			answer.err = errNoPeers
		case !routed:
			answer.err = errNoRoute
		default:
			requestCtx, cancel := context.WithCancelCause(life)
			timer := r.clock.AfterFunc(r.settings.stripeTimeout, func() { cancel(errStripeTimeout) })
			answer.reply, answer.err = r.peers.Peer(route.Address).ReadStripes(requestCtx, route, read)
			timer.Stop()
			cancel(nil)
		}
		if answer.err == nil && w.r.buggify(buggifyClusterLoseAnswer, 0.05) {
			answer.reply.Release()
			answer.reply, answer.err = peer.StripesReply{}, errAnswerLost
		}
		r.marks.observe(ctx, cache, answer.err, answer.err == nil && len(answer.reply.Items) == 0)
		w.deliver(answer)
	}) {
		w.deliver(stripeAnswer{cache: cache, err: peer.ErrClosed})
	}
}

// askNext asks the next rank not yet asked that is not marked down, and
// reports whether there was one.
func (w *windowRead) askNext(ctx context.Context) bool {
	for len(w.spares) > 0 {
		next := w.spares[0]
		w.spares = w.spares[1:]
		if w.r.marks.isDown(next.Identity) || w.tableDown(next) {
			continue
		}
		w.ask(ctx, next)
		return true
	}
	return false
}

// replace asks the next rank at once in place of one that gave nothing.
func (w *windowRead) replace(ctx context.Context) {
	if w.askNext(ctx) {
		w.r.count(func(stats *ReadStats) { stats.Replaced++ })
		w.r.probe(ProbeClusterReplaced)
	}
}

// deliver hands an answer to the read, or gives its buffer back once the read
// has finished.
func (w *windowRead) deliver(answer stripeAnswer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		if answer.err == nil {
			answer.reply.Release()
		}
		return
	}
	w.events <- answer
}

// finish ends the read: what is still to arrive is given back as it comes.
func (w *windowRead) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finished = true
	for {
		select {
		case answer := <-w.events:
			if answer.err == nil {
				answer.reply.Release()
			}
		default:
			return
		}
	}
}

// take takes the stripes of one answer, and reports whether it gave nothing:
// an error, BUSY, or no stripe that passed its checks. An answer that the
// read is stale, from a holder ahead of it, is noted, and the read starts
// again under the holder's generation; one from a holder behind it, which
// could not read the membership, is a miss.
func (w *windowRead) take(ctx context.Context, answer stripeAnswer) bool {
	if answer.err != nil {
		var stale *peer.StaleError
		if errors.As(answer.err, &stale) && stale.Generation > w.m.Generation() &&
			!w.r.bug("membership-ignore-stale-answer") {
			w.newer = max(w.newer, stale.Generation)
		}
		return true
	}
	defer answer.reply.Release()
	indices := make([][]int, len(w.wants))
	took := false
	offset := 0
	for _, item := range answer.reply.Items {
		raw := answer.reply.Payload[offset : offset+item.Size]
		offset += item.Size
		at := slices.Index(w.pages, item.Page)
		s, err := w.parse(ctx, at, item, raw)
		if err != nil {
			slog.WarnContext(ctx, "checkpoint: a peer sent a stripe that fails its checks; it is told to drop it",
				"cache", answer.cache.Identity, "window", w.window, "page", item.Page, "index", item.Index, "error", err)
			w.dropAt(ctx, answer.cache, item.Page, item.Index)
			continue
		}
		if w.r.bug("cluster-read-by-index") && w.holders[s.Index].Identity != answer.cache.Identity {
			// The guard takes from each rank only the indices the ranks put
			// on it, which a change of ranks leaves few of (B5).
			continue
		}
		w.held[at] = append(w.held[at], heldStripe{stripe: s, from: answer.cache})
		indices[at] = append(indices[at], s.Index)
		took = true
	}
	w.answered[answer.cache.Identity] = indices
	return !took
}

// parse checks one item a peer sent as the stripe of want at it says it is: a
// page asked for, a header that names the want's key, the read's code and the
// index the reply gives, and a checksum that holds. Its bytes are copied out
// of the reply's buffer.
func (w *windowRead) parse(ctx context.Context, at int, item peer.StripeItem, raw []byte) (stripe.Stripe, error) {
	if at < 0 {
		return stripe.Stripe{}, fmt.Errorf("page %d was not asked for", item.Page)
	}
	raw = bytes.Clone(raw)
	if len(raw) > diskItemFixed && w.r.buggify(buggifyClusterDamagedItem, 0.02) {
		raw[len(raw)-1] ^= 0x10
	}
	parsed, err := parseItem(raw, false)
	if err != nil {
		return stripe.Stripe{}, err
	}
	s := parsed.stripe()
	if parsed.key != w.wants[at].key || s.Code != w.code || s.Index != item.Index || s.Length != item.Length ||
		!storableStripe(s) {
		return stripe.Stripe{}, fmt.Errorf("%w: stripe %d of %s of %+v", errItemKey, s.Index, s.Code, parsed.key)
	}
	if len(s.Bytes) > 0 && w.r.buggify(buggifyClusterWrongStripe, 0.02) {
		// A stripe whose own checksum holds and whose bytes are not the
		// envelope's.
		s.Bytes[len(s.Bytes)/2] ^= 0x40
	}
	return s, nil
}

// join rebuilds every want not yet rebuilt that has k distinct indices in hand
// and more stripes than at its last try, and tells the holders of the stripes
// that rebuilding finds wrong to drop them. It reports whether some want has k
// stripes in hand that rebuild nothing, so which is wrong cannot be told
// without one more.
func (w *windowRead) join(ctx context.Context) bool {
	short := false
	for at, want := range w.wants {
		if w.out[at] != nil || distinct(w.held[at]) < w.code.K || len(w.held[at]) == w.tried[at] {
			continue
		}
		stripes := make([]stripe.Stripe, len(w.held[at]))
		for position, held := range w.held[at] {
			stripes[position] = held.stripe
		}
		var decoded []byte
		joined, err := stripe.Join(ctx, w.code, stripes, func(envelope []byte) error {
			data, err := w.codecs.Decode(ctx, envelope, want.maximum)
			if err != nil {
				return err
			}
			if !want.valid(data) {
				return ErrCorrupt
			}
			// What a read rebuilt is never nil, which is what a miss is.
			decoded = append([]byte{}, data...)
			return nil
		})
		if context.Cause(ctx) != nil {
			// A rebuild the caller gave up on says nothing about the stripes.
			return false
		}
		if len(joined.Wrong) > 0 && !w.r.bug("cluster-keep-wrong-stripe") {
			kept := w.held[at][:0:0]
			for position, held := range w.held[at] {
				if slices.Contains(joined.Wrong, position) {
					w.wrongStripe(ctx, at, held)
					continue
				}
				kept = append(kept, held)
			}
			w.held[at] = kept
		}
		if err != nil {
			w.tried[at] = len(w.held[at])
			short = short || errors.Is(err, stripe.ErrWrong)
			continue
		}
		w.out[at], w.envelopes[at] = decoded, bytes.Clone(joined.Envelope)
		for _, position := range joined.Used {
			if stripes[position].Index >= w.code.K {
				w.r.probe(ProbeClusterParity)
				break
			}
		}
	}
	return short
}

// distinct is how many distinct indices held holds.
func distinct(held []heldStripe) int {
	var seen []int
	for _, h := range held {
		if !slices.Contains(seen, h.stripe.Index) {
			seen = append(seen, h.stripe.Index)
		}
	}
	return len(seen)
}

// wrongStripe tells the holder of a stripe a rebuild found wrong to drop it:
// this host's own disk forgets it at once, and a peer is sent a drop.
func (w *windowRead) wrongStripe(ctx context.Context, at int, held heldStripe) {
	if held.own != nil {
		w.r.count(func(stats *ReadStats) { stats.WrongStripes++ })
		w.r.probe(ProbeClusterWrongStripe)
		w.r.disk.forget(ctx, *held.own, w.wants[at].key, fmt.Errorf("stripe %d of %s rebuilt no envelope that passes its check",
			held.stripe.Index, w.code))
		return
	}
	w.dropAt(ctx, held.from, w.pages[at], held.stripe.Index)
}

// dropAt counts a stripe of a peer's found wrong, and tells the peer to drop
// it, behind the fills.
func (w *windowRead) dropAt(ctx context.Context, cache rank.Cache, page uint32, index int) {
	w.r.count(func(stats *ReadStats) { stats.WrongStripes++ })
	w.r.probe(ProbeClusterWrongStripe)
	if cache.Identity == w.self.Identity {
		return
	}
	if w.r.filler.tell(w.m, cache, peer.Drop{Window: w.window, Page: page, Index: index, Code: w.code}) {
		w.r.count(func(stats *ReadStats) { stats.DropsSent++ })
		w.r.probe(ProbeClusterDrop)
	}
}

// counted counts what the read of a window rebuilt, under which code, and
// what it missed. own says this host's disk rebuilt everything alone under
// the list's code.
func (r *clusterReader) counted(out windowOut, own bool) {
	hits, misses := uint64(0), uint64(0)
	for _, data := range out.data {
		if data == nil {
			misses++
		} else {
			hits++
		}
	}
	earlier := uint64(out.earlier)
	r.count(func(stats *ReadStats) {
		stats.Hits += hits
		stats.Misses += misses
		stats.EarlierHits += earlier
		if own {
			stats.OwnHits += hits
		}
	})
	switch {
	case hits > 0 && own:
		r.probe(ProbeClusterOwnHit)
	case hits > 0:
		r.probe(ProbeClusterHit)
	}
	if earlier > 0 {
		r.probe(ProbeClusterEarlierCode)
	}
	if misses > 0 {
		r.probe(ProbeClusterMiss)
	}
}

// repair sends each index of a rebuilt envelope that no rank holds to a rank
// that holds fewer of the window's stripes than the code puts on it, in rank
// order, offering each rank first the indices the membership puts on it. Only
// a read that heard from every rank knows what no rank holds, so
// only such a read repairs; one that did not ask every rank leaves the window
// to a reader that does. It never sends an index another rank holds, so a
// change of ranks never leaves one index on two ranks. A window read under an
// earlier code is not repaired: it is filled under the deployment's code
// instead, and what it holds under the earlier one ages out.
func (w *windowRead) repair(ctx context.Context) {
	if w.earlier {
		return
	}
	for _, cache := range w.ranks {
		if _, heard := w.answered[cache.Identity]; !heard {
			return
		}
	}
	share := make(map[rank.Identity]int)
	for _, holder := range w.holders {
		share[holder.Identity]++
	}
	sending := make(map[rank.Identity][]keyedStripe)
	for at, envelope := range w.envelopes {
		if envelope == nil {
			continue
		}
		var held []int
		for _, cache := range w.ranks {
			held = append(held, w.answered[cache.Identity][at]...)
		}
		var stripes []stripe.Stripe
		for _, cache := range w.ranks {
			lacking := share[cache.Identity] - len(w.answered[cache.Identity][at])
			for _, index := range w.preferred(cache.Identity) {
				if lacking <= 0 {
					break
				}
				if slices.Contains(held, index) && !w.r.bug("cluster-repair-held-index") ||
					slices.Contains(w.answered[cache.Identity][at], index) {
					continue
				}
				if stripes == nil {
					split, err := stripe.Split(w.code, envelope)
					if err != nil {
						return
					}
					stripes = split
				}
				held = append(held, index)
				lacking--
				sending[cache.Identity] = append(sending[cache.Identity], keyedStripe{key: w.wants[at].key,
					stripe: stripes[index]})
			}
		}
	}
	for _, cache := range w.ranks {
		if repairs := sending[cache.Identity]; len(repairs) > 0 {
			w.r.count(func(stats *ReadStats) { stats.Repairs += uint64(len(repairs)) })
			w.r.probe(ProbeClusterRepair)
			w.r.filler.repair(w.m, w.window, w.code, cache, repairs)
		}
	}
}

// preferred is the indices of the code in the order a repair offers them to
// the disk of identity: those the ranks put on it first, so a window that
// lost a stripe gets back the placement a fill gives it, then the rest.
func (w *windowRead) preferred(identity rank.Identity) []int {
	var first, rest []int
	for index, holder := range w.holders {
		if holder.Identity == identity {
			first = append(first, index)
		} else {
			rest = append(rest, index)
		}
	}
	return append(first, rest...)
}

// hedger is one reader's delay before it asks the rest of a window's ranks,
// and its budget for doing so, as FoundationDB's load balancer keeps them.
// The delay is the 95th percentile of the reader's recent times to k stripes,
// and no less than a floor. The budget counts twentieths of a request: a read
// that had its stripes within the delay adds one, and a second request takes
// twenty.
type hedger struct {
	floor time.Duration

	mu     sync.Mutex
	wait   time.Duration
	budget int
	seen   int
	recent [hedgeWindow]time.Duration
	sorted [hedgeWindow]time.Duration
}

func (h *hedger) delay() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wait
}

// take spends one second request, if the budget holds one.
func (h *hedger) take() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.budget < hedgeEarn {
		return false
	}
	h.budget -= hedgeEarn
	return true
}

// done records a read that had k stripes of every page in took. waited says
// the delay passed before they came.
func (h *hedger) done(took time.Duration, waited bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !waited {
		h.budget = min(h.budget+1, hedgeMax*hedgeEarn)
	}
	h.recent[h.seen%hedgeWindow] = took
	h.seen++
	if h.seen%hedgeEvery != 0 {
		return
	}
	n := min(h.seen, hedgeWindow)
	s := h.sorted[:n]
	copy(s, h.recent[:n])
	slices.Sort(s)
	h.wait = max(s[(n*95+99)/100-1], h.floor)
}

// checkHit has the part an envelope of key was served from checked with a
// HEAD, for one hit of the disk tier in headEvery, behind the fills. A warm
// cache hides a reclamation that deleted a part some root still reads, until
// the cache turns over far from the cause; a part found missing is logged as
// an error with the page's identity, and counted. object names the part, or
// the index object for a segment.
func (r *clusterReader) checkHit(ctx context.Context, key diskKey, objects platform.ObjectStore,
	object func() (platform.ObjectKey, error)) {
	if !r.sampleHit() || r.bug("cluster-head-never") {
		return
	}
	named, err := object()
	if err != nil {
		return
	}
	r.filler.behind(func(ctx context.Context) {
		r.count(func(stats *ReadStats) { stats.HeadChecks++ })
		r.probe(ProbeClusterHeadCheck)
		_, err := objects.Head(ctx, named)
		switch {
		case errors.Is(err, platform.ErrNotFound):
			r.count(func(stats *ReadStats) { stats.HeadMissing++ })
			r.probe(ProbeClusterHeadMissing)
			slog.ErrorContext(ctx, "checkpoint: the cache served a page whose part the store no longer holds",
				"object", named.String(), "checkpoint", key.Ref.String(), "volume", key.Volume, "page", key.Page,
				"segment", key.segment)
		case err != nil:
			slog.DebugContext(ctx, "checkpoint: a sampled check of a cached page's part failed", "object",
				named.String(), "error", err)
		}
	})
}
