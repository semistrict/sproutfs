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

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// Filling the cluster. Inside the share the cluster cache is turned on for,
// three things bring a window to the cluster: a read of the store, a
// publication and a pull. Each hands the envelopes it has in hand to the
// cache's filler, which splits them under the list's code and puts each
// stripe on the cache the list holds it on: this host's own stripes on its
// own disk, and every other as a keep to the host whose cache holds it.
//
// A read hands over the run the store served once the read's callers have
// their bytes, never before. A read of the cluster that rebuilt a window
// under a code the deployment used before its own hands it over the same
// way, so the window moves to the deployment's code. Every fill is split
// under the list's own code, never an earlier one. A publication hands over each part once its PUT
// has succeeded, and the segments it wrote once the index object has, so no
// cache ever holds the bytes of a part the store refused. A pull hands over
// what it copies.
//
// Nothing waits on a fill. The filler holds the fills handed to it in one
// queue, bounded in bytes, and does them one at a time in the order they were
// handed over: it asks a read's fill right, writes this host's own stripes and
// sends each keep, and takes the next fill once the keep before it is
// answered. Every write to this host's own disk, its own fills' and its peers'
// keeps', goes through one queue of writes drained by one worker. Every keep
// goes through a rate of bytes per second and this host's background budget
// at the fill priority. A fill that finds the queue full, the rate spent or
// the budget without room is dropped, and its window is read from the store
// the next time. A fault, a publication and a pull never wait for one.
//
// One fill at a time is a decision, not a convenience. Fills on goroutines of
// their own reach a peer's link, its connection and the background budget in
// whatever order the Go scheduler runs them: which keep a dropped frame or a
// partition takes, and which finds the budget full, would be the scheduler's
// choice rather than the order the fills were handed over in. A keep is
// background work under a rate, so its round trip costs a fill nothing it
// needs.
//
// A cold burst would fill one window many times: many hosts miss it at once,
// each reads the store, and each would send its stripes. So a read's fill needs
// the window's fill right, which the cache ranked first for the window hands
// out once per window per interval, and only while it holds nothing of the
// pages asked for. A reader asks for it with a read of stripes that wants no
// bytes; a reader that is not given it sends nothing. A publication and a pull
// need no right: each is the only one of its kind for its window.
//
// The cache takes a keep only for a window its own list ranks it for, under
// its list's code, and drops every stripe of it that it already holds or is
// already writing.

// Defaults of a cache's fills.
const (
	// DefaultFillQueueBytes bounds the host's queue of writes to its own disk.
	DefaultFillQueueBytes = 64 << 20
	// DefaultFillBytesPerSecond is the rate of keeps a host sends its peers,
	// with a burst of one second of it.
	DefaultFillBytesPerSecond = 128 << 20
	// DefaultFillRightInterval is how long a window's fill right, once given,
	// is not given again.
	DefaultFillRightInterval = 10 * time.Second
)

// DropReason is why a fill dropped stripes.
type DropReason int

const (
	// DropQueue is the host's queue of writes to its own disk being full.
	DropQueue DropReason = iota
	// DropRate is the host's rate of keeps being spent.
	DropRate
	// DropBudget is this host's background budget having no room for a keep.
	DropBudget
	// DropBusy is the holder at its budget for this host's writes.
	DropBusy
	// DropDown is a holder marked down.
	DropDown
	// DropStale is a holder's address answering for another cache: the list
	// held is stale.
	DropStale
	// DropPeer is a keep its holder dropped: its queue was full or its disk
	// refused the write. A holder that already held every stripe of a keep
	// drops it too.
	DropPeer
	// DropDisk is this host's own disk refusing the write: its share, its
	// write budget or its index.
	DropDisk
	// DropFailed is a fill that failed on its way: a keep whose request
	// failed, a write the disk failed, or work the cache closed under.
	DropFailed
	dropReasons
)

func (r DropReason) String() string {
	return [...]string{"queue", "rate", "budget", "busy", "down", "stale", "peer", "disk", "failed"}[r]
}

// DropReasons is every reason, in order.
func DropReasons() []DropReason {
	reasons := make([]DropReason, dropReasons)
	for at := range reasons {
		reasons[at] = DropReason(at)
	}
	return reasons
}

// FillStats is what a cache's fills did, in stripes unless they say otherwise.
type FillStats struct {
	// FromReads and FromPublications count the windows this host filled from
	// its reads of the store, once rank 1 gave it the fill right, and from
	// its publications and pulls.
	FromReads, FromPublications uint64
	// WithoutRight counts the windows this host read from the store and sent
	// nothing of, for want of the fill right, and RightsGranted the rights
	// its cache gave out as rank 1.
	WithoutRight, RightsGranted uint64
	// Sent is the stripes this host's keeps carried that their holders kept,
	// and SentBytes what those keeps carried.
	Sent, SentBytes uint64
	// Kept is the stripes fills wrote to this host's own disk: its own, and
	// what its peers' keeps carried.
	Kept uint64
	// Dropped is the stripes fills dropped, by reason.
	Dropped [dropReasons]uint64
	// Duplicates is the stripes a fill or a keep carried that this cache
	// already held or was already writing. Refused is the stripes of keeps
	// this cache refused: for a window its list does not rank it for, under
	// another code, or that do not hold together.
	Duplicates, Refused uint64
	// Queued is the bytes of the queue held now, and QueueBytes its bound.
	Queued, QueueBytes int64
}

