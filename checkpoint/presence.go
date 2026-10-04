package checkpoint

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/rank"
)

// Asking the cluster what it holds. A pull inside the share wants to know, for
// each page of a checkpoint, whether a read would find it on the hosts' disks,
// and it wants to know that without reading the page. So it asks the
// window's first k+m ranks, the disks a read asks, which stripes of the
// window each holds (peer.Presence), and counts a page held when they hold k
// distinct indices of it under the membership's code: what a read needs to
// rebuild it. The disks this host serves answer with no request.
//
// Every rank is asked once for all the windows it ranks for, beside the
// others. A rank this reader has marked down, that the table of peers has
// marked down, or that no member serves is not asked, and holds nothing as far
// as the pull knows; so does one that does not answer within the stripe
// timeout. The pull then reads the page from the store and fills the cluster
// with it, and a holder that has the stripe already drops the keep. A holder
// ahead of the asker answers stale: the asker reads the membership and asks
// again under the newer generation, as a read does.
//
// Only the membership's own code is asked about. A window stored under a code
// the deployment used before is read from the store by the pull and filled
// under the deployment's code, as a read that rebuilds it under the earlier
// code refills it.

// The probes a presence check marks.
const (
	// ProbePresenceHeld is a page a presence check found the cluster holds.
	ProbePresenceHeld = "checkpoint/presence-held"
	// ProbePresenceLacking is a page a presence check found it does not.
	ProbePresenceLacking = "checkpoint/presence-lacking"
	// ProbePresenceLost is an answer the lose-presence site took.
	ProbePresenceLost = "checkpoint/presence-answer-lost"
	// ProbePresenceStale is a presence check asked again under a newer
	// generation a holder named.
	ProbePresenceStale = "checkpoint/presence-asked-again"
)

// buggifyPresenceLost loses a holder's answer to a presence check, as one that
// never came does.
const buggifyPresenceLost = "checkpoint/presence-lose-answer"

// presenceAnswer is one rank's answer to a presence check: for each window it
// was asked about, the stripes it holds.
type presenceAnswer struct {
	held []peer.Present
	err  error
}

// holds reports, for each key, whether the cluster holds k distinct indices of
// its envelope on the window's first k+m ranks, under the code of the
// membership the cache follows. A cache that follows none reports nothing
// held.
func (r *clusterReader) holds(ctx context.Context, keys []diskKey) []bool {
	held := make([]bool, len(keys))
	m, ok := r.cluster.following()
	if !ok {
		return held
	}
	for tries := 0; ; tries++ {
		newer := r.presence(ctx, m, keys, held)
		if newer <= m.Generation() || tries == staleRetries || ctx.Err() != nil {
			return held
		}
		next, err := r.cluster.catch(ctx, newer)
		if err != nil || next.Generation() <= m.Generation() {
			slog.DebugContext(ctx, "checkpoint: a presence check told it is stale could not read the membership",
				"generation", m.Generation(), "holder", newer, "error", err)
			return held
		}
		r.probe(ProbePresenceStale)
		m = next
		clear(held)
	}
}

// presenceWindow is the keys of one window a presence check asks about: the
// page of each within the window, and where each page's key is among the
// check's keys.
type presenceWindow struct {
	window rank.Window
	pages  []uint32
	at     map[uint32]int
}

