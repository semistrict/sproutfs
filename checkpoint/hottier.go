package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Reading through a hot tier. A hot tier is a second bucket that holds copies
// of checkpoint objects under the same names as the regional bucket: a zonal
// bucket in the hosts' zone, say. It is reached through the same object store
// interface and adapters, and uses no feature of any one cloud. It is an
// alternative to the cluster cache on the hosts' disks; a store refuses to
// have both.
//
// A read of a checkpoint object runs against the hot tier first, under a
// bound. A hit is the answer. A miss, an object the hot tier does not hold,
// runs the same read against the regional bucket and hands the object to the
// hot tier's fills behind the read. A failure does the same without the fill:
// a request that fails, a read past the bound, or bytes that do not make what
// the read wanted, such as a reply cut short. So a hot tier that is down,
// slow, lost with its zone or emptied only costs reads of the regional bucket,
// and never fails a read. Three failures in a row mark the hot tier down, and
// reads skip it for a while.
//
// Checkpoint objects are immutable and named by the checkpoint that wrote
// them, and a VM's starting epoch is drawn, so a name never stands for two
// contents. A fill is therefore a create-if-absent PUT of the regional
// object's bytes under its own name, with no version to check: two hosts that
// fill one object at once write the same bytes, one of them finds it there,
// and no copy is ever stale. A fill of an object a read missed GETs the whole
// object from the regional bucket first. A publication hands over each part
// once its regional PUT has succeeded, and its index object once that PUT
// has, with their bytes in hand.
//
// Nothing waits on a fill. One worker does the fills one at a time, in the
// order they were handed over, from one queue bounded in bytes, within a rate
// of bytes per second. A fill that finds the queue full or the rate spent is
// dropped, and the object is read from the regional bucket next time too. The
// regional bucket stays the only durable copy. The hot tier is never expired
// here: what a checkpoint's reclamation deletes stays in the hot tier.
//
// One worker is a decision, as it is for the cluster's fills: fills on
// goroutines of their own would reach the buckets in the order the Go
// scheduler ran them.

// Defaults of a hot tier.
const (
	// DefaultHotTierBound is how long a read waits for the hot tier before it
	// reads the regional bucket instead.
	DefaultHotTierBound = 500 * time.Millisecond
	// DefaultHotTierQueueBytes bounds the fills held.
	DefaultHotTierQueueBytes = 256 << 20
	// DefaultHotTierBytesPerSecond is the rate of fills, with a burst of one
	// second of it.
	DefaultHotTierBytesPerSecond = 128 << 20
)

const (
	// hotTierDownAfter is how many failures of the hot tier in a row mark it
	// down, and hotTierDownFor how long reads then skip it. The first read
	// after that tries it again.
	hotTierDownAfter = 3
	hotTierDownFor   = 10 * time.Second
	// maximumHotObject bounds an object a fill copies, which is what the
	// worker holds in memory while it does: any part, and every index object
	// but the very largest.
	maximumHotObject = maximumPartSize
)

// HotTierConfig is a hot tier: its bucket and how its fills are bounded.
type HotTierConfig struct {
	// Store is the hot tier's bucket. Its keys are the regional bucket's.
	Store platform.ObjectStore
	// Bound is how long a read waits for the hot tier before it reads the
	// regional bucket instead. Default DefaultHotTierBound.
	Bound time.Duration
	// QueueBytes bounds the bytes of the fills held; a fill that finds it full
	// is dropped. Default DefaultHotTierQueueBytes.
	QueueBytes int64
	// BytesPerSecond is the rate of fills, with a burst of one second of it; a
	// fill past it is dropped. Default DefaultHotTierBytesPerSecond.
	BytesPerSecond int64
	// SkipPublications keeps publications from writing their parts and index
	// objects to the hot tier, so only reads fill it.
	SkipPublications bool
	// HeadCheckEvery is how many hits go by between two checks, by a HEAD of
	// the regional bucket, that the object behind a hit still exists there.
	// Default DefaultHeadCheckEvery; negative checks none.
	HeadCheckEvery int
	// Clock is what the bound, the rate and the time the hot tier is marked
	// down are measured by. Nil is the wall clock.
	Clock platform.Clock
}

// HotFailure is why a read of the hot tier failed.
type HotFailure int

