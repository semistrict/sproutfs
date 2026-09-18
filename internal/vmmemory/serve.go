package vmmemory

import (
	"context"
	"errors"
	"sort"
	"time"
)

// ErrHandedOff reports a region whose volume belongs to another host now. Its
// pages are still served; nothing that would read or write the volume is.
var ErrHandedOff = errors.New("managed-memory region handed its volume off")

// ReadResident copies one page's current bytes for a peer, reports false for a
// page whose bytes this host does not hold, and reports separately whether the
// page it served is this region's own state rather than the volume's. It never
// loads: a page the destination is told is absent is one it reads from the
// volume itself, which is what the volume is for.
//
// Unpublished means the page is dirty here: the guest wrote it after this
// host's last checkpoint, so no checkpoint of the VM holds these bytes and the
// destination's pager must keep them privately until its own next checkpoint
// publishes them. A served page that is not unpublished is the selected
// checkpoint's own bytes, which the destination could equally have read from
// storage.
//
// Held means host memory or private state of this host's own: a shared
// resident page, a private page the guest wrote, or the spill copy of either.
// A page that was never faulted and a page the volume reports as a hole are
// not held.
//
// A page the guest still shares with a checkpoint is served from that
// checkpoint's copy, which is the same resident page the guest reads through:
// a store since the seal would have copied the guest away from the checkpoint and would
// be served from its own page instead. After the final seal of a stopped guest
// the two are therefore the same bytes, and that is what the destination must
// be given.
//
// While the guest runs, the answer is only as current as the moment it is
// taken, exactly like the pages a bulk stream already sent.
func (r *Region) ReadResident(ctx context.Context, page uint64, dst []byte) (held, unpublished bool, err error) {
	h := r.host
	if len(dst) != PageSize {
		return false, false, ErrRange
	}
	if err := r.live.RLock(ctx); err != nil {
		return false, false, err
	}
	defer r.live.RUnlock()
	if page >= uint64(r.pageCount) {
		return false, false, ErrRange
	}
	// The fault stripe orders this against the faults that change who owns a
	// page's bytes, and the I/O permit is taken before any resident page's lock,
	// as a fault takes it: a reader holding a page and waiting for a permit would
	// deadlock against a fault holding a permit and waiting for that page. The
	// stripe comes before the region here for the same reason it does in a
	// fault: the two orders must be one order.
	if err := r.stripe(page).Lock(ctx); err != nil {
		return false, false, err
	}
	defer r.stripe(page).Unlock()
	if err := r.mu.RLock(ctx); err != nil {
		return false, false, err
	}
	defer r.mu.RUnlock()
	if err := r.serving(); err != nil {
		return false, false, err
	}
	b := r.lookupBinding(page)
	if b == nil {
		return false, false, nil
	}
	if err := h.beginIO(ctx); err != nil {
		return false, false, err
	}
	defer h.endIO()
	pg, err := h.current(ctx, b)
	if err != nil {
		return false, false, err
	}
	if pg != nil {
		defer h.unlock(pg)
	} else if !b.dirty || (b.checkpoint == nil && !h.spillHolds(b.spillSlot)) {
		// Evicted since it was listed, or never held at all.
		return false, false, nil
	}
	if err := h.read(ctx, b, pg, dst); err != nil {
		return false, false, err
	}
	return true, b.dirty, nil
}

// Resident lists the pages this region holds, in ascending order, for a bulk
// stream to the destination of a migration. These are the pages ReadResident
// can serve; the rest are the destination's own reads from the volume. It is a
// snapshot: a page can be evicted before the stream asks for it, which
// ReadResident then reports as absent.
//
// A region that cannot answer reports why. An empty listing is a region that
// holds nothing, and a destination told that reads every page from the volume,
// which is only correct when this host really holds none of them.
func (r *Region) Resident() ([]uint64, error) {
	// The caller of a listing has no deadline to give: this waits only for the
	// exclusive holders of the region, which are bounded page-table work.
	if err := r.mu.RLock(context.Background()); err != nil {
		return nil, err
	}
	defer r.mu.RUnlock()
	if err := r.serving(); err != nil {
		return nil, err
	}
	return r.residentPages(), nil
}

