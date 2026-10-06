package vmmemory

import (
	"context"
	"errors"
	"time"
)

// ErrHandedOff reports a memory region whose volume belongs to another host now. Its
// pages are still served; nothing that would read or write the volume is.
var ErrHandedOff = errors.New("managed-memory-region handed its volume off")

// ReadResident copies one page's current bytes for a peer, reports false for a
// page whose bytes this host does not hold, and reports separately whether the
// page it served is this memory region's own state rather than the volume's. It never
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
func (r *MemoryRegion) ReadResident(ctx context.Context, page uint64, dst []byte) (held, unpublished bool, err error) {
	z := r.zircon

	return z.readResident(ctx, page, dst)
}

// Resident lists the pages this memory region holds, in ascending order, for a bulk
// stream to the destination of a migration. These are the pages ReadResident
// can serve; the rest are the destination's own reads from the volume. It is a
// snapshot: a page can be evicted before the stream asks for it, which
// ReadResident then reports as absent.
//
// A memory region that cannot answer reports why. An empty listing is a memory region that
// holds nothing, and a destination told that reads every page from the volume,
// which is only correct when this host really holds none of them.
func (r *MemoryRegion) Resident() ([]uint64, error) {
	z := r.zircon

	return z.resident()
}

// Handoff gives up this memory region's volume while keeping its pages. It belongs to
// a live migration: the guest is stopped, the final checkpoint is in the log,
// and the volume handle is about to be released so another host can acquire the
// log at once. Nothing here may touch that volume again, so verification stops
// checking authority this host no longer has, and a flush, a seal, a population
// or any fault reports ErrHandedOff instead: the guest is stopped, and a fault
// would mean it is not. ReadResident and Resident keep serving this host's
// pages to the destination until Detach releases them.
//
// A sealed memory region is not something to hand off: a checkpoint is reading its
// checkpoint under a volume handle that is about to be another host's, so the
// seal is reported and the memory region keeps its volume.
//
// It reports how long this memory region has held its oldest unpublished write, which
// the handoff carries so the destination goes on measuring the same loss window
// instead of starting a new one. It is read here, under the lock that makes the
// volume another host's, because that is the moment the set stops changing.
func (r *MemoryRegion) Handoff(ctx context.Context) (time.Duration, error) {
	z := r.zircon

	return z.handoff(ctx)
}

// Unpublished lists the pages this memory region holds that no checkpoint of its VM
// has, in ascending order. They are the guest's writes since this host's last
// checkpoint, and they exist nowhere but here: a migration destination must
// fetch every one of them before this host may stop serving, while the rest of
// Resident is an optimization it can skip and read from object storage instead.
//
// A memory region that cannot answer reports why, because the empty set is what a
// destination acts on by fetching nothing: these pages exist nowhere else, and
// a handoff that names none of them rewinds the guest to the last checkpoint.
func (r *MemoryRegion) Unpublished() ([]uint64, error) {
	z := r.zircon

	return z.unpublished()
}

// MemoryRegionStats is one memory region's share of the host's pages. Like Stats it is
// read-only instrumentation: nothing consults it.
type MemoryRegionStats struct {
	// ResidentPages counts pages holding host memory, shared or private.
	// PrivatePages counts pages whose bytes are this memory region's own and not yet
	// its volume's, whether they are resident, spilled or held by a checkpoint.
	// SharedPages counts the resident ones at least one other memory region of this
	// pager also maps, which is the part of this memory region's memory the host is
	// holding once rather than once per guest.
	ResidentPages, PrivatePages, SharedPages int
	// DirtySince is when the oldest of those private pages was written, zero
	// where there are none. It is the one field here a host acts on rather than
	// reports: the loss window of the VM this memory region belongs to is the oldest of
	// its memory regions' DirtySince, and while that is older than the window the
	// pager holds the guest's stores back.
	DirtySince time.Time
	// PageSize is the page of the pager holding this memory region, carried with the
	// counts so the byte conversions below need nothing else. A host adds a VM's
	// memory regions up across two pagers of different geometry, and page counts of
	// different pages cannot be added at all.
	PageSize uint64
}

// The three counts in the unit the host accounts memory in. Page counts belong
// to the pager that holds them and cannot be added across pagers of different
// geometry; bytes can, which is what a host-wide report needs.
func (s MemoryRegionStats) ResidentBytes() uint64 { return uint64(s.ResidentPages) * s.PageSize }
func (s MemoryRegionStats) PrivateBytes() uint64  { return uint64(s.PrivatePages) * s.PageSize }
func (s MemoryRegionStats) SharedBytes() uint64   { return uint64(s.SharedPages) * s.PageSize }

// Stats reports this memory region's pages. It is a snapshot taken without stopping
// the guest, exactly like Resident.
func (r *MemoryRegion) Stats(ctx context.Context) (MemoryRegionStats, error) {
	z := r.zircon

	return z.stats(ctx)
}

// eachBindingBatch is how many bindings eachBinding visits under one hold of
// its locks.
const eachBindingBatch = 256