const (
	// HotFailError is a request the hot tier failed.
	HotFailError HotFailure = iota
	// HotFailSlow is a read the bound gave up on.
	HotFailSlow
	// HotFailCorrupt is a reply that did not make what the read wanted: cut
	// short, an object shorter than the read, or bytes that do not decode.
	HotFailCorrupt
	hotFailures
)

func (f HotFailure) String() string { return [...]string{"error", "slow", "corrupt"}[f] }

// HotFailures is every reason a read of the hot tier fails, in order.
func HotFailures() []HotFailure {
	failures := make([]HotFailure, hotFailures)
	for at := range failures {
		failures[at] = HotFailure(at)
	}
	return failures
}

// HotDrop is why a fill of the hot tier was dropped.
type HotDrop int

const (
	// HotDropQueue is the queue of fills being full.
	HotDropQueue HotDrop = iota
	// HotDropRate is the rate of fills being spent.
	HotDropRate
	// HotDropRead is a GET of the regional bucket the fill could not make.
	HotDropRead
	// HotDropWrite is a PUT the hot tier failed or refused.
	HotDropWrite
	// HotDropClosed is a fill the hot tier closed under.
	HotDropClosed
	hotDrops
)

func (d HotDrop) String() string { return [...]string{"queue", "rate", "read", "write", "closed"}[d] }

// HotDrops is every reason a fill is dropped, in order.
func HotDrops() []HotDrop {
	drops := make([]HotDrop, hotDrops)
	for at := range drops {
		drops[at] = HotDrop(at)
	}
	return drops
}

// HotTierStats is what a hot tier's reads and fills did.
type HotTierStats struct {
	// Hits and Misses count reads the hot tier answered and reads of objects
	// it does not hold. Failed counts the rest, by why.
	Hits, Misses uint64
	Failed       [hotFailures]uint64
	// Skipped counts reads that went to the regional bucket while the hot
	// tier was marked down, and MarkedDown the times it was marked. Down says
	// whether it is now.
	Skipped, MarkedDown uint64
	Down                bool
	// FromReads and FromPublications count the fills handed over and queued,
	// and Duplicates the misses of an object a fill was already held for.
	FromReads, FromPublications, Duplicates uint64
	// Sent counts the fills the hot tier took and SentBytes their bytes;
	// Present the fills that found the object already there.
	Sent, SentBytes, Present uint64
	// Dropped counts the fills dropped, by why.
	Dropped [hotDrops]uint64
	// HeadChecks counts the sampled HEADs of a hit's regional object, and
	// HeadMissing those that found it gone.
	HeadChecks, HeadMissing uint64
	// Queued is the bytes of the fills held now, and QueueBytes their bound.
	Queued, QueueBytes int64
}

// The probes a hot tier marks.
const (
	ProbeHotHit         = "checkpoint/hot-tier-hit"
	ProbeHotMiss        = "checkpoint/hot-tier-miss"
	ProbeHotFailError   = "checkpoint/hot-tier-failed-error"
	ProbeHotFailSlow    = "checkpoint/hot-tier-failed-slow"
	ProbeHotFailCorrupt = "checkpoint/hot-tier-failed-corrupt"
	ProbeHotMarkedDown  = "checkpoint/hot-tier-marked-down"
	ProbeHotSkipped     = "checkpoint/hot-tier-skipped"
	ProbeHotFillSent    = "checkpoint/hot-tier-fill-sent"
	ProbeHotFillPresent = "checkpoint/hot-tier-fill-present"
	ProbeHotDuplicate   = "checkpoint/hot-tier-fill-duplicate"
	ProbeHotDropQueue   = "checkpoint/hot-tier-dropped-queue"
	ProbeHotDropRate    = "checkpoint/hot-tier-dropped-rate"
	ProbeHotDropRead    = "checkpoint/hot-tier-dropped-read"
	ProbeHotDropWrite   = "checkpoint/hot-tier-dropped-write"
	ProbeHotHeadCheck   = "checkpoint/hot-tier-head-check"
	ProbeHotHeadMissing = "checkpoint/hot-tier-head-missing"
)

// HotTierProbes is every probe a hot tier marks.
func HotTierProbes() []string {
	return []string{ProbeHotHit, ProbeHotMiss, ProbeHotFailError, ProbeHotFailSlow, ProbeHotFailCorrupt,
		ProbeHotMarkedDown, ProbeHotSkipped, ProbeHotFillSent, ProbeHotFillPresent, ProbeHotDuplicate,
		ProbeHotDropQueue, ProbeHotDropRate, ProbeHotDropRead, ProbeHotDropWrite, ProbeHotHeadCheck,
		ProbeHotHeadMissing}
}