// Handoff gives up this region's volume while keeping its pages. It belongs to
// a live migration: the guest is stopped, the final checkpoint is in the log,
// and the volume handle is about to be released so another host can acquire the
// log at once. Nothing here may touch that volume again, so verification stops
// checking authority this host no longer has, and a flush, a seal, a population
// or any fault reports ErrHandedOff instead: the guest is stopped, and a fault
// would mean it is not. ReadResident and Resident keep serving this host's
// pages to the destination until Detach releases them.
//
// A sealed region is not something to hand off: a checkpoint is reading its
// checkpoint under a volume handle that is about to be another host's, so the
// seal is reported and the region keeps its volume.
//
// It reports how long this region has held its oldest unpublished write, which
// the handoff carries so the destination goes on measuring the same loss window
// instead of starting a new one. It is read here, under the lock that makes the
// volume another host's, because that is the moment the set stops changing.
func (r *Region) Handoff(ctx context.Context) (time.Duration, error) {
	if err := r.mu.Lock(ctx); err != nil {
		return 0, err
	}
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return 0, err
	}
	if r.currentCheckpoint() != nil {
		return 0, ErrSealed
	}
	r.handed = true
	return r.unpublishedAge(), nil
}

// Unpublished lists the pages this region holds that no checkpoint of its VM
// has, in ascending order. They are the guest's writes since this host's last
// checkpoint, and they exist nowhere but here: a migration destination must
// fetch every one of them before this host may stop serving, while the rest of
// Resident is an optimization it can skip and read from object storage instead.
//
// A region that cannot answer reports why, because the empty set is what a
// destination acts on by fetching nothing: these pages exist nowhere else, and
// a handoff that names none of them rewinds the guest to the last checkpoint.
func (r *Region) Unpublished() ([]uint64, error) {
	if err := r.mu.RLock(context.Background()); err != nil {
		return nil, err
	}
	defer r.mu.RUnlock()
	if err := r.serving(); err != nil {
		return nil, err
	}
	var result []uint64
	r.eachBinding(func(b *binding) {
		if b.dirty {
			result = append(result, b.index)
		}
	})
	return result, nil
}

// residentPages collects the pages holding host memory or private state.
func (r *Region) residentPages() []uint64 {
	var result []uint64
	r.eachBinding(func(b *binding) {
		if b.resident != nil || b.dirty {
			result = append(result, b.index)
		}
	})
	return result
}

// RegionStats is one region's share of the host's pages. Like Stats it is
// read-only instrumentation: nothing consults it.
type RegionStats struct {
	// ResidentPages counts pages holding host memory, shared or private.
	// PrivatePages counts pages whose bytes are this region's own and not yet
	// its volume's, whether they are resident, spilled or held by a checkpoint.
	ResidentPages, PrivatePages int
	// DirtySince is when the oldest of those private pages was written, zero
	// where there are none. It is the one field here a host acts on rather than
	// reports: the loss window of the VM this region belongs to is the oldest of
	// its regions' DirtySince, and while that is older than the window the
	// pager holds the guest's stores back.
	DirtySince time.Time
}

// Stats reports this region's pages. It is a snapshot taken without stopping
// the guest, exactly like Resident.
func (r *Region) Stats(ctx context.Context) (RegionStats, error) {
	if err := r.mu.RLock(ctx); err != nil {
		return RegionStats{}, err
	}
	defer r.mu.RUnlock()
	var stats RegionStats
	r.eachBinding(func(b *binding) {
		if b.resident != nil {
			stats.ResidentPages++
		}
		if b.dirty {
			stats.PrivatePages++
		}
	})
	stats.DirtySince = r.OldestUnpublished()
	return stats, nil
}

// eachBinding visits every page that has per-page state, in ascending order,
// with the binding map and the host lock held. Both are taken per binding block
// rather than for the whole scan, so a large region does not hold up the page
// transitions that need them — a reclaim reading which reservation a page names
// among them. Caller holds the region lock, which is what keeps the blocks
// themselves in existence across the scan.
func (r *Region) eachBinding(visit func(*binding)) {
	h := r.host
	r.bindingsMu.Lock()
	keys := make([]uint64, 0, len(r.blocks))
	for key := range r.blocks {
		keys = append(keys, key)
	}
	r.bindingsMu.Unlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, key := range keys {
		r.bindingsMu.Lock()
		block := r.blocks[key]
		h.mu.Lock()
		for i := range block {
			if b := &block[i]; b.index < uint64(r.pageCount) {
				visit(b)
			}
		}
		h.mu.Unlock()
		r.bindingsMu.Unlock()
	}
}
