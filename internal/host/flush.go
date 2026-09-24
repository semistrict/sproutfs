package host

import (
	"time"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// A guest's flush is its fsync reaching the host: the guest's filesystem asking
// that what it wrote so far be durable. The interval makes a VM's disks durable
// every CheckpointInterval regardless, so a flush does not take a checkpoint of
// its own. What it does is hold the guest back while the disks are stale: a
// flush whose VM holds a disk write older than the flush bound waits until a
// checkpoint covers it, and the host asks for that checkpoint out of the
// interval's turn. A flush that returned therefore leaves nothing of the guest's
// older than the bound unpublished, and a guest whose disks cannot be published
// stops making fsync progress instead of being told a lie.
//
// Staleness is measured by the VM's oldest unpublished disk write rather than by
// when its last checkpoint landed. The two give the guest the same guarantee —
// everything written before the bound is durable — but a VM that has written
// nothing since its last checkpoint is not stale however long ago that was, and
// its flushes complete at once.

// DefaultFlushBound is how old a VM's oldest unpublished disk write may be for
// a flush of it to complete at once, when a host configures none. It is the
// checkpoint interval: a guest whose disks the interval is keeping up with never
// waits, and one whose writes have outlived an interval waits for the
// checkpoint its flush asked for.
const DefaultFlushBound = DefaultCheckpointInterval

// flushBoundOf resolves what a configuration asked for into the bound itself:
// zero is the default, and a negative value turns the bound off, which
// everything below spells as zero.
func flushBoundOf(configured time.Duration) time.Duration {
	switch {
	case configured == 0:
		return DefaultFlushBound
	case configured < 0:
		return 0
	}
	return configured
}

// pendingFlush is one flush a VM's disks were too stale to complete. since is
// the age its guarantee is about: the flush is owed once no disk write made
// before it is unpublished, which the first checkpoint sealed after the flush
// arrived gives it — however long that checkpoint takes, and however much the
// guest writes meanwhile.
type pendingFlush struct {
	since time.Time
	done  func(error)
}

// flushed answers the pager's flush of one region: at once when the VM's disks
// are fresh enough, otherwise once a checkpoint makes them so.
//
// It runs on the pager's goroutine, so it only records the flush and signals the
// loop, as checkpointNow does. The check and the record are one step under the
// registration's lock, and so is the loop's release after a publication lands,
// so a flush either sees that publication or is released by it.
//
// A flush the host cannot make durable completes at once: a region no VM this
// host runs maps, and a VM with no checkpoint loop, have nothing that would ever
// release it, and a guest must not hang on a flush for that.
func (h *Host) flushed(region *vmmemory.Region, done func(error)) {
	if h.flushBound <= 0 {
		done(nil)
		return
	}
	_, entry := h.machineFor(region)
	if entry == nil || entry.now == nil {
		done(nil)
		return
	}
	since := h.clock.Now().Add(-h.flushBound)
	entry.mu.Lock()
	if covered(entry, since) {
		entry.mu.Unlock()
		done(nil)
		return
	}
	entry.flushes = append(entry.flushes, pendingFlush{since: since, done: done})
	entry.mu.Unlock()
	select {
	case entry.now <- struct{}{}:
	default:
		// The loop already owes this VM a checkpoint.
	}
}

// dropFlush answers no flush, which is what a closing host does with them.
func dropFlush(*vmmemory.Region, func(error)) {}

// releaseFlushes completes every flush a publication that just landed has
// covered, and keeps the ones it has not.
func releaseFlushes(entry *registration) {
	entry.mu.Lock()
	var released []func(error)
	kept := entry.flushes[:0]
	for _, flush := range entry.flushes {
		if covered(entry, flush.since) {
			released = append(released, flush.done)
			continue
		}
		kept = append(kept, flush)
	}
	clear(entry.flushes[len(kept):])
	entry.flushes = kept
	entry.mu.Unlock()
	for _, done := range released {
		done(nil)
	}
}

// covered reports that one VM holds no disk write made before since that no
// checkpoint has published.
func covered(entry *registration, since time.Time) bool {
	oldest := oldestOf(entry.runtime.Regions())
	return oldest.IsZero() || !oldest.Before(since)
}
