package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// Filling the cluster. Inside the share the cluster cache is turned on for,
// three things bring a window to the cluster: a read of the store, a
// publication and a pull. Each hands the envelopes it has in hand to the
// cache's filler, which splits them under the membership's code and puts each
// stripe on the disk the membership ranks for it: a stripe of a disk this host
// keeps on that disk, and every other as a keep to the member that serves its
// disk.
// Every keep, fill right and drop names the generation it was placed under; a
// holder ahead of it answers stale, and the filler reads the membership and
// sends it again under the newer generation.
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
// A fault and a pull never wait on a fill. The filler holds the fills handed
// to it in one queue, bounded in bytes. One worker decides them in the order
// they were handed over, each fill's holders in rank order: it asks a read's
// fill right, takes each keep's bytes of a rate of bytes per second and its
// room in this host's background budget, and hands each holder's stripes to
// the lane of the member that serves the holder's disk, or to the lane of
// this host's own disks. The lanes carry their keeps side by side, each one
// keep at a time, in the order they were decided (see fillsend.go). Every
// write to this host's own disk, its own fills' and its peers' keeps', goes
// through one queue of writes drained by one worker. A read's fill that finds
// the queue full, the rate spent or the budget without room is dropped, and
// its window is read from the store the next time.
//
// A publication's fills are paced rather than dropped. A suspend or a stop is
// followed by a restore elsewhere, and a window it did not fill is read from
// the store there, which costs the restore more than a slower publication
// costs. So a publication hands a window over only once the queue holds less
// than its high-water mark, three quarters of its bound, and waits for room
// until then; and a keep of a publication's that finds the rate spent, the
// background budget full or its holder BUSY is tried again later. The
// publication's parts are held under their upload slots until they are handed
// over, so the publication slows to the pace its keeps go at. A window taken
// into the queue holds a copy of its envelopes, not its part, so the queue's
// bytes are what the fills hold. A read's fill never waits behind a
// publication's: the last quarter of the queue is left to reads, repairs and
// peers' keeps, a quarter of the rate's burst and of the background budget is
// left to reads, the worker takes a read's fill before any publication's
// still to do, between one holder of a publication's fill and the next, and
// each lane carries a read's keep before the publication's behind the one on
// the wire. No wait is longer than a bound: a publication that has waited it
// out once waits no more, and each of its fills that finds no room after is
// dropped as a read's is.
//
// The order is a decision, not a convenience. Keeps decided on goroutines of
// their own would reach the rate, the background budget and a peer's link in
// whatever order the Go scheduler ran them: which keep a dropped frame or a
// partition takes, and which finds the budget full, would be the scheduler's
// choice rather than the order the fills were handed over in. So one worker
// decides, and each holder's keeps go on its link one at a time in the order
// decided; only holders' lanes, which share nothing a keep could find full,
// run beside each other.
//
// A cold burst would fill one window many times: many hosts miss it at once,
// each reads the store, and each would send its stripes. So a read's fill needs
// the window's fill right, which the cache ranked first for the window hands
// out once per window per interval, and only while it holds nothing of the
// pages asked for. A reader asks for it with a read of stripes that wants no
// bytes; a reader that is not given it sends nothing. A publication and a pull
// need no right: each is the only one of its kind for its window.
//
// The cache takes a keep only for a window the membership ranks its disk for,
// under the membership's code, at the generation the keep names, and drops
// every stripe of it that it already holds or is already writing.

// Defaults of a cache's fills.
const (
	// DefaultFillQueueBytes bounds the host's queue of writes to its own disk.
	// A larger queue publishes no faster once the rate binds, and costs the
	// publisher several times its bytes in memory
	// (docs/measurements/gce-fill-defaults-2026-10-06.md).
	DefaultFillQueueBytes = 64 << 20
	// DefaultFillBytesPerSecond is the rate of keeps a host sends its peers,
	// with a burst of one second of it. It is the fastest rate measured that
	// kept the faults on a publishing host as fast as at 128 MiB/s; at
	// 256 MiB/s and above their p99 was that of unpaced fills
	// (docs/measurements/gce-fill-defaults-2026-10-06.md).
	DefaultFillBytesPerSecond = 192 << 20
	// DefaultFillRightInterval is how long a window's fill right, once given,
	// is not given again.
	DefaultFillRightInterval = 10 * time.Second
	// DefaultFillWaitBound is the longest one wait of a publication's fills
	// lasts. It is longer than a dead peer takes to be marked down, after
	// which the keeps to it are dropped at once.
	DefaultFillWaitBound = 10 * time.Second
	// DefaultFillKeepsInFlight is how many of its publications' keeps and
	// writes a host has on their way at once.
	DefaultFillKeepsInFlight = 16
)