// The probes fills mark.
const (
	// ProbeFillRightGranted is a fill right rank 1 gave out.
	ProbeFillRightGranted = "checkpoint/fill-right-granted"
	// ProbeFillWithoutRight is a read of the store that sent nothing for want
	// of the fill right.
	ProbeFillWithoutRight = "checkpoint/fill-without-right"
	// ProbeFillRightLost is a right rank 1 gave whose answer never reached
	// the reader.
	ProbeFillRightLost = "checkpoint/fill-right-lost"
	// ProbeFillQueueFull is a fill dropped for a full queue.
	ProbeFillQueueFull = "checkpoint/fill-dropped-queue-full"
	// ProbeFillRateSpent is a keep dropped for a spent rate.
	ProbeFillRateSpent = "checkpoint/fill-dropped-rate-spent"
	// ProbeFillNoRoom is a keep dropped for want of room in the background
	// budget.
	ProbeFillNoRoom = "checkpoint/fill-dropped-background-budget"
	// ProbeFillPeerDropped is a keep its holder dropped.
	ProbeFillPeerDropped = "checkpoint/fill-dropped-by-holder"
	// ProbeFillWriteRefused is a fill's write this host's disk refused.
	ProbeFillWriteRefused = "checkpoint/fill-write-refused"
	// ProbeFillRanksChanged is a fill placed by a list other than the one
	// its right was asked under.
	ProbeFillRanksChanged = "checkpoint/fill-ranks-changed"
	// ProbeKeepKept is a keep a cache wrote.
	ProbeKeepKept = "checkpoint/keep-kept"
	// ProbeKeepDuplicate is a stripe of a keep the cache held or was writing.
	ProbeKeepDuplicate = "checkpoint/keep-duplicate"
	// ProbeKeepRefused is a keep for a window the cache's list does not rank
	// it for.
	ProbeKeepRefused = "checkpoint/keep-refused"
	// ProbeKeepDropped is a keep a cache dropped as one whose write budget is
	// spent does.
	ProbeKeepDropped = "checkpoint/keep-dropped"
)

// The fault-injection sites of fills.
const (
	// buggifyFillQueueFull has the queue report itself full.
	buggifyFillQueueFull = "checkpoint/fill-queue-full"
	// buggifyFillLoseRight loses the answer that carried a fill right.
	buggifyFillLoseRight = "checkpoint/fill-lose-right"
	// buggifyFillRanksChange places a fill by the list less one of the
	// caches it ranks, as a list that changed between the read and the fill.
	buggifyFillRanksChange = "checkpoint/fill-ranks-change"
	// buggifyFillSendTwice sends a keep twice, as a sender that retried one
	// whose answer it lost.
	buggifyFillSendTwice = "checkpoint/fill-send-twice"
	// buggifyFillRefuseWrite has the write budget refuse a fill's write.
	buggifyFillRefuseWrite = "checkpoint/fill-refuse-write"
	// buggifyKeepDrop has a cache drop a keep, as one whose write budget is
	// spent does.
	buggifyKeepDrop = "checkpoint/keep-drop"
)

// errKeepRefused reports a keep a cache will not take.
var errKeepRefused = errors.New("checkpoint: the keep is refused")

// stripeKey names one stripe of one envelope.
type stripeKey struct {
	key  diskKey
	code diskCode
}

// keyedStripe is one stripe and the envelope it is of.
type keyedStripe struct {
	key    diskKey
	stripe stripe.Stripe
}

// filler is a cache's fills: the fills handed over and the one worker that
// does them in order, its queue of writes to its own disk and the one worker
// that drains it, the rate of its keeps, and the fill rights it gives out as
// rank 1.
type filler struct {
	disk  *cacheDisk
	peers *peer.Table
	clock platform.Clock
	// ctx is the fills' life, which carries what the cache was made under;
	// Close ends it.
	ctx    context.Context
	cancel context.CancelFunc
	group  sync.WaitGroup

	queueBytes    int64
	rightInterval time.Duration
	rate          tokenBucket

	// down reports a holder the cache's reads have marked down, which is sent
	// no fill. Nil marks none.
	down func(rank.Identity) bool

	// fills is the fills handed over, which one worker does one at a time:
	// it asks the right, writes this host's stripes and sends the keeps of
	// each before it takes the next. writes is the writes to this host's own
	// disk, its fills' and its peers' keeps', which another worker does one at
	// a time. The two are apart because a keep waits for its holder's writes,
	// and a holder's writes must never wait for that holder's own keeps.
	fills, writes *lane

	mu sync.Mutex
	// queued is the bytes of the queue held: by fills handed over and not yet
	// done, and by peers' keeps not yet written.
	queued int64
	// busy counts the work held and the requests in flight; idle is closed
	// and replaced each time it falls to zero.
	busy int
	idle chan struct{}
	// writing is the stripes the queue holds for a keep, which a stripe of
	// another fill or keep is a duplicate of.
	writing map[stripeKey]bool
	// granted is when each window's right was last given out, and grants the
	// windows in the order they were, which is the order they lapse in.
	granted map[rank.Window]time.Time
	grants  []grant
	stats   FillStats
	closed  bool
}

