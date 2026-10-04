package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/rank"
)

// Hosts marked down. A reader marks a host down on its own, as mcrouter does:
// after three of its stripe requests in a row time out, or after one refused
// connection, which the table of peers already marks down as a hard failure
// and this reader takes from it. While a host is marked down, this reader does
// not ask it for stripes and does not send it fills; it reads from the other
// holders. A probe goes after DefaultProbeFirst, then at intervals growing by
// half up to DefaultProbeMax, spread by a hash of the host and the attempt so
// that every reader does not probe at once. Only a probe that succeeds clears
// the mark. A miss, BUSY, an answer for another disk, a stale answer or a
// stripe that fails its checks is not a failure of the host.
//
// A reader marks down at most a fifth of the disks of its membership, and
// always at least one
// host, so a small cluster can still mark one. Past that it marks no more:
// that many failing at once more likely means its own network has failed.

// Defaults of how a host marked down is probed back.
const (
	DefaultProbeFirst = 10 * time.Second
	DefaultProbeMax   = 60 * time.Second
)

// timeoutsToMark is how many stripe requests in a row must time out before
// their host is marked down.
const timeoutsToMark = 3

// downMarks is the hosts one reader has marked down, and the timeouts in a row
// of every host it has asked.
type downMarks struct {
	reader *clusterReader

	mu    sync.Mutex
	marks map[rank.Identity]*downMark
}

// downMark is what a reader knows of one host's answers.
type downMark struct {
	timeouts int
	down     bool
}

// isDown reports a host this reader has marked down.
func (m *downMarks) isDown(identity rank.Identity) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	mark := m.marks[identity]
	return mark != nil && mark.down
}

// down is how many hosts are marked down now.
func (m *downMarks) down() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, mark := range m.marks {
		if mark.down {
			count++
		}
	}
	return count
}

// observe weighs how one stripe request to cache ended: an answer, a miss
// among them, clears the host's timeouts in a row, a timeout adds one and the
// third marks it down, and a refused connection marks it down at once.
func (m *downMarks) observe(ctx context.Context, cache rank.Cache, err error, miss bool) {
	r := m.reader
	if err == nil && m.reader.buggify(buggifyClusterFalseTimeout, 0.05) {
		err = errStripeTimeout
	}
	if err == nil && miss && r.bug("cluster-mark-on-miss") {
		// The guard counts a holder that holds nothing as one that failed.
		err = errStripeTimeout
	}
	switch {
	case err == nil:
		m.mu.Lock()
		if mark := m.marks[cache.Identity]; mark != nil {
			mark.timeouts = 0
		}
		m.mu.Unlock()
	case errors.Is(err, errStripeTimeout):
		r.count(func(stats *ReadStats) { stats.Timeouts++ })
		m.reader.probe(ProbeClusterTimeout)
		m.mu.Lock()
		mark := m.marks[cache.Identity]
		if mark == nil {
			mark = &downMark{}
			m.marks[cache.Identity] = mark
		}
		mark.timeouts++
		timedOut := mark.timeouts >= timeoutsToMark
		m.mu.Unlock()
		if timedOut {
			m.mark(ctx, cache, fmt.Errorf("%d stripe requests in a row timed out", timeoutsToMark))
		}
	case refused(err) || r.peers != nil && !failedHere(err) && r.peers.Peer(cache.Address).Down():
		m.mark(ctx, cache, err)
	}
}

// refused reports a request the table of peers did not send, because it has
// marked the peer down for a hard failure: a refused connection.
func refused(err error) bool { return errors.Is(err, peer.ErrDown) }

// failedHere reports a request that ended for a reason of this host's or of
// the answer's, not of the peer's: closed, given up on, BUSY, answered for
// another disk or under another generation, or with nowhere to go.
func failedHere(err error) bool {
	return errors.Is(err, peer.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, peer.ErrBusy) ||
		errors.Is(err, peer.ErrNotMe) || errors.Is(err, peer.ErrStale) || errors.Is(err, errAnswerLost) ||
		errors.Is(err, errNoPeers) || errors.Is(err, errNoRoute)
}