// presence asks the ranks of every key's window under m which stripes they
// hold, marks in held the keys whose pages k distinct indices of are held,
// and reports the newest generation a holder answered stale with, zero for
// none.
func (r *clusterReader) presence(ctx context.Context, m membership.Membership, keys []diskKey, held []bool) uint64 {
	list := m.List()
	code := list.Code()
	var windows []presenceWindow
	at := make(map[rank.Window]int)
	for position, key := range keys {
		window := key.rankWindow()
		index, seen := at[window]
		if !seen {
			index = len(windows)
			at[window] = index
			windows = append(windows, presenceWindow{window: window, at: make(map[uint32]int)})
		}
		page := uint32(key.Page - window.Page(0))
		windows[index].pages = append(windows[index].pages, page)
		windows[index].at[page] = position
	}
	// indices is, for each key, the indices of its envelope found held.
	indices := make([][]bool, len(keys))
	for position := range indices {
		indices[position] = make([]bool, code.Width())
	}
	take := func(w presenceWindow, present peer.Present) {
		for index, pages := range present {
			if index >= code.Width() {
				break
			}
			for _, page := range pages {
				if position, asked := w.at[page]; asked {
					indices[position][index] = true
				}
			}
		}
	}
	local := make(map[rank.Identity]bool)
	for _, identity := range r.cluster.disks() {
		if !r.cluster.serves(m, identity) {
			continue
		}
		disk, release, kept := r.cluster.hold(identity)
		if !kept {
			continue
		}
		local[identity] = true
		for _, w := range windows {
			take(w, disk.present(w.window, w.pages, code))
		}
		release()
	}
	// asks is every rank asked, in the order its first window ranks it, and
	// of each the windows it is asked about.
	type ask struct {
		cache   rank.Cache
		route   membership.Route
		windows []int
	}
	var asks []ask
	asked := make(map[rank.Identity]int)
	for w, window := range windows {
		for _, cache := range list.Ranks(window.window) {
			if local[cache.Identity] || r.peers == nil || r.marks.isDown(cache.Identity) ||
				r.peers.Peer(cache.Address).Down() {
				continue
			}
			route, routed := m.Route(cache.Identity)
			if !routed {
				continue
			}
			index, seen := asked[cache.Identity]
			if !seen {
				index = len(asks)
				asked[cache.Identity] = index
				asks = append(asks, ask{cache: cache, route: route})
			}
			asks[index].windows = append(asks[index].windows, w)
		}
	}
	answers := make([]presenceAnswer, len(asks))
	var group sync.WaitGroup
	for index, a := range asks {
		request := peer.Presence{Code: code}
		for _, w := range a.windows {
			request.Windows = append(request.Windows, windows[w].window)
		}
		r.count(func(stats *ReadStats) { stats.Presences++ })
		group.Go(func() {
			requestCtx, cancel := context.WithCancelCause(ctx)
			timer := r.clock.AfterFunc(r.settings.stripeTimeout, func() { cancel(errStripeTimeout) })
			answers[index].held, answers[index].err = r.peers.Peer(a.route.Address).Presence(requestCtx, a.route, request)
			timer.Stop()
			cancel(nil)
			if answers[index].err == nil && r.buggify(buggifyPresenceLost, 0.05) {
				answers[index] = presenceAnswer{err: errAnswerLost}
				r.probe(ProbePresenceLost)
			}
		})
	}
	group.Wait()
	newer := uint64(0)
	for index, a := range asks {
		answer := answers[index]
		if answer.err != nil {
			var stale *peer.StaleError
			if errors.As(answer.err, &stale) && stale.Generation > m.Generation() {
				newer = max(newer, stale.Generation)
			}
			slog.DebugContext(ctx, "checkpoint: a rank did not say what it holds; the pull counts it as holding nothing",
				"cache", a.cache.Identity, "error", answer.err)
			continue
		}
		for at, w := range a.windows {
			if at < len(answer.held) {
				take(windows[w], answer.held[at])
			}
		}
	}
	for position, found := range indices {
		distinct := 0
		for _, index := range found {
			if index {
				distinct++
			}
		}
		held[position] = distinct >= code.K
		if r.bug("pull-count-any-stripe") {
			// The guard counts a page held when any rank holds any stripe
			// of it, as a check of pages alone would.
			held[position] = distinct > 0
		}
		if held[position] {
			r.probe(ProbePresenceHeld)
		} else {
			r.probe(ProbePresenceLacking)
		}
	}
	return newer
}