// A keep of a publication's that a busy holder or a full background budget
// refused is tried again after fillRetryFirst, and twice as long after each
// refusal after, up to fillRetryMost.
const (
	fillRetryFirst = 10 * time.Millisecond
	fillRetryMost  = 500 * time.Millisecond
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
	// DropStale is a holder that does not serve the disk at its address, or
	// a disk no member serves, or a holder on another generation that a
	// newer one did not settle: the membership held is stale.
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
	// this cache refused: for a window the membership does not rank it for, under
	// another code, or that do not hold together.
	Duplicates, Refused uint64
	// Queued is the bytes of the queue held now, QueueBytes its bound, and
	// QueuedPeak the most it has held.
	Queued, QueueBytes, QueuedPeak int64
	// Waits counts the waits of publications' fills: for room in the queue,
	// and a keep's for the rate, the background budget or a busy holder.
	// Waited is how long they waited in all, and GaveUp counts the
	// publications that waited out the bound and waited no more.
	Waits  uint64
	Waited time.Duration
	GaveUp uint64
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
	// ProbeFillRanksChanged is a fill placed by ranks other than the ones
	// its right was asked under.
	ProbeFillRanksChanged = "checkpoint/fill-ranks-changed"
	// ProbeKeepKept is a keep a cache wrote.
	ProbeKeepKept = "checkpoint/keep-kept"
	// ProbeKeepDuplicate is a stripe of a keep the cache held or was writing.
	ProbeKeepDuplicate = "checkpoint/keep-duplicate"
	// ProbeKeepRefused is a keep for a window the membership does not rank
	// the cache's disk for.
	ProbeKeepRefused = "checkpoint/keep-refused"
	// ProbeKeepDropped is a keep a cache dropped as one whose write budget is
	// spent does.
	ProbeKeepDropped = "checkpoint/keep-dropped"
	// ProbeFillPublicationWaited is a publication's fill that waited: for room
	// in the queue, or its keep for the rate, the budget or a busy holder.
	ProbeFillPublicationWaited = "checkpoint/fill-publication-waited"
	// ProbeFillPublicationGaveUp is a publication that waited out the bound
	// and waits no more.
	ProbeFillPublicationGaveUp = "checkpoint/fill-publication-gave-up"
)

// The fault-injection sites of fills.
const (
	// buggifyFillQueueFull has the queue report itself full.
	buggifyFillQueueFull = "checkpoint/fill-queue-full"
	// buggifyFillLoseRight loses the answer that carried a fill right.
	buggifyFillLoseRight = "checkpoint/fill-lose-right"
	// buggifyFillRanksChange places a fill by its ranks less one of the
	// disks, as ranks that changed between the read and the fill.
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
	cluster *cluster
	peers   *peer.Table
	clock   platform.Clock
	// waits is what a publication's fills wait on: the table of peers' clock,
	// which its requests are timed by, or clock where there is no table.
	waits platform.Clock
	// ctx is the fills' life, which carries what the cache was made under;
	// Close ends it.
	ctx    context.Context
	cancel context.CancelFunc
	group  sync.WaitGroup

	queueBytes    int64
	rightInterval time.Duration
	waitBound     time.Duration
	keepsInFlight int
	rate          tokenBucket

	// down reports a holder the cache's reads have marked down, which is sent
	// no fill. Nil marks none.
	down func(rank.Identity) bool

	// fills is the fills handed over, which one worker decides one holder at
	// a time: it asks the right, and hands each holder's stripes to the lane
	// of the member that serves the holder's disk, or to the lane of this
	// host's own disks. writes is the writes to this host's own disk, its
	// fills' and its peers' keeps', which another worker does one at a time.
	// The two are apart because a keep waits for its holder's writes, and a
	// holder's writes must never wait for that holder's own keeps.
	fills  *fillLanes
	writes *lane

	mu sync.Mutex
	// queued is the bytes of the queue held: by fills handed over and not yet
	// done, and by peers' keeps not yet written.
	queued int64
	// waiting is the publications' windows waiting for room below the
	// high-water mark, in the order they began to wait, which is the order
	// room is given in.
	waiting []*roomWaiter
	// lanes is the holders' lanes that hold jobs, by the address of the member
	// that serves their disks, and ownLane for this host's own disks. paced
	// counts the publications' jobs on every lane, which keepsInFlight bounds,
	// and pacedBytes what their keeps hold of the background budget.
	lanes      map[platform.Address]*holderLane
	paced      int
	pacedBytes int64
	// busy counts the work held and the requests in flight; idle is closed
	// and replaced each time it falls to zero.
	busy int
	idle chan struct{}
	// writing is the stripes the queue holds for a keep, by the disk they
	// go on, which a stripe of another fill or keep is a duplicate of.
	writing map[writingKey]bool
	// granted is when each window's right was last given out, and grants the
	// windows in the order they were, which is the order they lapse in.
	granted map[rank.Window]time.Time
	grants  []grant
	stats   FillStats
	closed  bool
}

// writingKey is one stripe the queue holds for the disk it goes on.
type writingKey struct {
	disk rank.Identity
	stripeKey
}

// grant is one fill right given out.
type grant struct {
	window rank.Window
	at     time.Time
}

// roomWaiter is one window of a publication's waiting for room in the queue.
// granted says the room was taken for it, under the filler's lock, before
// ready was closed; ready closed without it is a filler that closed.
type roomWaiter struct {
	bytes   int64
	ready   chan struct{}
	granted bool
}

// fillSettings is how a cache fills the cluster.
type fillSettings struct {
	peers                      *peer.Table
	clock                      platform.Clock
	queueBytes, bytesPerSecond int64
	rightInterval, waitBound   time.Duration
	keepsInFlight              int
}