// grant is one fill right given out.
type grant struct {
	window rank.Window
	at     time.Time
}

// fillSettings is how a cache fills the cluster.
type fillSettings struct {
	peers                      *peer.Table
	clock                      platform.Clock
	queueBytes, bytesPerSecond int64
	rightInterval              time.Duration
}

// newFiller starts a disk's fills under ctx.
func newFiller(ctx context.Context, disk *cacheDisk, settings fillSettings) *filler {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	clock := platform.ClockOr(settings.clock)
	f := &filler{disk: disk, peers: settings.peers, clock: clock, ctx: ctx, cancel: cancel,
		queueBytes: settings.queueBytes, rightInterval: settings.rightInterval,
		rate: newTokenBucket(clock, settings.bytesPerSecond), fills: newLane(), writes: newLane(),
		idle: make(chan struct{}), writing: make(map[stripeKey]bool), granted: make(map[rank.Window]time.Time)}
	f.group.Go(func() { f.fills.run(ctx) })
	f.group.Go(func() { f.writes.run(ctx) })
	return f
}

// close stops the fills: both workers, the right asked for and the keep in
// flight. What it had not done is dropped.
func (f *filler) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.cancel()
	f.group.Wait()
}

// statistics is what the fills did.
func (f *filler) statistics() FillStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	stats := f.stats
	stats.Queued, stats.QueueBytes = f.queued, f.queueBytes
	return stats
}

// settle returns once nothing is held: no work queued or being done, and no
// right or keep in flight.
func (f *filler) settle(ctx context.Context) error {
	for {
		f.mu.Lock()
		busy, idle := f.busy, f.idle
		f.mu.Unlock()
		if busy == 0 {
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// hold counts one more piece of work or request in flight, and reports false
// once the fills have closed. Caller holds f.mu.
func (f *filler) hold() bool {
	if f.closed {
		return false
	}
	f.busy++
	return true
}

// done counts one piece of work or request finished.
func (f *filler) done() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy--
	if f.busy == 0 {
		close(f.idle)
		f.idle = make(chan struct{})
	}
}

// goFill runs request beside the fill that started it, counted until it
// returns. It reports false once the fills have closed, and runs nothing. Only
// the fill-concurrently bug runs a request so.
func (f *filler) goFill(request func(context.Context)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hold() {
		return false
	}
	f.group.Go(func() {
		defer f.done()
		request(f.ctx)
	})
	return true
}

// reserve takes bytes of the queue for work a worker will do, and counts
// the work held. It reports false when the queue has no room, and the work is
// dropped. One piece larger than the whole queue is taken alone.
func (f *filler) reserve(ctx context.Context, bytes int64) bool {
	if sim.Buggify(ctx, buggifyFillQueueFull, 0.05) {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.queued > 0 && f.queued+bytes > f.queueBytes {
		if !f.bug("fill-queue-waits") || f.closed {
			return false
		}
		// The bug waits for room rather than dropping, which puts whoever
		// handed the fill over behind the queue.
		idle := f.idle
		f.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
		}
		f.mu.Lock()
		if ctx.Err() != nil {
			return false
		}
	}
	if !f.hold() {
		return false
	}
	f.queued += bytes
	return true
}

// release gives back what reserve took, once the work is done or dropped.
func (f *filler) release(bytes int64) {
	f.mu.Lock()
	f.queued -= bytes
	f.mu.Unlock()
	f.done()
}

// lane is work done one piece at a time, in the order it was handed over, by
// one worker.
type lane struct {
	mu    sync.Mutex
	ready []func(context.Context)
	// wake tells an idle worker of more, and stopped says the worker has
	// returned.
	wake    chan struct{}
	stopped bool
}

func newLane() *lane { return &lane{wake: make(chan struct{}, 1)} }

// push hands work to the lane's worker. Once the worker has returned, the work
// runs here under ended, the fills' ended context, which drops it and gives
// its room back.
func (l *lane) push(ended context.Context, work func(context.Context)) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		work(ended)
		return
	}
	l.ready = append(l.ready, work)
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// run is the lane's worker: it does what the lane holds, in order, until ctx
// ends and the lane is empty. The work left when ctx ends runs under it, and
// ends at once.
func (l *lane) run(ctx context.Context) {
	for {
		l.mu.Lock()
		if len(l.ready) == 0 {
			if ctx.Err() != nil {
				l.stopped = true
				l.mu.Unlock()
				return
			}
			l.mu.Unlock()
			select {
			case <-l.wake:
			case <-ctx.Done():
			}
			continue
		}
		next := l.ready[0]
		l.ready = l.ready[1:]
		l.mu.Unlock()
		next(ctx)
	}
}

// written does work on the queue of writes to this host's own disk, and
// returns once it has: the worker of fills waits for its own writes there,
// behind its peers' keeps.
func (f *filler) written(work func(context.Context)) {
	done := make(chan struct{})
	f.writes.push(f.ctx, func(ctx context.Context) {
		defer close(done)
		work(ctx)
	})
	<-done
}

// bug reports whether the in-tree bug id is on for this cache's run. It is
// asked of the cache's own context, which carries the runtime the cache was
// made under, whatever context the request that reaches it carries.
func (f *filler) bug(id string) bool { return sim.Bug(f.ctx, id) }

// count adds to the stats under the lock.
func (f *filler) count(change func(stats *FillStats)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.stats)
}

