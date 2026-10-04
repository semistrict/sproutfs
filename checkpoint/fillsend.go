package checkpoint

import (
	"context"
	"errors"
	"time"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// A fill's holders, side by side. The worker of fills decides each holder of
// a fill in turn, in the order the fills were handed over and each fill's
// holders in rank order: it takes the keep's bytes of the rate and its room in
// the background budget, or drops it, and hands it to the lane of the member
// that serves the holder's disk. Each lane carries its keeps one at a time, in
// the order they were decided, and the lanes run beside each other. So a
// publication's keeps go to its window's holders together, and each holder
// still receives this host's keeps in the order the fills were handed over
// in: which keep finds the rate or the budget spent, and the order of the
// keeps on each link, follow from that order and not from the Go scheduler.
//
// A lane has one keep on the wire at a time. A member answers the requests of
// one connection beside each other, so two keeps on the wire to one member
// would reach its queue of writes in whatever order its scheduler ran them.
// The fills of reads, pulls and repairs go ahead of the publications' on each
// lane, so a read's keep waits for the keep on the wire at most. The writes to
// this host's own disks are one more lane.
//
// A publication's keeps are bounded: a lane holds at most
// fillKeepsPerHolder of them, one on the wire and one behind it, and the
// host at most its keeps in flight. The worker waits for room on the lane of
// a publication's next holder rather than going past it, which would send a
// later fill's keep ahead of an earlier one. A read's keeps are bounded by
// the queue's room instead, and never wait for a lane.

// fillKeepsPerHolder is how many of the publications' keeps one lane holds:
// one on the wire, and one decided behind it, which goes as soon as the first
// is answered.
const fillKeepsPerHolder = 2

// ownLane is the lane of the writes to this host's own disks, which no
// member's address names.
const ownLane = platform.Address("")

// holderLane is the jobs on their way to the member that serves some disks,
// or to this host's own: those of reads, pulls and repairs first, then the
// publications', each in the order they were decided. One worker carries them
// one at a time while there are any.
type holderLane struct {
	prompt, paced []*holderJob
	// held is the publications' jobs the lane holds, queued or on their way.
	held    int
	running bool
}

// holderJob is one holder's stripes of a fill on their way: a keep to the
// member that serves the holder's disk, or a write to a disk this host keeps.
type holderJob struct {
	p       *placement
	lane    *holderLane
	holder  rank.Cache
	stripes []keyedStripe
	// own says the holder's disk is this host's; keep is what is sent
	// otherwise, which holds admitted bytes of the background budget until the
	// job ends, and twice says a lost answer has it sent again.
	own      bool
	keep     peer.Keep
	admitted int64
	twice    bool
	// pace is the publication's whose fill the job is of, nil for a read's,
	// a pull's or a repair's, and wait its wait for a busy holder.
	pace *fillPace
	wait keepWait
	// ended is closed once the job has ended.
	ended chan struct{}
}

// keepWait is one of a publication's keeps waiting: for the rate or the
// background budget before it is sent, or for a busy holder after. since is
// when it began to wait, zero while it has not; tried is when it was last
// tried; and backoff is how long it waits next for the budget or a busy
// holder.
type keepWait struct {
	since, tried time.Time
	backoff      time.Duration
}

// retry is how long a keep a busy holder or a full budget refused waits
// before it is tried again: fillRetryFirst, and twice as long each time
// after, up to fillRetryMost.
func (w *keepWait) retry() time.Duration {
	w.backoff = min(max(2*w.backoff, fillRetryFirst), fillRetryMost)
	return w.backoff
}

// decide does the next holder of p: a job on its lane, or its stripes
// dropped. For a publication's fill, paced, a holder that must wait is left
// where it is: decide reports blocked when the holder's lane, or the
// publications' jobs on their way, are at their bound, or their keeps hold
// the background budget, all of which a job's end lifts; and otherwise how
// long until it is tried again, for the rate or the background budget. A fill
// of a read's, a pull's or a repair's never waits.
func (f *filler) decide(ctx context.Context, p *placement, paced *pacedFill) (wait time.Duration, blocked bool) {
	holder := p.order[p.next]
	stripes := p.held[holder.Identity]
	job := &holderJob{p: p, holder: holder, stripes: stripes, ended: make(chan struct{})}
	if paced != nil {
		job.pace = paced.pace
	}
	queued := false
	switch {
	case ctx.Err() != nil:
		f.drop(ctx, DropFailed, len(stripes))
	case f.cluster.keeps(holder.Identity):
		if paced != nil && !f.laneRoom(ownLane) {
			return 0, true
		}
		job.own = true
		queued = f.enqueue(ownLane, job)
	default:
		route, routed := p.m.Route(holder.Identity)
		if !routed {
			f.drop(ctx, DropStale, len(stripes))
			break
		}
		if paced != nil && !f.laneRoom(route.Address) {
			return 0, true
		}
		switch verdict, wait := f.admit(ctx, p, holder, paced, job); verdict {
		case keepBlocked:
			return 0, true
		case keepWaits:
			return wait, false
		case keepAdmitted:
			queued = f.enqueue(route.Address, job)
		}
	}
	p.next++
	if paced != nil {
		f.waited(&paced.wait)
		paced.rated, paced.keep = false, nil
	}
	if queued && f.bug("fill-keeps-one-at-a-time") {
		// The bug waits for each holder before it decides the next, as a host
		// that sent one keep at a time did.
		<-job.ended
	}
	return 0, false
}

// keepVerdict is what admit made of a keep.
type keepVerdict int

const (
	// keepAdmitted is a keep that holds what it needs, and goes on its lane.
	keepAdmitted keepVerdict = iota
	// keepDropped is a keep whose stripes were dropped.
	keepDropped
	// keepWaits is a keep of a publication's tried again after a while.
	keepWaits
	// keepBlocked is a keep of a publication's tried again once one of the
	// publications' jobs on their way ends.
	keepBlocked
)

// admit takes what a keep of job's stripes needs before it is sent: its bytes
// of the rate and its room in this host's background budget, and draws
// whether a lost answer has it sent twice. A keep of a read's, a pull's or a
// repair's that finds the rate spent or the budget full is dropped. One of a
// publication's that waits, paced, is not: it waits, and admit says how long
// until it is tried again. Each of them comes free in time: the rate refills,
// and the budget's bulk work ends. A publication's keep that finds the budget
// held by the publications' own keeps on their way is blocked until one of
// them ends, which it waits for whether or not the publication still waits
// for its fills: that is the pace of its keeps, not a want of room. A keep
// larger than its share of the whole budget never fits it, and is dropped at
// once. A keep that waits is built once, and kept with paced. A holder this
// cache's reads have marked down is sent nothing.
func (f *filler) admit(ctx context.Context, p *placement, holder rank.Cache, paced *pacedFill,
	job *holderJob) (keepVerdict, time.Duration) {
	stripes := len(job.stripes)
	var keep peer.Keep
	if paced != nil && paced.keep != nil {
		keep = *paced.keep
	} else {
		keep = keepFor(p, job.stripes)
	}
	size := int64(len(keep.Payload))
	waits := paced != nil && paced.pace.waits()
	if paced != nil {
		paced.wait.tried = f.waits.Now()
	}
	if paced == nil || !paced.rated {
		if waits {
			if wait := f.rate.after(size); wait > 0 {
				// The rate is on the cache's clock, and a wait on the clock
				// of the table of peers: it is tried again no sooner than a
				// refusal would be, which costs it nothing, since the rate
				// fills to a second of itself meanwhile.
				paced.keep = &keep
				return f.waitFor(ctx, paced, DropRate, stripes, max(wait, fillRetryFirst))
			}
			paced.rated = true
		} else if !f.rate.take(size) {
			f.drop(ctx, DropRate, stripes)
			return keepDropped, 0
		}
	}
	if f.peers == nil {
		f.drop(ctx, DropFailed, stripes)
		return keepDropped, 0
	}
	if f.down != nil && f.down(holder.Identity) && !f.bug("cluster-fill-marked-down") {
		f.drop(ctx, DropDown, stripes)
		return keepDropped, 0
	}
	budget, priority := f.peers.Background(), keep.Priority()
	if !budget.TryAcquire(priority, size) {
		switch {
		case paced != nil && f.keepsHoldBudget():
			paced.keep = &keep
			return keepBlocked, 0
		case waits && budget.Admits(priority, size):
			paced.keep = &keep
			return f.waitFor(ctx, paced, DropBudget, stripes, paced.wait.retry())
		}
		f.drop(ctx, DropBudget, stripes)
		return keepDropped, 0
	}
	job.keep, job.admitted = keep, size
	job.twice = sim.Buggify(ctx, buggifyFillSendTwice, 0.1)
	return keepAdmitted, 0
}

// waitFor is a publication's keep that must wait, after at most, before it is
// tried again, as later decides: it waits, or it has waited out the bound and
// is dropped.
func (f *filler) waitFor(ctx context.Context, paced *pacedFill, reason DropReason, stripes int,
	after time.Duration) (keepVerdict, time.Duration) {
	if wait := f.later(ctx, paced.pace, &paced.wait, reason, stripes, after); wait > 0 {
		return keepWaits, wait
	}
	return keepDropped, 0
}

// keepsHoldBudget reports whether the publications' keeps on their way hold
// any of the background budget.
func (f *filler) keepsHoldBudget() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pacedBytes > 0
}