// newFiller starts a cache's fills under ctx.
func newFiller(ctx context.Context, shared *cluster, settings fillSettings) *filler {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	clock := platform.ClockOr(settings.clock)
	waits := clock
	if settings.peers != nil {
		waits = settings.peers.Clock()
	}
	f := &filler{cluster: shared, peers: settings.peers, clock: clock, waits: waits, ctx: ctx, cancel: cancel,
		queueBytes: settings.queueBytes, rightInterval: settings.rightInterval, waitBound: settings.waitBound,
		keepsInFlight: settings.keepsInFlight, rate: newTokenBucket(clock, settings.bytesPerSecond),
		fills: &fillLanes{wake: make(chan struct{}, 1)}, writes: newLane(), lanes: make(map[platform.Address]*holderLane),
		idle: make(chan struct{}), writing: make(map[writingKey]bool), granted: make(map[rank.Window]time.Time)}
	f.group.Go(func() { f.runFills(ctx) })
	f.group.Go(func() { f.writes.run(ctx) })
	return f
}

// close stops the fills: both workers, the right asked for, the lanes and
// their keeps in flight, and the publications waiting for room. What it had
// not done is dropped.
func (f *filler) close() {
	f.mu.Lock()
	f.closed = true
	for _, w := range f.waiting {
		close(w.ready)
	}
	f.waiting = nil
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
// dropped: a read's fill, a pull's, a repair and a peer's keep never wait for
// room. One piece larger than the whole queue is taken alone.
func (f *filler) reserve(ctx context.Context, bytes int64) bool {
	if sim.Buggify(ctx, buggifyFillQueueFull, 0.05) {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queued > 0 && f.queued+bytes > f.queueBytes {
		return false
	}
	return f.take(bytes)
}

// take takes bytes of the queue and counts the work held, and reports false
// once the fills have closed. Caller holds f.mu.
func (f *filler) take(bytes int64) bool {
	if !f.hold() {
		return false
	}
	f.queued += bytes
	f.stats.QueuedPeak = max(f.stats.QueuedPeak, f.queued)
	return true
}

// highWater is what the queue may hold, of a publication's fills and the
// rest together, before a publication's next window waits for room: three
// quarters of its bound. The last quarter is left to the work that never
// waits.
func (f *filler) highWater() int64 { return f.queueBytes - f.queueBytes/4 }

// fitsPaced reports room below the high-water mark for a window of bytes of a
// publication's. One larger than the mark is taken alone. Caller holds f.mu.
func (f *filler) fitsPaced(bytes int64) bool {
	return f.queued == 0 || f.queued+bytes <= f.highWater()
}

// room takes room in the queue for a window of bytes of a publication's,
// waiting under ctx, at most the bound, for the queue to fall below its
// high-water mark. Windows that wait are given room in the order they began
// to, and none is given room ahead of one already waiting. A publication that
// waited out the bound once waits no more. It reports why the window is
// dropped when it takes no room.
func (f *filler) room(ctx context.Context, pace *fillPace, bytes int64) (DropReason, bool) {
	if f.bug("fill-publication-dropped-when-full") {
		// The bug drops a publication's window as a read's is dropped.
		if f.reserve(f.ctx, bytes) {
			return 0, true
		}
		return DropQueue, false
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return DropFailed, false
	}
	if len(f.waiting) == 0 && f.fitsPaced(bytes) {
		f.take(bytes)
		f.mu.Unlock()
		return 0, true
	}
	if !pace.waits() {
		f.mu.Unlock()
		return DropQueue, false
	}
	w := &roomWaiter{bytes: bytes, ready: make(chan struct{})}
	f.waiting = append(f.waiting, w)
	f.stats.Waits++
	f.mu.Unlock()
	sim.Probe(f.ctx, ProbeFillPublicationWaited)
	began := f.waits.Now()
	var bound <-chan time.Time
	if !f.bug("fill-publication-waits-forever") {
		timer := f.waits.NewTimer(f.waitBound)
		defer timer.Stop()
		bound = timer.C()
	}
	timedOut := false
	select {
	case <-w.ready:
	case <-bound:
		timedOut = true
	case <-ctx.Done():
	}
	f.mu.Lock()
	f.stats.Waited += f.waits.Since(began)
	if w.granted {
		f.mu.Unlock()
		return 0, true
	}
	closed := f.closed
	if !closed {
		f.waiting = slices.DeleteFunc(f.waiting, func(other *roomWaiter) bool { return other == w })
		// The window gone may have been all that held back the next one.
		f.giveRoom()
	}
	f.mu.Unlock()
	if !timedOut || closed {
		return DropFailed, false
	}
	f.giveUp(pace)
	return DropQueue, false
}

// giveRoom gives room to the windows waiting for it, from the first, while each
// fits below the high-water mark. Caller holds f.mu.
func (f *filler) giveRoom() {
	for len(f.waiting) > 0 && f.fitsPaced(f.waiting[0].bytes) {
		w := f.waiting[0]
		if !f.take(w.bytes) {
			return
		}
		f.waiting = f.waiting[1:]
		w.granted = true
		close(w.ready)
	}
}

// giveUp is a publication that waited out the bound: it waits no more, and
// says so once.
func (f *filler) giveUp(pace *fillPace) {
	if !pace.giveUp() {
		return
	}
	f.count(func(stats *FillStats) { stats.GaveUp++ })
	sim.Probe(f.ctx, ProbeFillPublicationGaveUp)
	slog.WarnContext(f.ctx, "checkpoint: a publication waited out its bound for its fills; what of it finds no "+
		"room now is dropped", "bound", f.waitBound)
}

// release gives back what reserve, room or giveRoom took, once the work is done
// or dropped, and gives the room to the publications waiting for it.
func (f *filler) release(bytes int64) {
	f.mu.Lock()
	f.queued -= bytes
	if !f.closed {
		f.giveRoom()
	}
	f.mu.Unlock()
	f.done()
}

// fillPace is one publication's waits for its fills. A wait that reaches the
// bound gives up the publication's waits: from then on each of its fills
// that finds no room in the queue, the rate spent, the background budget full
// or its holder busy is dropped, as a read's is. The publication's uploads,
// which hand its parts over one after another, share it.
type fillPace struct {
	mu     sync.Mutex
	gaveUp bool
}

// waits reports whether the publication still waits for its fills.
func (p *fillPace) waits() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.gaveUp
}

// giveUp has the publication wait no more, and reports whether this call
// gave up.
func (p *fillPace) giveUp() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	gave := !p.gaveUp
	p.gaveUp = true
	return gave
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

// fillLanes is the work the worker of fills does, one piece at a time. The
// prompt work goes first, in the order it was handed over: the fills of reads
// and pulls, repairs and drops. The publications' fills go behind it, in the
// order they were handed over, one holder at a time. A publication's fill
// whose next holder must wait, for the rate, the background budget or room
// on its lane, leaves the worker to the prompt work until it is tried again.
type fillLanes struct {
	mu     sync.Mutex
	prompt []func(context.Context)
	paced  []*pacedFill
	// wake tells an idle worker of more, and stopped says the worker has
	// returned.
	wake    chan struct{}
	stopped bool
}

// pacedFill is one window of a publication's on its way: its fill, the
// publication's pace, and how far its placement has got. Only the worker of
// fills reads and writes it.
type pacedFill struct {
	fill *windowFill
	pace *fillPace
	// placed is the fill split and placed, nil before its first step.
	placed *placement
	// resume is when it is tried again after a keep that must wait; zero
	// goes on at once. blocked says its next holder waits for room on its
	// lane, or among the keeps in flight, which a job's end gives.
	resume  time.Time
	blocked bool
	// wait is the wait of the keep of its next holder for the rate or the
	// budget; rated says that keep has taken its bytes of the rate; and keep
	// is that keep, once built.
	wait  keepWait
	rated bool
	keep  *peer.Keep
}

// pushPrompt hands prompt work to the worker of fills. Once the worker has
// returned, the work runs here under ended, the fills' ended context, which
// drops it and gives its room back.
func (l *fillLanes) pushPrompt(ended context.Context, work func(context.Context)) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		work(ended)
		return
	}
	l.prompt = append(l.prompt, work)
	l.mu.Unlock()
	l.tell()
}