// drop counts stripes dropped for reason.
func (f *filler) drop(ctx context.Context, reason DropReason, stripes int) {
	switch reason {
	case DropQueue:
		sim.Probe(ctx, ProbeFillQueueFull)
	case DropRate:
		sim.Probe(ctx, ProbeFillRateSpent)
	case DropBudget:
		sim.Probe(ctx, ProbeFillNoRoom)
	case DropPeer:
		sim.Probe(ctx, ProbeFillPeerDropped)
	case DropDisk:
		sim.Probe(ctx, ProbeFillWriteRefused)
	}
	f.count(func(stats *FillStats) { stats.Dropped[reason] += uint64(stripes) })
}

// windowFill is the envelopes of one window a fill hands over.
type windowFill struct {
	window    rank.Window
	list      rank.List
	envelopes []envelope
	bytes     int64
}

// windows groups envelopes by the window they are in, in the order they come,
// keeping only the windows inside the share that the disk places by its list.
func (f *filler) windows(envelopes []envelope) []*windowFill {
	var fills []*windowFill
	at := make(map[rank.Window]*windowFill)
	for _, e := range envelopes {
		window := e.key.rankWindow()
		fill := at[window]
		if fill == nil {
			list, ok := f.disk.listFor(e.key, false)
			if !ok {
				continue
			}
			fill = &windowFill{window: window, list: list}
			at[window] = fill
			fills = append(fills, fill)
		}
		fill.envelopes = append(fill.envelopes, e)
		fill.bytes += int64(len(e.data))
	}
	return fills
}

// inShare reports whether the disk places key's window by its list: inside
// the share, on a host that follows a list. Such a window is the fills', and
// nothing writes it to the disk but them.
func (f *filler) inShare(key diskKey) bool {
	_, ok := f.disk.listFor(key, false)
	return ok
}

// fill hands envelopes of kind to the cluster, each window inside the share
// to its ranks, and returns at once. kind is WriteFillRead for a run a read
// of the store served, whose windows need the fill right, and
// WriteFillPublication for what a publication uploaded or a pull copied.
// Envelopes outside the share are left alone.
func (f *filler) fill(kind WriteKind, envelopes []envelope) {
	ctx := f.ctx
	for _, fill := range f.windows(envelopes) {
		stripes := len(fill.envelopes) * fill.list.Code().Width()
		if !f.reserve(ctx, fill.bytes) {
			f.drop(ctx, DropQueue, stripes)
			continue
		}
		if kind == WriteFillRead && !f.bug("no-fill-right") && f.bug("fill-concurrently") {
			f.rightBeside(kind, fill)
			continue
		}
		// The right is asked for behind the read, never in front of it; the
		// fill holds its room in the queue until it is done.
		f.fills.push(ctx, func(ctx context.Context) {
			defer f.release(fill.bytes)
			f.do(ctx, kind, fill)
		})
	}
}

// do is one fill, done by the worker of fills: the right a read's fill
// needs, then the fill placed.
func (f *filler) do(ctx context.Context, kind WriteKind, fill *windowFill) {
	// A fill under ended fills asks nothing, and place drops it.
	if ctx.Err() == nil && kind == WriteFillRead && !f.bug("no-fill-right") && !f.right(ctx, fill) {
		f.count(func(stats *FillStats) { stats.WithoutRight++ })
		return
	}
	f.place(ctx, kind, fill)
}

// rightBeside is the bug that asks a read's fill right on a goroutine of its
// own and hands the fill to the worker of fills once the right is answered:
// fills reach rank 1, and then the worker, in the order the scheduler runs
// them rather than the order they were handed over.
func (f *filler) rightBeside(kind WriteKind, fill *windowFill) {
	if !f.goFill(func(ctx context.Context) {
		if !f.right(ctx, fill) {
			f.count(func(stats *FillStats) { stats.WithoutRight++ })
			f.release(fill.bytes)
			return
		}
		f.fills.push(f.ctx, func(ctx context.Context) {
			defer f.release(fill.bytes)
			f.place(ctx, kind, fill)
		})
	}) {
		f.release(fill.bytes)
	}
}