// The fault-injection sites of a hot tier. Each is what a real bucket does.
const (
	// BuggifyHotDown has a request of the hot tier fail, as a bucket that is
	// down does, after its round trip.
	BuggifyHotDown = "checkpoint/hot-tier-down"
	// BuggifyHotSlow holds a read of the hot tier for up to twice its bound.
	BuggifyHotSlow = "checkpoint/hot-tier-slow"
	// BuggifyHotRefuse has the hot tier refuse a fill's PUT, as a bucket out
	// of quota or permission does.
	BuggifyHotRefuse = "checkpoint/hot-tier-refuse"
	// BuggifyHotLoseReply loses the reply to a request of the hot tier the
	// bucket carried out.
	BuggifyHotLoseReply = "checkpoint/hot-tier-lose-reply"
	// BuggifyHotPartial cuts a reply of the hot tier short, or has it keep
	// only the first half of a fill.
	BuggifyHotPartial = "checkpoint/hot-tier-partial"
)

// HotTierSites is every fault-injection site of a hot tier.
func HotTierSites() []string {
	return []string{BuggifyHotDown, BuggifyHotSlow, BuggifyHotRefuse, BuggifyHotLoseReply, BuggifyHotPartial}
}

var (
	// errHotTierSlow is the cause a read of the hot tier is cancelled with
	// when it reaches its bound.
	errHotTierSlow = errors.New("checkpoint: the hot tier did not answer within its bound")
	// errHotTierRefused is a fill the hot tier refused.
	errHotTierRefused = errors.New("checkpoint: the hot tier refused the fill")
	// errHotTierLost is a reply of the hot tier that was lost.
	errHotTierLost = errors.New("checkpoint: the hot tier's reply was lost")
)

// HotTier is a second bucket reads go to before the regional one, filled
// behind them. Make one with NewHotTier, give it to the store in
// Config.HotTier, and close it after the store.
type HotTier struct {
	objects      platform.ObjectStore
	clock        platform.Clock
	bound        time.Duration
	queueBytes   int64
	publications bool
	headEvery    int
	rate         tokenBucket
	// ctx is the fills' life, which carries what the hot tier was made under;
	// Close ends it.
	ctx    context.Context
	cancel context.CancelFunc
	group  sync.WaitGroup
	fills  *lane

	mu sync.Mutex
	// queued is the bytes of the fills held, and pending the objects they
	// are for.
	queued  int64
	pending map[platform.ObjectKey]bool
	// busy counts the fills held and the checks queued; idle is closed and
	// replaced each time it falls to zero.
	busy int
	idle chan struct{}
	// failures is the reads of the hot tier that failed in a row, and
	// downUntil when reads may try it again.
	failures  int
	downUntil time.Time
	hits      uint64
	stats     HotTierStats
	closed    bool
}

// NewHotTier starts a hot tier's fills under ctx, which they outlive.
func NewHotTier(ctx context.Context, config HotTierConfig) (*HotTier, error) {
	if config.Store == nil || config.Bound < 0 || config.QueueBytes < 0 || config.BytesPerSecond < 0 {
		return nil, ErrInvalidConfig
	}
	if config.Bound == 0 {
		config.Bound = DefaultHotTierBound
	}
	if config.QueueBytes == 0 {
		config.QueueBytes = DefaultHotTierQueueBytes
	}
	if config.BytesPerSecond == 0 {
		config.BytesPerSecond = DefaultHotTierBytesPerSecond
	}
	if config.HeadCheckEvery == 0 {
		config.HeadCheckEvery = DefaultHeadCheckEvery
	}
	clock := platform.ClockOr(config.Clock)
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := &HotTier{objects: config.Store, clock: clock, bound: config.Bound, queueBytes: config.QueueBytes,
		publications: !config.SkipPublications, headEvery: config.HeadCheckEvery,
		rate: newTokenBucket(clock, config.BytesPerSecond), ctx: ctx, cancel: cancel, fills: newLane(),
		pending: make(map[platform.ObjectKey]bool), idle: make(chan struct{})}
	h.group.Go(func() { h.fills.run(ctx) })
	return h, nil
}

// Close stops the fills. What they had not done is dropped. Calling it again
// does nothing.
func (h *HotTier) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.mu.Unlock()
	h.cancel()
	h.group.Wait()
}