// tell wakes the worker if it is idle.
func (l *fillLanes) tell() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// promptWaiting reports prompt work handed over and not yet taken.
func (l *fillLanes) promptWaiting() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prompt) > 0
}

// pushPaced hands a publication's window, whose room in the queue it holds, to
// the worker of fills. Once the worker has returned, it is dropped here.
func (f *filler) pushPaced(paced *pacedFill) {
	l := f.fills
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		for !f.stepPaced(f.ctx, paced) {
		}
		return
	}
	l.paced = append(l.paced, paced)
	l.mu.Unlock()
	l.tell()
}

// runFills is the worker of fills: it does the prompt work first, and a
// publication's fill whenever none is waiting and the fill is not waiting
// itself, until ctx ends and both are empty. What is left when ctx ends runs
// under it, and ends at once.
func (f *filler) runFills(ctx context.Context) {
	l := f.fills
	// The bug takes the publications' fills first, so a read's fill waits
	// behind every one of them still to do.
	behind := f.bug("fill-reads-behind-publications")
	for {
		l.mu.Lock()
		var head *pacedFill
		var wait time.Duration
		ready := false
		if len(l.paced) > 0 {
			head = l.paced[0]
			wait = head.resume.Sub(f.waits.Now())
			ready = !head.blocked && wait <= 0 || ctx.Err() != nil
		}
		var next func(context.Context)
		switch {
		case len(l.prompt) > 0 && !(behind && ready):
			next = l.prompt[0]
			l.prompt = l.prompt[1:]
		case ready:
		case head == nil && ctx.Err() != nil:
			l.stopped = true
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
		switch {
		case next != nil:
			next(ctx)
		case ready:
			if f.stepPaced(ctx, head) {
				l.mu.Lock()
				l.paced = l.paced[1:]
				l.mu.Unlock()
			}
		default:
			if head != nil && head.blocked && f.bug("fill-keeps-past-a-blocked-fill") && f.stepPast(ctx) {
				continue
			}
			f.idleFills(ctx, head != nil && !head.blocked, wait)
			// Whatever woke the worker may have been a job's end, which gives
			// a blocked fill the room it waits for: it looks again.
			l.mu.Lock()
			for _, paced := range l.paced {
				paced.blocked = false
			}
			l.mu.Unlock()
		}
	}
}

// stepPast is the bug that goes past a publication's fill whose next holder
// waits for room on its lane, and decides the holders of the fills behind it,
// so a holder can receive a later fill's keep before an earlier one's. It
// reports whether it stepped a fill.
func (f *filler) stepPast(ctx context.Context) bool {
	l := f.fills
	l.mu.Lock()
	var next *pacedFill
	for _, paced := range l.paced[1:] {
		if !paced.blocked && !paced.resume.After(f.waits.Now()) {
			next = paced
			break
		}
	}
	l.mu.Unlock()
	if next == nil {
		return false
	}
	if f.stepPaced(ctx, next) {
		l.mu.Lock()
		l.paced = slices.DeleteFunc(l.paced, func(paced *pacedFill) bool { return paced == next })
		l.mu.Unlock()
	}
	return true
}

// idleFills waits for more work, or a job's end, for the first publication's
// fill to be tried again when one is waiting for a time, or for ctx to end.
func (f *filler) idleFills(ctx context.Context, waiting bool, wait time.Duration) {
	var resume <-chan time.Time
	if waiting {
		timer := f.waits.NewTimer(wait)
		defer timer.Stop()
		resume = timer.C()
	}
	select {
	case <-f.fills.wake:
	case <-resume:
	case <-ctx.Done():
	}
}

// stepPaced does what it can of a publication's fill: it splits it on its
// first step, then decides one holder after another, letting prompt work go
// first between them. It reports whether every holder of the fill is decided;
// one that is not waits for its resume, for room on its next holder's lane,
// or for the prompt work. The fill gives its room in the queue back once its
// last holder's job has ended.
func (f *filler) stepPaced(ctx context.Context, paced *pacedFill) bool {
	if paced.placed == nil {
		if paced.placed = f.split(ctx, WriteFillPublication, paced.fill); paced.placed == nil {
			f.release(paced.fill.bytes)
			return true
		}
		paced.placed.room = paced.fill.bytes
	}
	p := paced.placed
	for !p.decided() {
		wait, blocked := f.decide(ctx, p, paced)
		if blocked {
			paced.blocked = true
			return false
		}
		if wait > 0 {
			paced.resume = f.waits.Now().Add(wait)
			return false
		}
		paced.resume = time.Time{}
		if !p.decided() && ctx.Err() == nil && f.fills.promptWaiting() {
			return false
		}
	}
	f.seal(p)
	return true
}

// written does work on the queue of writes to this host's own disk, and
// returns once it has: the lane of this host's own disks waits for its writes
// there, behind its peers' keeps.
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

// windowFill is the envelopes of one window a fill hands over, and the
// membership it was handed over under.
type windowFill struct {
	window    rank.Window
	m         membership.Membership
	envelopes []envelope
	bytes     int64
}

// windows groups envelopes by the window they are in, in the order they come,
// keeping only the windows inside the share that the cache places by its
// membership.
func (f *filler) windows(envelopes []envelope) []*windowFill {
	var fills []*windowFill
	at := make(map[rank.Window]*windowFill)
	for _, e := range envelopes {
		window := e.key.rankWindow()
		fill := at[window]
		if fill == nil {
			m, ok := f.cluster.placedBy(e.key, false)
			if !ok {
				continue
			}
			fill = &windowFill{window: window, m: m}
			at[window] = fill
			fills = append(fills, fill)
		}
		fill.envelopes = append(fill.envelopes, e)
		fill.bytes += int64(len(e.data))
	}
	return fills
}

// inShare reports whether the cache places key's window by its membership:
// inside the share, on a host that follows a membership. Such a window is the
// fills', and nothing writes it to a disk but them.
func (f *filler) inShare(key diskKey) bool {
	_, ok := f.cluster.placedBy(key, false)
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
		stripes := len(fill.envelopes) * fill.m.Code().Width()
		if !f.reserve(ctx, fill.bytes) {
			f.drop(ctx, DropQueue, stripes)
			continue
		}
		if kind == WriteFillRead && !f.bug("no-fill-right") && f.bug("fill-concurrently") {
			f.rightBeside(kind, fill)
			continue
		}
		// The right is asked for behind the read, never in front of it; the
		// fill holds its room in the queue until its holders' jobs are done.
		f.fills.pushPrompt(ctx, func(ctx context.Context) { f.do(ctx, kind, fill) })
	}
}