// pages is the pages of its window fill's envelopes are, counted from its
// first.
func (fill *windowFill) pages() []uint32 {
	pages := make([]uint32, 0, len(fill.envelopes))
	for _, e := range fill.envelopes {
		pages = append(pages, uint32(e.key.Page-fill.window.Page(0)))
	}
	return pages
}

// right asks the cache ranked first for fill's window for its fill right: of
// this host's own cache, or of its host by a read of the window's stripes that
// wants no bytes. A cache that cannot be asked gives none.
func (f *filler) right(ctx context.Context, fill *windowFill) bool {
	ranks := fill.list.Ranks(fill.window)
	if len(ranks) == 0 {
		return false
	}
	first, granted := ranks[0], false
	if first.Identity == f.disk.identity {
		granted = f.grant(ctx, fill.window, fill.pages(), fill.list.Code())
	} else if f.peers != nil {
		reply, err := f.peers.Peer(first.Address).ReadStripes(peer.WithClass(ctx, peer.BulkRead), first.Identity,
			peer.StripeRead{Window: fill.window, Pages: fill.pages(), Code: fill.list.Code()})
		if err != nil {
			slog.DebugContext(ctx, "checkpoint: rank 1 could not be asked for a fill right", "window", fill.window,
				"cache", first.Identity, "error", err)
			return false
		}
		reply.Release()
		granted = reply.FillRight
	}
	if granted && sim.Buggify(ctx, buggifyFillLoseRight, 0.1) {
		sim.Probe(ctx, ProbeFillRightLost)
		return false
	}
	if !granted {
		sim.Probe(ctx, ProbeFillWithoutRight)
	}
	return granted
}

// grant decides a fill right as the cache ranked first for window: given to
// the first reader that asks for it under this cache's own list and code,
// while the cache holds nothing of the pages asked for, once per window per
// interval. Pages nil asks for every page of the window.
func (f *filler) grant(ctx context.Context, window rank.Window, pages []uint32, code rank.Code) bool {
	list, ok := f.disk.list()
	if !ok || !validWindow(window, pages) || !window.InShare(f.disk.clusterPercent) || list.Code() != code {
		return false
	}
	ranks := list.Ranks(window)
	if len(ranks) == 0 || ranks[0].Identity != f.disk.identity || f.disk.holdsAnyOf(window, pages, code) {
		return false
	}
	now := f.clock.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.grants) > 0 && now.Sub(f.grants[0].at) >= f.rightInterval {
		lapsed := f.grants[0]
		f.grants = f.grants[1:]
		if f.granted[lapsed.window] == lapsed.at {
			delete(f.granted, lapsed.window)
		}
	}
	if _, given := f.granted[window]; given {
		return false
	}
	f.granted[window] = now
	f.grants = append(f.grants, grant{window: window, at: now})
	f.stats.RightsGranted++
	sim.Probe(ctx, ProbeFillRightGranted)
	return true
}

// validWindow reports a window a peer may name, and pages of it: a span of one
// page or up to a window of the smallest page, one page for a segment, and
// pages within it.
func validWindow(window rank.Window, pages []uint32) bool {
	if window.Pages < 1 || window.Pages > diskWindowBytes/PageSize4KiB || window.Segment && window.Pages != 1 {
		return false
	}
	for _, page := range pages {
		if page >= window.Pages {
			return false
		}
	}
	return true
}

// place splits fill's envelopes under the list held now and puts each stripe
// on the cache the list holds it on: this host's own on its own disk, the
// rest as keeps to their holders, one holder after another in the order the
// list names them. It is the worker of fills', and returns once its own
// stripes are written and every keep is answered or dropped.
func (f *filler) place(ctx context.Context, kind WriteKind, fill *windowFill) {
	if ctx.Err() != nil {
		f.drop(ctx, DropFailed, len(fill.envelopes)*fill.list.Code().Width())
		return
	}
	list, ok := f.disk.listFor(fill.envelopes[0].key, false)
	if !ok {
		return
	}
	if sim.Buggify(ctx, buggifyFillRanksChange, 0.1) {
		for _, cache := range list.Ranks(fill.window) {
			if cache.Identity != f.disk.identity {
				list = list.Without(cache.Identity)
				sim.Probe(ctx, ProbeFillRanksChanged)
				break
			}
		}
	}
	f.count(func(stats *FillStats) {
		if kind == WriteFillRead {
			stats.FromReads++
		} else {
			stats.FromPublications++
		}
	})
	code, holders := list.Code(), list.Holders(fill.window)
	held := make(map[rank.Identity][]keyedStripe)
	var order []rank.Cache
	for _, e := range fill.envelopes {
		stripes, err := stripe.Split(code, e.data)
		if err != nil {
			slog.WarnContext(ctx, "checkpoint: an envelope did not split; the store serves it", "window", fill.window,
				"code", code, "error", err)
			f.drop(ctx, DropFailed, code.Width())
			continue
		}
		for index, holder := range holders {
			if _, seen := held[holder.Identity]; !seen {
				order = append(order, holder)
			}
			held[holder.Identity] = append(held[holder.Identity], keyedStripe{key: e.key, stripe: stripes[index]})
		}
	}
	for _, holder := range order {
		if holder.Identity == f.disk.identity {
			f.written(func(ctx context.Context) { f.writeOwn(ctx, kind, fill.window, code, held[holder.Identity]) })
			continue
		}
		f.send(ctx, kind, fill.window, code, holder, held[holder.Identity])
	}
}