// Stats reports what the hot tier's reads and fills did.
func (h *HotTier) Stats() HotTierStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	stats := h.stats
	stats.Queued, stats.QueueBytes = h.queued, h.queueBytes
	stats.Down = h.clock.Now().Before(h.downUntil)
	return stats
}

// Settle returns once every fill handed over has been done or dropped, and
// every sampled check is answered. Nothing waits on a fill; this is what a
// test, or a measurement about to read a warm hot tier, waits on.
func (h *HotTier) Settle(ctx context.Context) error {
	for {
		h.mu.Lock()
		busy, idle := h.busy, h.idle
		h.mu.Unlock()
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

// bug reports whether the in-tree bug id is on for the hot tier's run.
func (h *HotTier) bug(id string) bool { return h != nil && sim.Bug(h.ctx, id) }

// count changes the stats under the lock.
func (h *HotTier) count(change func(stats *HotTierStats)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(&h.stats)
}

// read runs read, an operation over the object key, against the hot tier and,
// where the hot tier does not answer it, against regional. A miss is filled
// behind the read once the regional bucket has answered it.
func (h *HotTier) read(ctx context.Context, key platform.ObjectKey, regional platform.ObjectStore,
	read func(context.Context, *tier) error) error {
	missed := false
	if h.usable(ctx) {
		err := h.attempt(ctx, key, regional, read)
		if err == nil {
			return nil
		}
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if h.bug("hot-tier-read-fails") {
			return err
		}
		missed = errors.Is(err, platform.ErrNotFound)
	}
	from := &tier{objects: regional}
	if err := read(ctx, from); err != nil {
		return err
	}
	if missed {
		h.fillMiss(ctx, key, from, regional)
	}
	return nil
}

// usable reports whether a read may try the hot tier: it is not marked down.
func (h *HotTier) usable(ctx context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clock.Now().Before(h.downUntil) {
		h.stats.Skipped++
		sim.Probe(ctx, ProbeHotSkipped)
		return false
	}
	return true
}

// attempt runs read against the hot tier under the bound, and counts how it
// ended: a hit, a miss, or a failure and why.
func (h *HotTier) attempt(ctx context.Context, key platform.ObjectKey, regional platform.ObjectStore,
	read func(context.Context, *tier) error) error {
	bounded, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := h.clock.AfterFunc(h.bound, func() { cancel(errHotTierSlow) })
	err := read(bounded, &tier{objects: faultyStore{ObjectStore: h.objects, bound: h.bound}})
	timer.Stop()
	switch {
	case err == nil:
		h.hit(ctx, key, regional)
	case context.Cause(ctx) != nil:
	case errors.Is(err, platform.ErrNotFound):
		h.mu.Lock()
		h.failures = 0
		h.stats.Misses++
		h.mu.Unlock()
		sim.Probe(ctx, ProbeHotMiss)
	case errors.Is(context.Cause(bounded), errHotTierSlow):
		h.failed(ctx, key, HotFailSlow, err)
	case errors.Is(err, ErrCorrupt) || errors.Is(err, platform.ErrInvalidRange):
		h.failed(ctx, key, HotFailCorrupt, err)
	default:
		h.failed(ctx, key, HotFailError, err)
	}
	return err
}

// hit counts a read the hot tier answered, and has one hit in headEvery
// checked against the regional bucket behind the fills.
func (h *HotTier) hit(ctx context.Context, key platform.ObjectKey, regional platform.ObjectStore) {
	h.mu.Lock()
	h.failures = 0
	h.stats.Hits++
	h.hits++
	check := h.headEvery > 0 && h.hits%uint64(h.headEvery) == 0
	h.mu.Unlock()
	sim.Probe(ctx, ProbeHotHit)
	if check {
		h.checkHit(key, regional)
	}
}

// failed counts a read of the hot tier that failed for reason, and marks the
// hot tier down after hotTierDownAfter of them in a row.
func (h *HotTier) failed(ctx context.Context, key platform.ObjectKey, reason HotFailure, err error) {
	switch reason {
	case HotFailError:
		sim.Probe(ctx, ProbeHotFailError)
	case HotFailSlow:
		sim.Probe(ctx, ProbeHotFailSlow)
	case HotFailCorrupt:
		sim.Probe(ctx, ProbeHotFailCorrupt)
	}
	slog.DebugContext(ctx, "checkpoint: a read of the hot tier failed; the regional bucket serves it",
		"object", key.String(), "reason", reason.String(), "error", err)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stats.Failed[reason]++
	h.failures++
	if h.failures >= hotTierDownAfter {
		h.failures = 0
		h.downUntil = h.clock.Now().Add(hotTierDownFor)
		h.stats.MarkedDown++
		sim.Probe(ctx, ProbeHotMarkedDown)
		slog.WarnContext(ctx, "checkpoint: the hot tier failed three reads in a row; reads skip it for a while",
			"for", hotTierDownFor, "error", err)
	}
}

// checkHit has the regional object behind a hit checked with a HEAD, behind
// the fills. A warm hot tier hides a reclamation that deleted an object some
// root still reads, so one found gone is logged as an error and counted.
func (h *HotTier) checkHit(key platform.ObjectKey, regional platform.ObjectStore) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.busy++
	h.mu.Unlock()
	h.fills.push(h.ctx, func(ctx context.Context) {
		defer h.done()
		if ctx.Err() != nil {
			return
		}
		h.count(func(stats *HotTierStats) { stats.HeadChecks++ })
		sim.Probe(ctx, ProbeHotHeadCheck)
		_, err := regional.Head(ctx, key)
		switch {
		case errors.Is(err, platform.ErrNotFound):
			h.count(func(stats *HotTierStats) { stats.HeadMissing++ })
			sim.Probe(ctx, ProbeHotHeadMissing)
			slog.ErrorContext(ctx, "checkpoint: the hot tier served an object the regional bucket no longer holds",
				"object", key.String())
		case err != nil:
			slog.DebugContext(ctx, "checkpoint: a sampled check of a hot tier hit failed", "object", key.String(),
				"error", err)
		}
	})
}

// fillMiss hands over the object a read found missing in the hot tier, once
// the regional bucket has answered the read. from is what that read learned
// of the object: its size, and its bytes where the read fetched all of them.
func (h *HotTier) fillMiss(ctx context.Context, key platform.ObjectKey, from *tier, regional platform.ObjectStore) {
	if h.bug("hot-tier-fill-waits") {
		// The bug copies the object in front of the read's caller.
		h.copy(ctx, regional, key, from.whole)
		return
	}
	if h.hold(ctx, key, from.size, false) {
		h.fills.push(h.ctx, func(ctx context.Context) {
			defer h.release(key, from.size)
			h.copy(ctx, regional, key, from.whole)
		})
	}
}

// published hands over an object a publication wrote, once its regional PUT
// has succeeded, with its bytes. It does nothing for a hot tier that takes no
// publications, or for none at all.
func (h *HotTier) published(ctx context.Context, key platform.ObjectKey, data []byte) {
	if h == nil || !h.publications {
		return
	}
	if h.bug("hot-tier-fill-waits") {
		// The bug writes the object in front of the publication.
		h.copy(ctx, nil, key, data)
		return
	}
	size := int64(len(data))
	if h.hold(ctx, key, size, true) {
		h.fills.push(h.ctx, func(ctx context.Context) {
			defer h.release(key, size)
			h.copy(ctx, nil, key, data)
		})
	}
}

// hold takes room in the queue and the rate for a fill of size bytes of key,
// and reports false when the fill is dropped instead: the queue is full, the
// rate is spent, a fill of key is already held, or the hot tier has closed.
// The queue takes a fill larger than all of it only when it is empty.
func (h *HotTier) hold(ctx context.Context, key platform.ObjectKey, size int64, publication bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		h.stats.Dropped[HotDropClosed]++
		return false
	case h.pending[key]:
		h.stats.Duplicates++
		sim.Probe(ctx, ProbeHotDuplicate)
		return false
	case size <= 0 || size > maximumHotObject ||
		h.queued > 0 && h.queued+size > h.queueBytes && !h.bug("hot-tier-unbounded-queue"):
		h.stats.Dropped[HotDropQueue]++
		sim.Probe(ctx, ProbeHotDropQueue)
		return false
	case !h.rate.take(size):
		h.stats.Dropped[HotDropRate]++
		sim.Probe(ctx, ProbeHotDropRate)
		return false
	}
	h.pending[key] = true
	h.queued += size
	h.busy++
	if publication {
		h.stats.FromPublications++
	} else {
		h.stats.FromReads++
	}
	return true
}