// laneRoom reports room for one more of a publication's jobs on the lane of
// key, and among the host's keeps in flight.
func (f *filler) laneRoom(key platform.Address) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	lane := f.lanes[key]
	return f.paced < f.keepsInFlight && (lane == nil || lane.held < fillKeepsPerHolder)
}

// enqueue puts job on the lane of key, behind the jobs of its kind there, and
// starts the lane's worker if it has none. It reports false once the fills
// have closed, and drops the job.
func (f *filler) enqueue(key platform.Address, job *holderJob) bool {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		f.drop(f.ctx, DropFailed, len(job.stripes))
		if job.admitted > 0 {
			f.peers.Background().Release(job.admitted)
		}
		return false
	}
	lane := f.lanes[key]
	if lane == nil {
		lane = &holderLane{}
		f.lanes[key] = lane
	}
	job.lane = lane
	job.p.pending++
	if job.pace != nil {
		lane.paced = append(lane.paced, job)
		lane.held++
		f.paced++
		f.pacedBytes += job.admitted
	} else {
		lane.prompt = append(lane.prompt, job)
	}
	start := !lane.running
	lane.running = true
	f.mu.Unlock()
	if start {
		f.group.Go(func() { f.runLane(f.ctx, key, lane) })
	}
	return true
}

// runLane is the worker of one lane: it carries the lane's jobs one at a
// time, a read's, a pull's and a repair's first, until the lane is empty.
func (f *filler) runLane(ctx context.Context, key platform.Address, lane *holderLane) {
	// The bug carries the publications' jobs first, so a read's keep waits
	// behind every one of them on the lane.
	behind := f.bug("fill-reads-behind-publications")
	for {
		f.mu.Lock()
		var job *holderJob
		switch {
		case len(lane.prompt) > 0 && !(behind && len(lane.paced) > 0):
			job = lane.prompt[0]
			lane.prompt = lane.prompt[1:]
		case len(lane.paced) > 0:
			job = lane.paced[0]
			lane.paced = lane.paced[1:]
		default:
			lane.running = false
			if lane.held == 0 {
				delete(f.lanes, key)
			}
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		if f.bug("fill-concurrently") {
			// The bug sends each keep beside the ones before it, so they race
			// to the holder's link.
			f.group.Go(func() {
				f.carry(ctx, job)
				f.end(job)
			})
			continue
		}
		f.carry(ctx, job)
		f.end(job)
	}
}

// carry takes job to its holder, and counts what became of it: it writes the
// stripes of a disk this host keeps, or sends the keep under the placement's
// membership and waits for its answer. A holder ahead of the membership
// answers stale, and the keep is sent again under the newer generation, which
// the holder then checks its ranks under. A keep of a publication's that waits,
// which its holder answers BUSY, is sent again later, ahead of every job
// behind it on its lane; its holder's requests are answered in time.
func (f *filler) carry(ctx context.Context, job *holderJob) {
	p, stripes := job.p, len(job.stripes)
	switch {
	case ctx.Err() != nil:
		f.drop(ctx, DropFailed, stripes)
	case job.own:
		f.written(func(ctx context.Context) {
			f.writeOwn(ctx, job.holder.Identity, p.kind, p.window, p.code, job.stripes)
		})
	default:
		for {
			var last membership.Route
			if job.pace != nil {
				job.wait.tried = f.waits.Now()
			}
			err := f.routed(ctx, p.m, job.holder.Identity, func(route membership.Route) error {
				last = route
				return f.peers.Peer(route.Address).KeepAdmitted(ctx, route, job.keep)
			})
			if job.pace != nil && job.pace.waits() && errors.Is(err, peer.ErrBusy) {
				wait := f.later(ctx, job.pace, &job.wait, DropBusy, stripes, job.wait.retry())
				if wait == 0 {
					return
				}
				if f.sleep(ctx, wait) {
					continue
				}
				// A busy holder's last answer stands once the fills close.
			}
			f.waited(&job.wait)
			f.sent(ctx, err, stripes, len(job.keep.Payload))
			if err == nil && job.twice {
				// The second is what a sender that lost the first's answer
				// sends: its holder drops what it already holds, and the
				// second answer counts for nothing.
				_ = f.peers.Peer(last.Address).KeepAdmitted(ctx, last, job.keep)
			}
			return
		}
	}
}

// sleep waits for wait on the clock the publications' fills wait on, and
// reports false when ctx ends first.
func (f *filler) sleep(ctx context.Context, wait time.Duration) bool {
	timer := f.waits.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C():
		return true
	case <-ctx.Done():
		return false
	}
}