// writeOwn writes the stripes of a fill of window this host's own cache
// holds, leaving out those it holds or is writing already. It is the worker
// of writes', so nothing writes between what it finds held and what it
// writes. The list it was placed by may not be the one the cache holds now,
// which is the one a keep is held to, and so is this.
func (f *filler) writeOwn(ctx context.Context, kind WriteKind, window rank.Window, code rank.Code,
	stripes []keyedStripe) {
	if err := f.ranked(window, code); err != nil && !f.bug("keep-unranked") {
		sim.Probe(ctx, ProbeKeepRefused)
		f.count(func(stats *FillStats) { stats.Refused += uint64(len(stripes)) })
		return
	}
	var missing []keyedStripe
	for _, s := range stripes {
		if f.held(stripeKey{key: s.key, code: codeOf(s.stripe)}) {
			f.count(func(stats *FillStats) { stats.Duplicates++ })
			continue
		}
		missing = append(missing, s)
	}
	if len(missing) == 0 {
		return
	}
	if sim.Buggify(ctx, buggifyFillRefuseWrite, 0.05) {
		f.drop(ctx, DropDisk, len(missing))
		return
	}
	f.write(ctx, kind, missing)
}

// held reports whether the disk holds a stripe or the queue holds it for a
// keep.
func (f *filler) held(s stripeKey) bool {
	f.mu.Lock()
	writing := f.writing[s]
	f.mu.Unlock()
	return writing || f.disk.holdsStripe(s.key, s.code)
}

// write puts stripes on this host's own disk, the stripes of each envelope
// next to each other, and counts what it kept and what the disk refused. It
// reports how many it kept.
func (f *filler) write(ctx context.Context, kind WriteKind, stripes []keyedStripe) int {
	kept := 0
	for start := 0; start < len(stripes); {
		end := start + 1
		for end < len(stripes) && stripes[end].key == stripes[start].key {
			end++
		}
		group := make([]stripe.Stripe, 0, end-start)
		for _, s := range stripes[start:end] {
			group = append(group, s.stripe)
		}
		written, err := f.disk.writeStripes(ctx, stripes[start].key, group, kind)
		if errors.Is(err, ErrDiskRefused) {
			f.drop(ctx, DropDisk, len(group))
		} else if err != nil {
			f.drop(ctx, DropFailed, len(group))
		}
		f.count(func(stats *FillStats) { stats.Kept += uint64(written) })
		kept += written
		start = end
	}
	return kept
}

// send hands one holder's stripes of a window to the rate, sends them as one
// keep and returns once it is answered. The keep carries each stripe as the
// holder's disk stores it, header and checksum included.
func (f *filler) send(ctx context.Context, kind WriteKind, window rank.Window, code rank.Code, holder rank.Cache,
	stripes []keyedStripe) {
	keep := peer.Keep{Window: window, Code: code, Publication: kind == WriteFillPublication, Repair: kind == WriteRepair}
	for _, s := range stripes {
		item := encodeItem(s.key, s.stripe)
		keep.Items = append(keep.Items, peer.StripeItem{Page: uint32(s.key.Page - window.Page(0)), Index: s.stripe.Index,
			Length: s.stripe.Length, Size: len(item)})
		keep.Payload = append(keep.Payload, item...)
	}
	if !f.rate.take(int64(len(keep.Payload))) {
		f.drop(ctx, DropRate, len(stripes))
		return
	}
	if f.peers == nil {
		f.drop(ctx, DropFailed, len(stripes))
		return
	}
	if f.down != nil && f.down(holder.Identity) && !f.bug("cluster-fill-marked-down") {
		// A host this cache's reads have marked down is sent no fill.
		f.drop(ctx, DropDown, len(stripes))
		return
	}
	request := func(ctx context.Context) {
		err := f.peers.Peer(holder.Address).Keep(ctx, holder.Identity, keep)
		f.sent(ctx, err, len(stripes), len(keep.Payload))
		if sim.Buggify(ctx, buggifyFillSendTwice, 0.1) {
			// The second is what a sender that lost the first's answer
			// sends: its holder drops what it already holds, and the second
			// answer counts for nothing.
			_ = f.peers.Peer(holder.Address).Keep(ctx, holder.Identity, keep)
		}
	}
	if !f.bug("fill-concurrently") {
		request(ctx)
		return
	}
	// The bug sends the keep beside the fill, so the next fill's keeps race it
	// to the holder's link.
	if !f.goFill(request) {
		f.drop(ctx, DropFailed, len(stripes))
	}
}