// release gives back what hold took, once the fill is done or dropped.
func (h *HotTier) release(key platform.ObjectKey, size int64) {
	h.mu.Lock()
	delete(h.pending, key)
	h.queued -= size
	h.mu.Unlock()
	h.done()
}

// done counts one fill or check finished.
func (h *HotTier) done() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.busy--
	if h.busy == 0 {
		close(h.idle)
		h.idle = make(chan struct{})
	}
}

// drop counts a fill dropped for reason.
func (h *HotTier) drop(ctx context.Context, reason HotDrop) {
	switch reason {
	case HotDropRead:
		sim.Probe(ctx, ProbeHotDropRead)
	case HotDropWrite:
		sim.Probe(ctx, ProbeHotDropWrite)
	}
	h.count(func(stats *HotTierStats) { stats.Dropped[reason]++ })
}

// copy is one fill: data, or where it is nil the whole object as the regional
// bucket holds it, PUT to the hot tier under key if no object is there.
func (h *HotTier) copy(ctx context.Context, regional platform.ObjectStore, key platform.ObjectKey, data []byte) {
	if ctx.Err() != nil {
		h.drop(ctx, HotDropClosed)
		return
	}
	if data == nil {
		read, _, err := platform.ReadObject(ctx, regional, key, 0, maximumHotObject, ErrCorrupt)
		if err != nil {
			slog.DebugContext(ctx, "checkpoint: a fill of the hot tier could not read the regional object",
				"object", key.String(), "error", err)
			h.drop(ctx, HotDropRead)
			return
		}
		data = read
	}
	_, err := faultyStore{ObjectStore: h.objects}.Put(ctx, platform.PutRequest{Key: key,
		Body: bytes.NewReader(data), Size: int64(len(data)),
		Attributes: map[string]string{digestAttribute: digestOf(data)},
		Conditions: platform.PutConditions{IfNoneMatch: true}})
	switch {
	case err == nil:
		h.count(func(stats *HotTierStats) {
			stats.Sent++
			stats.SentBytes += uint64(len(data))
		})
		sim.Probe(ctx, ProbeHotFillSent)
	case errors.Is(err, platform.ErrPrecondition):
		h.count(func(stats *HotTierStats) { stats.Present++ })
		sim.Probe(ctx, ProbeHotFillPresent)
	default:
		slog.DebugContext(ctx, "checkpoint: the hot tier did not take a fill", "object", key.String(), "error", err)
		h.drop(ctx, HotDropWrite)
	}
}