// end is a job done: it gives back the job's room in the background budget
// and its place on its lane, and once its fill's last job has ended, the
// fill's room in the queue. The worker of fills, which may be waiting for
// room on the lane, is told.
func (f *filler) end(job *holderJob) {
	if job.admitted > 0 {
		f.peers.Background().Release(job.admitted)
	}
	f.mu.Lock()
	if job.pace != nil {
		job.lane.held--
		f.paced--
		f.pacedBytes -= job.admitted
	}
	job.p.pending--
	last := job.p.pending == 0 && job.p.sealed
	f.mu.Unlock()
	close(job.ended)
	if last {
		f.release(job.p.room)
	}
	if job.pace != nil {
		f.fills.tell()
	}
}

// seal is every holder of p decided by the worker of fills. The fill's room
// in the queue is given back once its last job has ended, which may be now.
func (f *filler) seal(p *placement) {
	f.mu.Lock()
	p.sealed = true
	last := p.pending == 0
	f.mu.Unlock()
	if last {
		f.release(p.room)
	}
}

// later is a keep of a publication's that must wait, wait at most, before it
// is tried again, and reports how long that is: no further than the bound
// from when the keep began to wait. A keep that has waited the bound gives up
// the publication's waits and is dropped for reason, and later reports zero.
func (f *filler) later(ctx context.Context, pace *fillPace, wait *keepWait, reason DropReason, stripes int,
	after time.Duration) time.Duration {
	now := f.waits.Now()
	if wait.since.IsZero() {
		wait.since = now
		f.count(func(stats *FillStats) { stats.Waits++ })
		sim.Probe(ctx, ProbeFillPublicationWaited)
	}
	if f.bug("fill-publication-waits-forever") {
		return after
	}
	if left := f.waitBound - now.Sub(wait.since); left > 0 {
		return min(after, left)
	}
	f.giveUp(pace)
	f.drop(ctx, reason, stripes)
	return 0
}

// waited ends a keep's wait, counting how long it waited until its last try.
func (f *filler) waited(wait *keepWait) {
	if !wait.since.IsZero() {
		waited := wait.tried.Sub(wait.since)
		f.count(func(stats *FillStats) { stats.Waited += waited })
	}
	*wait = keepWait{}
}