// publish hands what a publication made durable to the cluster, each window
// inside the share to its ranks, in the order the envelopes come. It returns
// once every window is handed over or dropped: each waits under ctx for room
// below the queue's high-water mark, as pace allows. A publication's fill
// needs no right.
//
// A window taken into the queue is given bytes of its own. Its envelopes are
// views of their part, and a window holding them would hold the whole part
// until the window's last keep was answered: the queue would count a window
// and the host hold the part, as many parts as the queue held windows of.
// Copied, a window holds what the queue counts of it, and the part is let go
// once its last window is handed over.
func (f *filler) publish(ctx context.Context, pace *fillPace, envelopes []envelope) {
	for _, fill := range f.windows(envelopes) {
		if reason, ok := f.room(ctx, pace, fill.bytes); !ok {
			f.drop(f.ctx, reason, len(fill.envelopes)*fill.m.Code().Width())
			continue
		}
		if !f.bug("fill-windows-hold-their-parts") {
			fill.own()
		}
		f.pushPaced(&pacedFill{fill: fill, pace: pace})
	}
}

// own copies the fill's envelopes into one buffer of its own, of exactly the
// fill's bytes, in their order.
func (fill *windowFill) own() {
	buffer := make([]byte, 0, fill.bytes)
	for at := range fill.envelopes {
		start := len(buffer)
		buffer = append(buffer, fill.envelopes[at].data...)
		fill.envelopes[at].data = buffer[start:]
	}
}