// sent counts what became of a keep: kept, or dropped for why.
func (f *filler) sent(ctx context.Context, err error, stripes, bytes int) {
	switch {
	case err == nil:
		f.count(func(stats *FillStats) {
			stats.Sent += uint64(stripes)
			stats.SentBytes += uint64(bytes)
		})
	case errors.Is(err, peer.ErrNoRoom):
		f.drop(ctx, DropBudget, stripes)
	case errors.Is(err, peer.ErrBusy):
		f.drop(ctx, DropBusy, stripes)
	case errors.Is(err, peer.ErrDown):
		f.drop(ctx, DropDown, stripes)
	case errors.Is(err, peer.ErrNotMe):
		f.drop(ctx, DropStale, stripes)
	case errors.Is(err, peer.ErrDropped):
		f.drop(ctx, DropPeer, stripes)
	default:
		// A keep that failed for a reason the others do not name is worth a
		// line: it is a holder or a link this host cannot use.
		slog.WarnContext(ctx, "checkpoint: a keep failed; its stripes are dropped", "stripes", stripes, "error", err)
		f.drop(ctx, DropFailed, stripes)
	}
}

// keep writes what a peer's keep carries, if this cache's own list ranks it
// for the window under the keep's code: every stripe it does not hold or
// write already, through the queue, at the keep's priority. It reports
// peer.ErrDropped when it writes nothing.
func (f *filler) keep(ctx context.Context, keep peer.Keep) error {
	stripes, err := f.parseKeep(keep)
	if err == nil && !f.bug("keep-unranked") {
		err = f.ranked(keep.Window, keep.Code)
	}
	if err != nil {
		sim.Probe(ctx, ProbeKeepRefused)
		f.count(func(stats *FillStats) { stats.Refused += uint64(len(keep.Items)) })
		return fmt.Errorf("%w: %w", peer.ErrDropped, err)
	}
	if sim.Buggify(ctx, buggifyKeepDrop, 0.05) {
		sim.Probe(ctx, ProbeKeepDropped)
		f.drop(ctx, DropDisk, len(stripes))
		return fmt.Errorf("%w: its write budget is spent", peer.ErrDropped)
	}
	missing, bytes := f.markWriting(ctx, stripes)
	if len(missing) == 0 {
		return fmt.Errorf("%w: the cache holds every stripe", peer.ErrDropped)
	}
	unmark := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range missing {
			delete(f.writing, stripeKey{key: s.key, code: codeOf(s.stripe)})
		}
	}
	if !f.reserve(ctx, bytes) {
		unmark()
		f.drop(ctx, DropQueue, len(missing))
		return fmt.Errorf("%w: the queue is full", peer.ErrDropped)
	}
	kind := WriteFillRead
	switch {
	case keep.Repair:
		kind = WriteRepair
	case keep.Publication:
		kind = WriteFillPublication
	}
	result := make(chan int, 1)
	f.writes.push(f.ctx, func(ctx context.Context) {
		defer f.release(bytes)
		defer unmark()
		result <- f.write(ctx, kind, missing)
	})
	select {
	case written := <-result:
		if written == 0 {
			return fmt.Errorf("%w: the disk refused it", peer.ErrDropped)
		}
		sim.Probe(ctx, ProbeKeepKept)
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// markWriting marks the stripes of a keep the cache neither holds nor writes as
// being written, and reports them and their bytes. The rest are duplicates.
func (f *filler) markWriting(ctx context.Context, stripes []keyedStripe) ([]keyedStripe, int64) {
	var missing []keyedStripe
	var bytes int64
	for _, s := range stripes {
		named := stripeKey{key: s.key, code: codeOf(s.stripe)}
		duplicate := f.disk.holdsStripe(named.key, named.code)
		f.mu.Lock()
		if duplicate || f.writing[named] && !f.bug("keep-while-writing") {
			f.stats.Duplicates++
			f.mu.Unlock()
			sim.Probe(ctx, ProbeKeepDuplicate)
			continue
		}
		f.writing[named] = true
		f.mu.Unlock()
		missing = append(missing, s)
		bytes += int64(len(s.stripe.Bytes))
	}
	return missing, bytes
}

// parseKeep reads the stripes a keep carries, each an item as the disk stores
// it, and checks each against what the keep says of it: its window, its page,
// its index, its code and its envelope's length, and its own checksum.
func (f *filler) parseKeep(keep peer.Keep) ([]keyedStripe, error) {
	if !validWindow(keep.Window, nil) || keep.Code.Validate() != nil {
		return nil, fmt.Errorf("%w: window %+v under %s", errKeepRefused, keep.Window, keep.Code)
	}
	stripes := make([]keyedStripe, 0, len(keep.Items))
	offset := 0
	for _, item := range keep.Items {
		if item.Size < 0 || offset+item.Size > len(keep.Payload) {
			return nil, fmt.Errorf("%w: an item of %d bytes past the payload's %d", errKeepRefused, item.Size,
				len(keep.Payload))
		}
		parsed, err := parseItem(keep.Payload[offset:offset+item.Size], false)
		offset += item.Size
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errKeepRefused, err)
		}
		s := parsed.stripe()
		if parsed.key.rankWindow() != keep.Window || parsed.key.Page != keep.Window.Page(item.Page) ||
			s.Index != item.Index || s.Code != keep.Code || s.Length != item.Length || !storableStripe(s) {
			return nil, fmt.Errorf("%w: an item of %+v is not what the keep says it is", errKeepRefused, parsed.key)
		}
		// The payload is the peer server's buffer, given back once the keep
		// is answered, and the write may outlive the answer.
		s.Bytes = bytes.Clone(s.Bytes)
		stripes = append(stripes, keyedStripe{key: parsed.key, stripe: s})
	}
	if offset != len(keep.Payload) {
		return nil, fmt.Errorf("%w: %d bytes past its items", errKeepRefused, len(keep.Payload)-offset)
	}
	return stripes, nil
}

