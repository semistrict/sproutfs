package host

import (
	"time"

	"github.com/semistrict/sproutfs/vmmemory"
)

// The loss window is a VM's, because the checkpoint that ends it is: one pause
// seals every memory region a VM maps, so the pages of one memory region and the pages of its
// siblings become durable together. The pager holds no idea of a VM — it has
// memory regions and a dirty budget — so this is where the two meet: the host already
// answers the pager's pressure per memory region, and it answers the age the same way.

// lossWindowOf resolves what a configuration asked for into the window itself:
// zero is the default a deployment that named none gets, and a negative value is
// the bound turned off, which everything below spells as zero. It is the
// CheckpointInterval convention, and it is here rather than inline because the
// host and the pager it is given must resolve it the same way — a host reporting
// a window its pager does not enforce would report a bound nothing keeps.
func lossWindowOf(configured time.Duration) time.Duration {
	switch {
	case configured == 0:
		return DefaultLossWindow
	case configured < 0:
		return 0
	}
	return configured
}

// oldestUnpublished answers Pressure.Oldest: when the oldest write no
// checkpoint of this memory region's VM covers was made, zero where that VM holds
// none. A memory region belonging to no VM this host runs answers for itself, which is
// the safe reading — it is at least as old as the memory region's own pages — and the
// pager stalls such a store anyway, since no checkpoint of it can be taken.
//
// It runs on the goroutine of the store that is waiting, so it only reads: every
// memory region reports its own oldest page under its own bookkeeping lock, and nothing
// here waits for a fault, a seal or a publication.
func (h *Host) oldestUnpublished(memoryRegion *vmmemory.MemoryRegion) time.Time {
	_, entry := h.machineFor(memoryRegion)
	if entry == nil {
		return memoryRegion.OldestUnpublished()
	}
	return oldestOf(entry.runtime.MemoryRegions())
}

// oldestOf is the oldest unpublished write across one VM's disks. RAM is not
// in it: the interval checkpoints disks alone, so nothing it takes would ever
// make a RAM write published, and a guest's RAM is not what the window bounds.
func oldestOf(memoryRegions map[string]*vmmemory.MemoryRegion) time.Time {
	var oldest time.Time
	for _, memoryRegion := range memoryRegions {
		if memoryRegion.Kind() == vmmemory.Ram {
			continue
		}
		since := memoryRegion.OldestUnpublished()
		if since.IsZero() {
			continue
		}
		if oldest.IsZero() || since.Before(oldest) {
			oldest = since
		}
	}
	return oldest
}

// LossWindow reports how long the named VM has held a write no checkpoint
// covers, and whether the pager is holding its stores back for it. Zero is a VM
// with nothing unpublished, a VM this host does not run, and a VM whose memory regions
// this host cannot see; waiting is always false where the bound is disabled.
//
// It is what a host's status says about one VM's exposure, and the only place
// the number exists: the pager measures it per memory region and nothing else adds them
// up.
func (h *Host) LossWindow(vmID string) (age time.Duration, waiting bool) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	if entry == nil {
		return 0, false
	}
	oldest := oldestOf(entry.runtime.MemoryRegions())
	if oldest.IsZero() {
		return 0, false
	}
	age = max(h.clock.Since(oldest), 0)
	return age, h.lossWindow > 0 && age > h.lossWindow
}

// overLossWindow reports a VM whose oldest unpublished write is older than this
// host's window, which is a VM whose guest the pager is already holding back.
func (h *Host) overLossWindow(entry *registration) bool {
	if h.lossWindow <= 0 || entry == nil {
		return false
	}
	oldest := oldestOf(entry.runtime.MemoryRegions())
	return !oldest.IsZero() && h.clock.Since(oldest) > h.lossWindow
}

// backoff is how long the checkpoint loop waits before trying again after a
// publication that failed while its VM was already past the loss window. The
// interval is the wrong wait there: the window stays exceeded for the whole of
// it, so the guest is held back for the whole of it, and a store that came back
// a moment after the failure would not be used until the next turn. An eighth
// of the interval is the first retry, doubling up to the interval, which spends
// at most eight times an interval's requests on an outage and settles back to
// the interval's own rate while one lasts.
//
// There is no jitter here. Jitter exists to keep the VMs of one host from
// checkpointing in lockstep on a working deployment; these attempts are a host
// that cannot publish at all, and what each of them is worth is that it happens
// soon.
func backoff(interval time.Duration, failures int) time.Duration {
	wait := interval / 8
	if wait <= 0 {
		// An interval too short to divide: retrying at nothing at all would be a
		// busy loop rather than a retry.
		return interval
	}
	for range failures - 1 {
		if wait >= interval {
			break
		}
		wait *= 2
	}
	return min(wait, interval)
}
