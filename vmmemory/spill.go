package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// reservation is the dirty reservation a page was admitted under: a reference
// of the pager's spill storage (zirconvm.SpillStorage), which is where the
// page's bytes go when it is spilled, or none. A page takes it before it is
// dirty, so a spill never needs room (D5 of the Zircon port). Whether the
// reference holds the page's bytes is the storage's state, not the binding's:
// an eviction writes those bytes while a seal may be handing the reservation
// to the checkpoint's copy of the page, and only the reference is named by
// both.
type reservation struct {
	ref   zirconvm.ReferenceValue
	taken bool
}

// The index of published versions is memory: each costs its key twice, once
// in the map and once in the queue it is dropped from, and the map's own
// overhead. maxVersionIndexBytes bounds it, which at 2 MiB pages is every slot
// of any spill file a node has, and at 4 KiB about 20 GiB of versions.
const (
	maxVersionIndexBytes   = 512 << 20
	versionIndexEntryBytes = 104
)

// versionKey is the key a published version is kept under: its page's
// identity, with the checkpoint and volume it names interned as a number
// (versionRootLocked). A full identity holds two strings, and a 4 KiB pager
// keeps millions of versions.
type versionKey struct {
	root uint32
	page uint64
}

// versionRoot is the number a version key names root by, made where there is
// none and create says to. The numbers live as long as the pager; each is one
// checkpoint's volume.
func (h *Host) versionRoot(root rootKey, create bool) (uint32, bool) {
	h.versionRootsMu.RLock()
	n, ok := h.versionRoots[root]
	h.versionRootsMu.RUnlock()
	if ok || !create {
		return n, ok
	}
	h.versionRootsMu.Lock()
	defer h.versionRootsMu.Unlock()
	if n, ok := h.versionRoots[root]; ok {
		return n, true
	}
	n = uint32(len(h.versionRoots))
	h.versionRoots[root] = n
	return n, true
}

// versionKey is the key id's version is kept under.
func (h *Host) versionKey(id pageKey, create bool) (versionKey, bool) {
	root, ok := h.versionRoot(rootOf(id), create)
	return versionKey{root: root, page: id.id.Page}, ok
}

// versionSlots is how many evictions a pager has writing versions at once at
// most, and so how many slots its spill file has beside its dirty budget: a
// version is written to a slot no reservation holds, and a reservation the
// dirty budget admits must always find one.
const versionSlots = 8

// spillPages is how many pages a pager's spill file holds: its dirty budget,
// and a slot for each version write in flight where it keeps versions.
func spillPages(cfg Config) int {
	if spillVersions(cfg) == 0 {
		return cfg.DirtyPages
	}
	return cfg.DirtyPages + versionSlots
}

// SpillFileBytes is the extent of the spill file a pager of cfg allocates
// when it starts, which a host's disk promises it.
func SpillFileBytes(cfg Config) int64 {
	return int64(spillPages(cfg)) * int64(cfg.PageSize)
}

// spillVersions is the most published versions a pager's spill file keeps.
func spillVersions(cfg Config) int {
	switch {
	case cfg.MaxSpillVersions < 0 || cfg.Ephemeral:
		// An ephemeral disk's pages are never published, so it has no
		// versions to keep.
		return 0
	case cfg.MaxSpillVersions > 0:
		return min(cfg.MaxSpillVersions, cfg.DirtyPages+versionSlots)
	}
	return min(cfg.DirtyPages+versionSlots, maxVersionIndexBytes/versionIndexEntryBytes)
}

// noReservation is no reservation at all.
var noReservation reservation

// none reports whether this is no reservation.
func (r reservation) none() bool { return !r.taken }

// spillHolds reports whether a reservation holds its page's bytes.
func (h *Host) spillHolds(spill reservation) bool {
	return !spill.none() && h.spill.Holds(spill.ref)
}

// ErrSpillCorrupt reports a spilled page whose bytes are not the ones that were
// written to its reservation. The spill file is scratch on a local device, so
// the authority for what it should hold lives in this process and not in the
// file: a page that comes back short, zeroed or holding bytes nobody wrote is a
// page the device lost, and handing it to the guest would be handing the guest
// silently wrong memory.
var ErrSpillCorrupt = errors.New("vmmemory: spilled page does not match its checksum")

// readSpill reads the bytes a reservation holds into dst.
func (h *Host) readSpill(ctx context.Context, spill reservation, dst []byte) error {
	if spill.none() {
		return errors.New("private page has no current backing")
	}
	if _, _, err := h.spill.CompressedData(ctx, spill.ref, dst); err != nil {
		if errors.Is(err, zirconvm.ErrIODataIntegrity) {
			return errors.Join(ErrSpillCorrupt, err)
		}
		return err
	}
	return nil
}

// releaseSpill returns a dirty page's reservation to the host and wakes
// waiters.
func (h *Host) releaseSpill(spill reservation) {
	h.mu.Lock()
	h.spill.Free(spill.ref)
	h.releasedLocked()
	h.mu.Unlock()
}

// releasedLocked counts one reservation out of the dirty budget and wakes
// waiters. Caller holds h.mu.
func (h *Host) releasedLocked() {
	h.dirty--
	if h.dirty < h.highWater {
		// Back under the mark: the next store to cross it asks again.
		h.asked = false
	}
	h.signal()
}