// faultyStore is the hot tier's bucket with the faults a real one has, at the
// fault-injection sites above. Outside a simulation that turns them on it is
// the bucket.
type faultyStore struct {
	platform.ObjectStore
	bound time.Duration
}

func (s faultyStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if err := sim.BuggifyDelay(ctx, BuggifyHotSlow, 0.2, 2*s.bound); err != nil {
		return platform.GetResult{}, err
	}
	result, err := s.ObjectStore.Get(ctx, request)
	switch {
	case err != nil:
		return result, err
	case sim.Buggify(ctx, BuggifyHotDown, 0.2):
		result.Body.Close()
		return platform.GetResult{}, fmt.Errorf("%w: the hot tier is down", platform.ErrUnavailable)
	case sim.Buggify(ctx, BuggifyHotLoseReply, 0.1):
		result.Body.Close()
		return platform.GetResult{}, errHotTierLost
	case sim.Buggify(ctx, BuggifyHotPartial, 0.1) && result.ContentLength > 0:
		result.Body = &cutShort{ReadCloser: result.Body, left: result.ContentLength / 2}
	}
	return result, nil
}

func (s faultyStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if sim.Buggify(ctx, BuggifyHotRefuse, 0.1) {
		return platform.PutResult{}, errHotTierRefused
	}
	if request.Size > 1 && sim.Buggify(ctx, BuggifyHotPartial, 0.1) {
		// A bucket that kept the first half of what it was sent: every read of
		// the object that reaches past it, or reads its end, gets something
		// other than what it asked for.
		request.Size /= 2
	}
	result, err := s.ObjectStore.Put(ctx, request)
	if err == nil && sim.Buggify(ctx, BuggifyHotLoseReply, 0.1) {
		return platform.PutResult{}, errHotTierLost
	}
	return result, err
}

// cutShort is a body that ends after left bytes, as a reply whose connection
// dropped does.
type cutShort struct {
	io.ReadCloser
	left int64
}

func (c *cutShort) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.ReadCloser.Read(p)
	c.left -= int64(n)
	return n, err
}