// do is one fill, done by the worker of fills: the right a read's fill
// needs, then the fill placed. The fill holds room in the queue, which it
// gives back once it is placed, or now when it is not.
func (f *filler) do(ctx context.Context, kind WriteKind, fill *windowFill) {
	// A fill under ended fills asks nothing, and place drops it.
	if ctx.Err() == nil && kind == WriteFillRead && !f.bug("no-fill-right") && !f.right(ctx, fill) {
		f.count(func(stats *FillStats) { stats.WithoutRight++ })
		f.release(fill.bytes)
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
		f.fills.pushPrompt(f.ctx, func(ctx context.Context) { f.place(ctx, kind, fill) })
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

// right asks the disk ranked first for fill's window for its fill right: of
// a disk this host keeps, or of the member that serves it by a read of the
// window's stripes that wants no bytes, under the fill's generation. A rank 1
// ahead of it answers stale, and the right is asked again of rank 1 under the
// newer generation. A disk that cannot be asked gives none.
func (f *filler) right(ctx context.Context, fill *windowFill) bool {
	granted := false
	for tries := 0; ; tries++ {
		ranks := fill.m.List().Ranks(fill.window)
		if len(ranks) == 0 {
			return false
		}
		first := ranks[0]
		if disk, release, kept := f.cluster.hold(first.Identity); kept {
			granted = disk.owns(fill.m) && f.grant(ctx, disk, fill.m, fill.window, fill.pages(), fill.m.Code())
			release()
			break
		}
		if f.peers == nil {
			return false
		}
		route, routed := fill.m.Route(first.Identity)
		if !routed {
			return false
		}
		reply, err := f.peers.Peer(route.Address).ReadStripes(peer.WithClass(ctx, peer.BulkRead), route,
			peer.StripeRead{Window: fill.window, Pages: fill.pages(), Code: fill.m.Code()})
		if err == nil {
			reply.Release()
			granted = reply.FillRight
			break
		}
		if newer, ok := f.newer(ctx, fill.m, err, tries); ok {
			fill.m = newer
			continue
		}
		slog.DebugContext(ctx, "checkpoint: rank 1 could not be asked for a fill right", "window", fill.window,
			"cache", first.Identity, "error", err)
		return false
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

// grant decides a fill right as disk, ranked first for window under m: given
// to the first reader that asks for it under m's code, while the disk holds
// nothing of the pages asked for, once per window per interval. Pages nil
// asks for every page of the window.
func (f *filler) grant(ctx context.Context, disk *cacheDisk, m membership.Membership, window rank.Window,
	pages []uint32, code rank.Code) bool {
	if !validWindow(window, pages) || !window.InShare(f.cluster.percent) || m.Code() != code {
		return false
	}
	ranks := m.List().Ranks(window)
	if len(ranks) == 0 || ranks[0].Identity != disk.identity || disk.holdsAnyOf(window, pages, code) {
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

// placement is one fill split under the membership it is placed by: each
// holder's stripes, the holders in rank order, and the next of them to
// decide. room is the bytes of the queue the fill holds, which it gives back
// once it is sealed, every holder decided, and none of its jobs is pending;
// the filler's lock guards those two.
type placement struct {
	kind   WriteKind
	m      membership.Membership
	window rank.Window
	code   rank.Code
	order  []rank.Cache
	held   map[rank.Identity][]keyedStripe
	next   int
	room   int64
	// pending counts the jobs of the placement on their way.
	pending int
	sealed  bool
}

// decided reports whether every holder of the placement is decided.
func (p *placement) decided() bool { return p.next == len(p.order) }

// place splits fill's envelopes under the membership held now and puts each
// stripe on the disk its ranks hold it on: those of a disk this host keeps on
// that disk, the rest as keeps to the members that serve theirs. It is the
// worker of fills', and decides every holder at once, in rank order: the fill
// of a read, a pull or a repair never waits for a lane. The fill holds its
// room in the queue until its holders' jobs have ended.
func (f *filler) place(ctx context.Context, kind WriteKind, fill *windowFill) {
	p := f.split(ctx, kind, fill)
	if p == nil {
		f.release(fill.bytes)
		return
	}
	p.room = fill.bytes
	for !p.decided() {
		f.decide(ctx, p, nil)
	}
	f.seal(p)
}

// split splits fill's envelopes under the membership held now, by the holder
// each stripe goes to, and counts the window filled. It reports nil for a
// fill with nothing to place: one under ended fills, which it drops, or of a
// window the cache no longer places by its membership.
func (f *filler) split(ctx context.Context, kind WriteKind, fill *windowFill) *placement {
	if ctx.Err() != nil {
		f.drop(ctx, DropFailed, len(fill.envelopes)*fill.m.Code().Width())
		return nil
	}
	m, ok := f.cluster.placedBy(fill.envelopes[0].key, false)
	if !ok {
		return nil
	}
	list := m.List()
	if sim.Buggify(ctx, buggifyFillRanksChange, 0.1) {
		for _, cache := range list.Ranks(fill.window) {
			if !f.cluster.keeps(cache.Identity) {
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
	p := &placement{kind: kind, m: m, window: fill.window, code: code, held: make(map[rank.Identity][]keyedStripe)}
	for _, e := range fill.envelopes {
		stripes, err := stripe.Split(code, e.data)
		if err != nil {
			slog.WarnContext(ctx, "checkpoint: an envelope did not split; the store serves it", "window", fill.window,
				"code", code, "error", err)
			f.drop(ctx, DropFailed, code.Width())
			continue
		}
		for index, holder := range holders {
			if _, seen := p.held[holder.Identity]; !seen {
				p.order = append(p.order, holder)
			}
			p.held[holder.Identity] = append(p.held[holder.Identity], keyedStripe{key: e.key, stripe: stripes[index]})
		}
	}
	// The stripes hold the envelopes' bytes now, and the fill lets its
	// envelopes go.
	fill.envelopes = nil
	return p
}

// writeOwn writes the stripes of a fill of window that a disk this host keeps
// holds, leaving out those it holds or is writing already. It is the worker
// of writes', so nothing writes between what it finds held and what it
// writes. The membership it was placed by may not be the one the host holds
// now, which is the one its own fills are held to, as a keep is to its own.
// A disk the host stopped keeping since is a stale placement.
func (f *filler) writeOwn(ctx context.Context, identity rank.Identity, kind WriteKind, window rank.Window,
	code rank.Code, stripes []keyedStripe) {
	disk, release, kept := f.cluster.hold(identity)
	if !kept {
		f.drop(ctx, DropStale, len(stripes))
		return
	}
	defer release()
	m, _ := f.cluster.following()
	if err := f.ranked(m, disk, window, code); err != nil && !f.bug("keep-unranked") {
		sim.Probe(ctx, ProbeKeepRefused)
		f.count(func(stats *FillStats) { stats.Refused += uint64(len(stripes)) })
		return
	}
	var missing []keyedStripe
	for _, s := range stripes {
		if f.held(disk, stripeKey{key: s.key, code: codeOf(s.stripe)}) {
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
	f.write(ctx, disk, kind, missing)
}

// held reports whether disk holds a stripe or the queue holds it for a keep
// of disk.
func (f *filler) held(disk *cacheDisk, s stripeKey) bool {
	f.mu.Lock()
	writing := f.writing[writingKey{disk: disk.identity, stripeKey: s}]
	f.mu.Unlock()
	return writing || disk.holdsStripe(s.key, s.code)
}

// write puts stripes on a disk this host keeps, the stripes of each envelope
// next to each other, and counts what it kept and what the disk refused. It
// reports how many it kept.
func (f *filler) write(ctx context.Context, disk *cacheDisk, kind WriteKind, stripes []keyedStripe) int {
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
		written, err := disk.writeStripes(ctx, stripes[start].key, group, kind)
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

// keepFor is a keep of one holder's stripes of p's window, each as the
// holder's disk stores it.
func keepFor(p *placement, stripes []keyedStripe) peer.Keep {
	size := int64(0)
	for _, s := range stripes {
		size += itemHeaderBytes(s.key) + int64(len(s.stripe.Bytes))
	}
	keep := peer.Keep{Window: p.window, Code: p.code, Publication: p.kind == WriteFillPublication,
		Repair: p.kind == WriteRepair, Items: make([]peer.StripeItem, 0, len(stripes)), Payload: make([]byte, 0, size)}
	for _, s := range stripes {
		start := len(keep.Payload)
		keep.Payload = appendItem(keep.Payload, s.key, s.stripe)
		keep.Items = append(keep.Items, peer.StripeItem{Page: uint32(s.key.Page - p.window.Page(0)),
			Index: s.stripe.Index, Length: s.stripe.Length, Size: len(keep.Payload) - start})
	}
	return keep
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
	case errors.Is(err, peer.ErrNotMe), errors.Is(err, peer.ErrStale), errors.Is(err, errNoRoute):
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

// keep writes what a peer's keep of disk carries, if m, the membership at the
// generation the keep named, ranks the disk for the window under the keep's
// code: every stripe it does not hold or write already, through the queue, at
// the keep's priority. It reports peer.ErrDropped when it writes nothing.
func (f *filler) keep(ctx context.Context, m membership.Membership, disk *cacheDisk, keep peer.Keep) error {
	stripes, err := f.parseKeep(keep)
	if err == nil && !f.bug("keep-unranked") {
		err = f.ranked(m, disk, keep.Window, keep.Code)
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
	missing, bytes := f.markWriting(ctx, disk, stripes)
	if len(missing) == 0 {
		return fmt.Errorf("%w: the cache holds every stripe", peer.ErrDropped)
	}
	unmark := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range missing {
			delete(f.writing, writingKey{disk: disk.identity, stripeKey: stripeKey{key: s.key, code: codeOf(s.stripe)}})
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
	// The write holds the disk until it is done, past the keep's answer
	// should its caller give up first: a shard is closed only once nothing
	// writes it.
	holder, release, kept := f.cluster.hold(disk.identity)
	if !kept {
		unmark()
		f.release(bytes)
		f.drop(ctx, DropStale, len(missing))
		return fmt.Errorf("%w: %w", peer.ErrDropped, ErrNotKept)
	}
	result := make(chan int, 1)
	f.writes.push(f.ctx, func(ctx context.Context) {
		defer f.release(bytes)
		defer unmark()
		defer release()
		result <- f.write(ctx, holder, kind, missing)
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

// markWriting marks the stripes of a keep of disk the cache neither holds nor
// writes as being written, and reports them and their bytes. The rest are
// duplicates.
func (f *filler) markWriting(ctx context.Context, disk *cacheDisk, stripes []keyedStripe) ([]keyedStripe, int64) {
	var missing []keyedStripe
	var bytes int64
	for _, s := range stripes {
		named := writingKey{disk: disk.identity, stripeKey: stripeKey{key: s.key, code: codeOf(s.stripe)}}
		duplicate := disk.holdsStripe(named.key, named.code)
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

// ranked checks that m has this host serve disk, ranks the disk for window
// under code, and that the window is inside the share. Every stripe the cache
// writes for the cluster, its own fills' and its peers' keeps, is held to it.
func (f *filler) ranked(m membership.Membership, disk *cacheDisk, window rank.Window, code rank.Code) error {
	switch {
	case !window.InShare(f.cluster.percent):
		return fmt.Errorf("%w: the cluster cache is not on for the window", errKeepRefused)
	case !disk.owns(m):
		return fmt.Errorf("%w: generation %d does not have this host serve disk %s", errKeepRefused,
			m.Generation(), disk.identity)
	case m.Code() != code:
		return fmt.Errorf("%w: the membership's code is %s, not %s", errKeepRefused, m.Code(), code)
	case !slices.ContainsFunc(m.List().Ranks(window), func(cache rank.Cache) bool {
		return cache.Identity == disk.identity
	}):
		return fmt.Errorf("%w: the membership does not rank this disk for the window", errKeepRefused)
	}
	return nil
}

// routed sends one request to the member that serves disk under m, and when
// that member answers that m is stale, reads the membership and sends it
// again under the newer generation. It reports errNoRoute for a disk no
// member serves.
func (f *filler) routed(ctx context.Context, m membership.Membership, disk rank.Identity,
	request func(membership.Route) error) error {
	for tries := 0; ; tries++ {
		route, ok := m.Route(disk)
		if !ok {
			return errNoRoute
		}
		err := request(route)
		newer, again := f.newer(ctx, m, err, tries)
		if !again {
			return err
		}
		m = newer
	}
}

// newer is the membership to send a request again under, after err answered
// one made under m on its tries'th try: a holder ahead of m said m is stale,
// and the membership read since is newer than m.
func (f *filler) newer(ctx context.Context, m membership.Membership, err error, tries int) (membership.Membership, bool) {
	var stale *peer.StaleError
	if !errors.As(err, &stale) || stale.Generation <= m.Generation() || tries == staleRetries ||
		f.bug("membership-ignore-stale-answer") {
		return membership.Membership{}, false
	}
	newer, catchErr := f.cluster.catch(ctx, stale.Generation)
	if catchErr != nil || newer.Generation() <= m.Generation() {
		return membership.Membership{}, false
	}
	sim.Probe(ctx, membership.ProbeSenderCaughtUp)
	return newer, true
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
	b.refill()
	if float64(bytes) > b.tokens {
		return false
	}
	b.tokens -= float64(bytes)
	return true
}

// after takes bytes for a keep of a publication's once the bucket holds them
// and a quarter of a second of the rate beside, which is left to the keeps of
// reads, and reports how long until it does when it does not now. A keep
// larger than three quarters of the burst is taken once the bucket is full,
// and leaves it in debt for the rest.
func (b *tokenBucket) after(bytes int64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	need := min(float64(bytes)+b.rate/4, b.rate)
	if b.tokens >= need {
		b.tokens -= float64(bytes)
		return 0
	}
	return max(time.Duration(math.Ceil((need-b.tokens)/b.rate*float64(time.Second))), 1)
}

// refill adds what the bucket's clock says has passed since the last refill,
// up to a second of the rate. Caller holds b.mu.
func (b *tokenBucket) refill() {
	now := b.clock.Now()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.rate, b.tokens+b.rate*elapsed.Seconds())
		b.last = now
	}
}

// repair hands over the stripes of a window a read rebuilt that holder lacks:
// indices no rank holds, for a rank that holds fewer than the code puts on it.
// A repair is a fill of the lowest priority. It takes room in the queue, goes
// behind every fill handed over before it, and is sent within the rate and
// the background budget at the repair priority, or written to this host's own
// disk at it; one that finds any of them without room is dropped, never
// queued.
func (f *filler) repair(m membership.Membership, window rank.Window, code rank.Code, holder rank.Cache,
	stripes []keyedStripe) {
	ctx := f.ctx
	bytes := int64(0)
	for _, s := range stripes {
		bytes += int64(len(s.stripe.Bytes))
	}
	if !f.reserve(ctx, bytes) {
		f.drop(ctx, DropQueue, len(stripes))
		return
	}
	f.fills.pushPrompt(ctx, func(ctx context.Context) {
		p := &placement{kind: WriteRepair, m: m, window: window, code: code, order: []rank.Cache{holder},
			held: map[rank.Identity][]keyedStripe{holder.Identity: stripes}, room: bytes}
		f.decide(ctx, p, nil)
		f.seal(p)
	})
}

// tell sends a holder a drop of a stripe a read found wrong under m, behind
// the fills, and reports whether it was handed over. Nothing waits for its
// answer.
func (f *filler) tell(m membership.Membership, holder rank.Cache, drop peer.Drop) bool {
	if f.peers == nil {
		return false
	}
	return f.behind(func(ctx context.Context) {
		if err := f.routed(ctx, m, holder.Identity, func(route membership.Route) error {
			return f.peers.Peer(route.Address).Drop(ctx, route, drop)
		}); err != nil {
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
	f.fills.pushPrompt(f.ctx, func(ctx context.Context) {
		defer f.done()
		if ctx.Err() != nil {
			return
		}
		work(ctx)
	})
	return true
}