// retireSpill gives back the reservation of a checkpoint's page whose
// checkpoint has retired, keeping its bytes as the version of the identity the
// page was published as where the reservation holds them (step 1 of
// plans/local-writeback-2026-10-09.md). They are that version's bytes: the
// copy a checkpoint holds never changes, and a reservation holds the bytes of
// a page only from the eviction that wrote them until a refault hands the
// guest that page as its own again (refault). A load reads the version before
// the backing (readVersions).
func (h *Host) retireSpill(spill reservation, now storedPage) {
	h.mu.Lock()
	if now.stored && !now.id.zero() {
		// Publish gives the allocation back where it keeps nothing.
		key, _ := h.versionKey(now.id, true)
		if h.spill.Publish(spill.ref, key) {
			h.stats.KeptVersions++
		}
	} else {
		h.spill.Free(spill.ref)
	}
	h.releasedLocked()
	h.mu.Unlock()
}

// readVersions reads the pages of [first, first+len(wanted)) that wanted marks
// and the spill file keeps a published version of into data, which covers the
// run whole, and reports what is left for the backing to read: wanted, less
// what it read. keys is each page's identity, zero where it has none to look
// up. A version the device lost is dropped and the page read from the backing;
// a read the caller gave up on fails the run.
func (r *MemoryRegion) readVersions(ctx context.Context, first uint64, wanted []bool, keys []pageKey,
	data []byte) ([]bool, error) {
	h := r.host
	ps := h.pageSize
	left := wanted
	loaded := uint64(0)
	for at, id := range keys {
		if !wanted[at] || id == (pageKey{}) || id.zero() {
			continue
		}
		key, known := h.versionKey(id, false)
		if !known {
			continue
		}
		found, err := h.spill.ReadVersion(ctx, key, data[uint64(at)*ps:uint64(at+1)*ps])
		if err != nil && context.Cause(ctx) != nil {
			return nil, err
		}
		if err != nil {
			slog.Warn("vmmemory: a published version in the spill file could not be read; the page is read from its volume",
				"page", first+uint64(at), "error", err)
			continue
		}
		if !found {
			continue
		}
		if loaded == 0 {
			left = slices.Clone(wanted)
		}
		left[at] = false
		loaded++
	}
	if loaded > 0 {
		h.mu.Lock()
		h.stats.VersionLoads += loaded
		h.mu.Unlock()
	}
	return left, nil
}

// keepVersion writes page, a page of an identity root the caller holds the
// lock of and is about to evict, to the spill file as the version of the
// identity it holds, where the spill file keeps none: the page's next load
// reads it there rather than its volume (step 2 of
// plans/local-writeback-2026-10-09.md). cow and offset are where the root
// holds it. A page lent under a fork point's name that no checkpoint has
// published is not kept, and neither is one while versionSlots evictions are
// writing theirs: each write is to a slot no reservation holds, and the
// spill file has that many beside the dirty budget, so a reservation the
// budget admits always finds one. A write that fails leaves the page to be
// read from its volume, as it would have been.
func (h *Host) keepVersion(ctx context.Context, cow *zirconvm.CowPages, offset uint64, page *zirconvm.VmPage) {
	if spillVersions(h.cfg) == 0 || isLent(page) {
		return
	}
	h.mu.Lock()
	root := h.rootOfPages[cow]
	if root == nil || (root.lent != nil && !root.published) {
		h.mu.Unlock()
		return
	}
	interned, _ := h.versionRoot(root.key, true)
	key := versionKey{root: interned, page: offset / h.pageSize}
	if h.spill.HasVersion(key) {
		h.mu.Unlock()
		return
	}
	if h.versionWrites == versionSlots || h.err != nil {
		h.stats.VersionWritesSkipped++
		h.mu.Unlock()
		return
	}
	h.versionWrites++
	h.mu.Unlock()
	buffer := h.takeWindow(1)
	defer h.putWindow(buffer)
	data := (*buffer)[:h.pageSize]
	f := frameOf(page)
	err := f.file.Read(ctx, f.slot, data)
	written := false
	if err == nil {
		written, err = h.spill.WriteVersion(ctx, key, data)
	}
	h.mu.Lock()
	h.versionWrites--
	if written {
		h.stats.VersionWrites++
	}
	h.mu.Unlock()
	if err != nil && context.Cause(ctx) == nil {
		slog.Warn("vmmemory: an evicted page could not be kept in the spill file; it is read from its volume next time",
			"volume", root.key.volume, "page", key.page, "error", err)
	}
}

// takeReservationLocked admits one more private page to the dirty budget if
// the budget has room. The spill file has a slot for every reservation the
// budget admits beside every version write in flight, so one it admits always
// finds a slot. Caller holds h.mu.
func (h *Host) takeReservationLocked() (reservation, bool) {
	if h.dirty >= h.cfg.DirtyPages {
		return noReservation, false
	}
	ref, ok := h.spill.Reserve()
	if !ok {
		panic(fmt.Sprintf("vmmemory: the spill file had no slot for a reservation the dirty budget admitted: %d of %d dirty, %d versions being written",
			h.dirty, h.cfg.DirtyPages, h.versionWrites))
	}
	h.dirty++
	h.stats.PeakDirtyPages = max(h.stats.PeakDirtyPages, h.dirty)
	return reservation{ref: ref, taken: true}, true
}

// tryTakeSpill admits one more private page to the dirty budget without
// waiting. A load that cannot have one fails rather than holding resident locks
// and an I/O permit while the budget frees up.
func (h *Host) tryTakeSpill() (reservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return noReservation, h.err
	}
	if spill, ok := h.takeReservationLocked(); ok {
		return spill, nil
	}
	return noReservation, ErrCapacity
}

// takeFreeSpill admits up to want more private pages to the dirty budget
// without waiting: it takes only reservations that are free now, which is all
// write-ahead may use.
func (h *Host) takeFreeSpill(want int) []reservation {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil || want <= 0 {
		return nil
	}
	spills := make([]reservation, 0, min(want, h.cfg.DirtyPages-h.dirty))
	for len(spills) < want {
		spill, ok := h.takeReservationLocked()
		if !ok {
			break
		}
		spills = append(spills, spill)
	}
	return spills
}