// ranked checks that this cache's own list ranks it for window, under code,
// and that the window is inside the share. Every stripe the cache writes for
// the cluster, its own fills' and its peers' keeps, is held to it.
func (f *filler) ranked(window rank.Window, code rank.Code) error {
	list, ok := f.disk.list()
	switch {
	case !ok || !window.InShare(f.disk.clusterPercent):
		return fmt.Errorf("%w: the cluster cache is not on for the window", errKeepRefused)
	case list.Code() != code:
		return fmt.Errorf("%w: the list's code is %s, not %s", errKeepRefused, list.Code(), code)
	case !slices.ContainsFunc(list.Ranks(window), func(cache rank.Cache) bool {
		return cache.Identity == f.disk.identity
	}):
		return fmt.Errorf("%w: the list does not rank this cache for the window", errKeepRefused)
	}
	return nil
}

// tokenBucket is a rate of bytes per second with a burst of one second of it,
// on a clock.
type tokenBucket struct {
	clock platform.Clock
	rate  float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newTokenBucket(clock platform.Clock, bytesPerSecond int64) tokenBucket {
	return tokenBucket{clock: clock, rate: float64(bytesPerSecond), tokens: float64(bytesPerSecond),
		last: clock.Now()}
}

// take takes bytes if the bucket holds them, and reports whether it did.
func (b *tokenBucket) take(bytes int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.rate, b.tokens+b.rate*elapsed.Seconds())
		b.last = now
	}
	if float64(bytes) > b.tokens {
		return false
	}
	b.tokens -= float64(bytes)
	return true
}

// repair hands over the stripes of a window a read rebuilt that holder lacks:
// indices no rank holds, for a rank that holds fewer than the code puts on it.
// A repair is a fill of the lowest priority. It takes room in the queue, goes
// behind every fill handed over before it, and is sent within the rate and
// the background budget at the repair priority, or written to this host's own
// disk at it; one that finds any of them without room is dropped, never
// queued.
func (f *filler) repair(window rank.Window, code rank.Code, holder rank.Cache, stripes []keyedStripe) {
	ctx := f.ctx
	bytes := int64(0)
	for _, s := range stripes {
		bytes += int64(len(s.stripe.Bytes))
	}
	if !f.reserve(ctx, bytes) {
		f.drop(ctx, DropQueue, len(stripes))
		return
	}
	f.fills.push(ctx, func(ctx context.Context) {
		defer f.release(bytes)
		if ctx.Err() != nil {
			f.drop(ctx, DropFailed, len(stripes))
			return
		}
		if holder.Identity == f.disk.identity {
			f.written(func(ctx context.Context) { f.writeOwn(ctx, WriteRepair, window, code, stripes) })
			return
		}
		f.send(ctx, WriteRepair, window, code, holder, stripes)
	})
}

// tell sends a holder a drop of a stripe a read found wrong, behind the fills,
// and reports whether it was handed over. Nothing waits for its answer.
func (f *filler) tell(holder rank.Cache, drop peer.Drop) bool {
	if f.peers == nil {
		return false
	}
	return f.behind(func(ctx context.Context) {
		if err := f.peers.Peer(holder.Address).Drop(ctx, holder.Identity, drop); err != nil {
			slog.DebugContext(ctx, "checkpoint: a holder could not be told to drop a wrong stripe", "cache",
				holder.Identity, "window", drop.Window, "page", drop.Page, "index", drop.Index, "error", err)
		}
	})
}

// behind does work behind the fills handed over before it, by the worker of
// fills, and reports whether it was handed over: none is once the fills have
// closed. It takes no room in the queue; it is a small request nothing waits
// on, and work left when the fills close is dropped.
func (f *filler) behind(work func(context.Context)) bool {
	f.mu.Lock()
	held := f.hold()
	f.mu.Unlock()
	if !held {
		return false
	}
	f.fills.push(f.ctx, func(ctx context.Context) {
		defer f.done()
		if ctx.Err() != nil {
			return
		}
		work(ctx)
	})
	return true
}