// mark marks cache down, unless a fifth of the membership's disks, and at
// least one, is marked down already, and starts probing it back.
func (m *downMarks) mark(ctx context.Context, cache rank.Cache, cause error) {
	r := m.reader
	held, _ := r.disk.following()
	list := held.List()
	limit := max(1, list.Len()/5)
	m.mu.Lock()
	mark := m.marks[cache.Identity]
	if mark == nil {
		mark = &downMark{}
		m.marks[cache.Identity] = mark
	}
	if mark.down {
		m.mu.Unlock()
		return
	}
	down := 0
	for _, listed := range list.Caches() {
		if other := m.marks[listed.Identity]; other != nil && other.down {
			down++
		}
	}
	if down >= limit && !r.bug("cluster-mark-every-host") {
		m.mu.Unlock()
		r.count(func(stats *ReadStats) { stats.Capped++ })
		m.reader.probe(ProbeClusterCapped)
		return
	}
	mark.down = true
	m.mu.Unlock()
	r.count(func(stats *ReadStats) { stats.MarkedDown++ })
	m.reader.probe(ProbeClusterMarkedDown)
	slog.WarnContext(ctx, "checkpoint: a host is marked down; reads skip it until a probe answers",
		"cache", cache.Identity, "address", cache.Address, "cause", cause)
	if !r.spawn(func(life context.Context) { m.probeBack(life, cache) }) {
		m.clear(cache.Identity)
	}
}

// clear clears a host's mark and its timeouts.
func (m *downMarks) clear(identity rank.Identity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.marks, identity)
}

// probeBack probes a host marked down until a probe succeeds, then clears its
// mark: first after about probeFirst, then at intervals growing by half up to
// probeMax.
func (m *downMarks) probeBack(ctx context.Context, cache rank.Cache) {
	r := m.reader
	wait := r.settings.probeFirst
	if r.bug("cluster-probe-at-once") {
		// The guard probes every second, as the table of peers first does.
		wait = time.Second
	}
	for attempt := 1; ; attempt++ {
		timer := r.clock.NewTimer(spreadProbe(cache.Identity, attempt, wait))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		if r.peers != nil {
			probeCtx, cancel := context.WithCancelCause(ctx)
			over := r.clock.AfterFunc(r.settings.stripeTimeout, func() { cancel(errStripeTimeout) })
			_, err := r.peers.Peer(cache.Address).Probe(probeCtx, cache.Identity)
			over.Stop()
			cancel(nil)
			if err == nil {
				m.clear(cache.Identity)
				r.count(func(stats *ReadStats) { stats.Cleared++ })
				m.reader.probe(ProbeClusterCleared)
				slog.InfoContext(ctx, "checkpoint: a host marked down answered its probe", "cache", cache.Identity)
				return
			}
			if ctx.Err() != nil {
				return
			}
		}
		if !r.bug("cluster-probe-at-once") {
			wait = nextProbeWait(wait, r.settings.probeMax)
		}
	}
}

// nextProbeWait is the wait after one of wait: half as long again, up to most.
func nextProbeWait(wait, most time.Duration) time.Duration { return min(wait*3/2, most) }

// spreadProbe is wait moved by up to a tenth either way, by a hash of the host
// and the attempt.
func spreadProbe(identity rank.Identity, attempt int, wait time.Duration) time.Duration {
	hash := fnv.New64a()
	_, _ = fmt.Fprintf(hash, "%s/%d", identity, attempt)
	tenth := int64(wait / 10)
	if tenth <= 0 {
		return wait
	}
	return wait - time.Duration(tenth) + time.Duration(int64(hash.Sum64()%uint64(2*tenth+1)))
}
