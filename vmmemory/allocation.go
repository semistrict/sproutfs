package vmmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory/internal/slots"
)

// arenaFile is one file of the arena as this pager keeps it: the file itself,
// which of its offsets hold a page, and who may read it.
type arenaFile struct {
	ArenaFile
	// id is the order the pager made the file in, which is what it is called
	// in what the pager reports. A session names it by a number of its own.
	id    int
	slots *slots.Space
	// leases names the resource reservation the page at one slot was admitted
	// under, and the extent that slot belongs to where the placement rule put
	// it there. It is a map rather than one entry per slot because a file has
	// far more slots than pages: what the arena holds is bounded by
	// Config.ResidentPages, however large its address space is. It is guarded
	// by Host.mu.
	leases map[int]residentSlot
	// owner is the memory region whose private file this is, and nil for a
	// file other memory regions may read: the one file of a shared arena, the
	// shared file of an isolated one, and a fork point's file.
	owner *MemoryRegion
	// The rest belongs to an isolated arena and is guarded by Host.mu; see
	// isolation.go. pages is the resident page at each held slot of a private
	// or a fork file, which are the files the pager looks into by slot.
	// digests is what the upload read of each published page of a private
	// file hashed to. holders is every memory region a fork file was given
	// to, by the number it was given under. orphaned marks a private file
	// whose memory region has detached, which is given back with its last
	// page.
	pages    map[int]*resident
	digests  map[int][32]byte
	holders  map[*MemoryRegion]int
	orphaned bool
}

// keepFile keeps one file the arena made, whose offsets space says which hold
// a page. Caller holds h.mu, or has not shared the host yet.
func (h *Host) keepFile(file ArenaFile, space *slots.Space) *arenaFile {
	f := &arenaFile{ArenaFile: file, id: h.madeFiles, slots: space, leases: make(map[int]residentSlot)}
	h.madeFiles++
	return f
}

// fileSlot is where a resident page is: one slot of one file of the arena.
// Slots of two files are never consecutive, whatever their numbers, so a run of
// pages one mapping command covers is a run of slots of one file.
type fileSlot struct {
	file *arenaFile
	slot int
}

// plus is the slot count slots after this one, in the same file.
func (s fileSlot) plus(count int) fileSlot { return fileSlot{s.file, s.slot + count} }

// The numbers a memory region's session names its files by. File 0 is its
// private file, the only one it may map writable, and file 1 the shared file
// of an isolated arena. A fork point's file takes the next number free when a
// child of it first maps from it.
const (
	privateFileNumber = 0
	sharedFileNumber  = 1
)

// privateFile is the file a private page of this memory region goes in: a
// store's copy, a write-ahead page and a page a rule copied. A shared arena has
// one file, and it is every memory region's.
func (r *MemoryRegion) privateFile() *arenaFile {
	if r.private != nil {
		return r.private
	}
	return r.host.files[0]
}

// sharedFile is the file a page this memory region loads by its identity goes
// in. Other memory regions may map such a page too.
func (r *MemoryRegion) sharedFile() *arenaFile {
	if r.host.shared != nil {
		return r.host.shared
	}
	return r.host.files[0]
}

// fileNumber is the number this memory region's session names a file by, or
// -1 for a file it was never given, which no map may name.
func (r *MemoryRegion) fileNumber(f *arenaFile) int {
	switch {
	case f == r.privateFile():
		return privateFileNumber
	case f == r.host.shared:
		return sharedFileNumber
	}
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	if number, given := r.forks[f]; given {
		return number
	}
	return -1
}

// runAt is the run of count pages from page that count slots of one file from
// at back, as this memory region's session names that file.
func (r *MemoryRegion) runAt(page uint64, at fileSlot, count int) MapRun {
	return MapRun{Page: page, File: r.fileNumber(at.file), Slot: at.slot, Count: count}
}

// giveFiles hands the memory region's process the files every session holds:
// its private file, writable, and in an isolated arena the shared file, which
// it may only read. Nothing is mapped before they are.
func (r *MemoryRegion) giveFiles(ctx context.Context) error {
	if err := r.mapping.GiveFile(ctx, privateFileNumber, r.privateFile().ArenaFile, true); err != nil {
		return err
	}
	if shared := r.host.shared; shared != nil {
		return r.mapping.GiveFile(ctx, sharedFileNumber, shared.ArenaFile, false)
	}
	return nil
}

// heldLocked is how many pages the arena holds, in every file. Caller holds
// h.mu.
func (h *Host) heldLocked() int { return h.held }

// freeLocked is how many more pages f may be given: what is left of the
// pager's capacity, and never more than the file's own offsets allow. Caller
// holds h.mu.
func (h *Host) freeLocked(f *arenaFile) int {
	return min(f.slots.Free(), h.cfg.ResidentPages-h.held)
}

// residentSlot is what one arena slot holding a page costs: the resource
// reservation it was admitted under, and the extent it belongs to where the
// placement rule put it there rather than the allocator. A slot of an extent
// stays that extent's when its page goes; an ordinary one goes back to its
// file's free offsets with it.
type residentSlot struct {
	lease  *resource.Lease
	extent *extent
}

// takeFree takes count consecutive free slots starting at one, against the
// host budget: the pages they will hold are what that budget bounds, so a
// refused reservation is a refused allocation. Caller holds h.mu.
func (h *Host) takeFree(at fileSlot, count int) bool {
	if count > h.freeLocked(at.file) {
		// The arena's addresses are not its capacity: an offset run this long
		// exists, and the memory behind it does not.
		return false
	}
	lease, err := h.resources.TryAcquire(context.Background(), int64(count)*int64(h.pageSize))
	if err != nil {
		return false
	}
	at.file.slots.Take(at.slot, count)
	h.held += count
	for i := range count {
		at.file.leases[at.slot+i] = residentSlot{lease: lease}
	}
	h.stats.PeakResidentPages = max(h.stats.PeakResidentPages, h.heldLocked())
	return true
}

// putFree returns one slot and the reservation it held. A slot with no
// reservation, or a reservation that will not take its bytes back, is an
// accounting invariant this host has broken: it makes the host terminal, which
// stops the VMs it runs deliberately, rather than killing the process they run
// in. The slot is not returned either, because nothing knows what it holds.
// Caller holds h.mu.
func (h *Host) putFree(at fileSlot) {
	entry := at.file.leases[at.slot]
	if entry.lease == nil {
		h.err = errors.Join(h.err, fmt.Errorf("managed arena terminal: slot %d of file %d freed without a resource reservation", at.slot, at.file.id))
		return
	}
	if err := entry.lease.Release(int64(h.pageSize)); err != nil {
		h.err = errors.Join(h.err, fmt.Errorf("managed arena terminal: releasing slot %d of file %d: %w", at.slot, at.file.id, err))
		return
	}
	if entry.lease.Bytes() == 0 {
		entry.lease.Close()
	}
	delete(at.file.leases, at.slot)
	h.held--
	if e := entry.extent; e != nil {
		// The memory leaves and the address stays the range's, until the last
		// page of the extent goes and the extent itself does.
		if e.fixed {
			at.file.slots.Put(at.slot)
		} else {
			at.file.slots.Empty()
		}
		e.held--
		h.dropExtent(e)
	} else {
		at.file.slots.Put(at.slot)
	}
	h.emptiedLocked(at.file)
}

// firstFreeLocked is the lowest slot of f a page may be put at, or -1 where
// the pager has no page left to put anywhere, however many of f's own offsets
// are unoccupied. Caller holds h.mu.
func (h *Host) firstFreeLocked(f *arenaFile) int {
	if h.freeLocked(f) == 0 {
		return -1
	}
	return f.slots.First()
}

// allocateFree takes up to want consecutive free slots of one file without
// evicting. It returns the first slot and the count taken, which may be zero.
// Contiguous slots let consecutive pages become one mapping; the scan is
// bounded so a fragmented file costs a shorter run, not a long search. A run
// the budget will not reserve whole is halved rather than abandoned.
func (h *Host) allocateFree(f *arenaFile, want int) (fileSlot, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	bestStart, bestLen := f.slots.LongestRun(min(want, h.freeLocked(f)))
	for bestLen > 0 {
		if h.takeFree(fileSlot{f, bestStart}, bestLen) {
			return fileSlot{f, bestStart}, bestLen
		}
		bestLen /= 2
	}
	return fileSlot{f, 0}, 0
}

// allocateFreeFrom is allocateFree preferring the whole run to start at
// prefer, so that a new mapping continues the slots of the one before it.
func (h *Host) allocateFreeFrom(prefer fileSlot, want int) (fileSlot, int) {
	f := prefer.file
	h.mu.Lock()
	if prefer.slot >= 0 && prefer.slot <= f.slots.Offsets()-want && want <= h.freeLocked(f) {
		count := 0
		for count < want && f.slots.IsFree(prefer.slot+count) {
			count++
		}
		if count == want && h.takeFree(prefer, count) {
			h.mu.Unlock()
			return prefer, count
		}
	}
	h.mu.Unlock()
	return h.allocateFree(f, want)
}

// allocatePrivate takes the slot a private page of this index goes at: the
// slot the placement rule gives it within its range's extent, evicting where
// the page budget rather than the address is what is missing. A page the rule
// has no slot for — a pager that places nothing, no extent left, or a slot
// already holding the bytes a checkpoint froze — falls back to an ordinary one
// beside its neighbours.
func (r *MemoryRegion) allocatePrivate(ctx context.Context, index uint64) (fileSlot, error) {
	if err := context.Cause(ctx); err != nil {
		return fileSlot{}, err
	}
	if r.private != nil {
		// A page of a private file has two places, and one of them is free or
		// holds a page nothing maps.
		return r.allocateOwn(ctx, index, false)
	}
	h := r.host
	f := r.privateFile()
	h.mu.Lock()
	if h.err != nil {
		err := h.err
		h.mu.Unlock()
		return fileSlot{}, err
	}
	at, placeable := h.place(r, index)
	noExtent := !placeable && h.carving(f) && f.slots.FreeExtents() == 0
	h.mu.Unlock()
	if at.slot >= 0 {
		return at, nil
	}
	if noExtent {
		// The file's extents are held by the idle pages of memory regions that
		// have gone; one is given back for this range.
		freed, err := h.reclaimExtent(ctx, f)
		if err != nil {
			return fileSlot{}, err
		}
		if freed {
			h.mu.Lock()
			at, placeable = h.place(r, index)
			h.mu.Unlock()
			if at.slot >= 0 {
				return at, nil
			}
		}
	}
	if !placeable {
		return r.allocateNear(ctx, f, index)
	}
	return h.allocate(ctx, r, f, func() int {
		at, _ := h.place(r, index)
		return at.slot
	}, sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5))
}

// Prefer extending a neighboring mapping's physical run before using the
// first free slot of f. This consumes no speculative reservation and never
// waits for a preferred slot; pressure falls back to ordinary bounded
// reclamation.
func (r *MemoryRegion) allocateNear(ctx context.Context, f *arenaFile, index uint64) (fileSlot, error) {
	if err := context.Cause(ctx); err != nil {
		return fileSlot{}, err
	}
	h := r.host
	// Taking a victim while free slots are there is legal and merely wasteful,
	// and it is the only way an arena that is not full reaches the eviction
	// paths at all: a pager sized to hold its whole guest never evicts, so
	// nothing ever overlaps an eviction with a publication or a seal.
	if preferEviction := sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5); preferEviction {
		return h.allocate(ctx, r, f, nil, true)
	}
	for _, delta := range []int64{-1, 1} {
		neighbor := int64(index) + delta
		if neighbor < 0 || neighbor >= int64(r.pageCount) {
			continue
		}
		b := r.lookupBinding(uint64(neighbor))
		if b == nil {
			continue
		}
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return fileSlot{}, err
		}
		if pg := b.resident; pg != nil && pg.slot >= 0 && pg.file == f {
			at := pg.plus(-int(delta))
			if at.slot >= 0 && at.slot < f.slots.Offsets() && f.slots.IsFree(at.slot) && h.takeFree(at, 1) {
				h.mu.Unlock()
				return at, nil
			}
		}
		h.mu.Unlock()
	}
	return h.allocate(ctx, r, f, nil, false)
}

// allocate returns one slot of f for a page of r, evicting the least recently
// used unlocked page when the arena is full. It waits for progress rather than
// failing while every candidate is temporarily busy.
//
// The victim leaves every protected memory region its pages where it can: see
// protectedLocked. Where it cannot, it is the least recently used page of all.
//
// place, where it is not nil, is where the slot must be: the placement rule has
// already decided this page's slot, so only the page budget is at stake and
// only an eviction anywhere can make room for it. It is called under the host
// lock and reports the slot it took, or -1 while that room is not there yet.
//
// preferEviction takes a victim even where a free slot would do, for one pass:
// the iteration after a fruitless preference takes the free slot, so a
// buggified allocation cannot wait on a victim that will not come.
func (h *Host) allocate(ctx context.Context, r *MemoryRegion, f *arenaFile, place func() int, preferEviction bool) (fileSlot, error) {
	for {
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return fileSlot{}, err
		}
		if r != nil {
			r.wanted = h.displaced
		}
		resourceChanged := h.resources.Changed()
		capacityBlocked := false
		if place != nil {
			if !preferEviction {
				if slot := place(); slot >= 0 {
					h.mu.Unlock()
					return fileSlot{f, slot}, nil
				}
			}
			// The address is this page's whatever happens; what is missing is a
			// page of the budget, which every eviction gives back.
			capacityBlocked = true
		} else if slot := h.firstFreeLocked(f); slot >= 0 && !preferEviction && h.takeFree(fileSlot{f, slot}, 1) {
			h.mu.Unlock()
			return fileSlot{f, slot}, nil
		} else if slot >= 0 {
			capacityBlocked = true
		}
		// An idle page is the first thing given up: no memory region maps it, so its
		// slot costs no revocation and no spill, and nothing a guest is using.
		if !preferEviction {
			if pg := h.takeIdleLocked(); pg != nil {
				h.mu.Unlock()
				if err := h.dropIdle(ctx, pg); err != nil {
					return fileSlot{}, err
				}
				continue
			}
		}
		var candidates []*resident
		busy := false
		share := h.cfg.ResidentPages / max(len(h.memoryRegions), 1)
		for _, fair := range []bool{true, false} {
			for pg := h.lru.front(); pg != nil; pg = h.lru.next(pg) {
				if fair && !h.fairLocked(pg, r, share) {
					continue
				}
				if !pg.mu.TryLock() {
					busy = true
					continue
				}
				// A page a store is replacing is one the guest still reads through a
				// mapping that names this offset, and the command that stops it
				// naming it has not landed. It is not this reclaim's to take; the
				// store gives it up itself once its mapping is in.
				usable := pg.replacing == 0
				for b := range pg.aliases.all() {
					if b.memoryRegion.terminal.Load() != nil {
						usable = false
						break
					}
				}
				if usable {
					// One victim per reclaim: a page is the whole scratch budget one
					// eviction may read out of the arena.
					candidates = append(candidates, pg)
					break
				}
				pg.mu.Unlock()
			}
			if len(candidates) > 0 {
				break
			}
		}
		changed := h.changed
		// Slots reserved by a concurrent load have no LRU entry yet.
		if h.lru.len() < h.cfg.ResidentPages {
			busy = true
		}
		h.mu.Unlock()
		if len(candidates) > 0 {
			preferEviction = false
			err := h.evictBatch(ctx, candidates)
			h.unlockAll(candidates)
			// A victim another memory region will not give up is not this allocation's
			// failure: the next pass skips it, because that memory region is terminal
			// from here, and takes another page. The arena is finite, so every
			// such pass removes one page from what this loop will consider,
			// and an arena made entirely of them reports ErrCapacity rather
			// than spinning.
			if err != nil && !errors.Is(err, errVictimHeld) {
				return fileSlot{}, err
			}
			continue
		}
		if preferEviction {
			// Nothing was evictable this pass. Take the free slot next one
			// rather than wait for a victim.
			preferEviction = false
			continue
		}
		if !busy && !capacityBlocked {
			return fileSlot{}, ErrCapacity
		}
		select {
		case <-ctx.Done():
			return fileSlot{}, context.Cause(ctx)
		case <-changed:
		case <-resourceChanged:
		}
	}
}

// fairLocked reports whether evicting pg to make room for a page of r leaves
// every other protected memory region its pages. Caller holds h.mu.
func (h *Host) fairLocked(pg *resident, r *MemoryRegion, share int) bool {
	for b := range pg.aliases.all() {
		if q := b.memoryRegion; q != r && h.protectedLocked(q, share) {
			return false
		}
	}
	return true
}

// protectedLocked reports a memory region whose pages an eviction for another
// memory region leaves alone. Every attached memory region is owed share of the
// arena's pages. One is protected while it holds no more than that and has
// asked for a page within the arena's last turnover: as many evictions of
// mapped pages as the arena has pages.
//
// The pager sees a guest's faults and not its other accesses, so asking for a
// page is the only sign it has that a guest is using its memory. A guest whose
// working set is resident asks for nothing. It loses its protection after a
// turnover, gives up its least recently faulted page, and is protected again as
// soon as it faults on that page. So a guest that cycles through more memory
// than the arena holds evicts its own pages, and costs a neighbour within its
// share one refault a turnover rather than its working set. An idle guest's
// pages are anyone's to take, which keeps the arena in use. Caller holds h.mu.
func (h *Host) protectedLocked(q *MemoryRegion, share int) bool {
	return q.resident <= share && h.displaced-q.wanted < uint64(h.cfg.ResidentPages)
}

// takeIdleLocked locks and returns the oldest idle page it can take without
// waiting, or nil where there is none. Caller holds h.mu.
func (h *Host) takeIdleLocked() *resident { return h.takeIdleWhereLocked(nil) }

// takeIdleWhereLocked is takeIdleLocked for the idle pages want accepts, or
// any where want is nil. Caller holds h.mu.
func (h *Host) takeIdleWhereLocked(want func(*resident) bool) *resident {
	for pg := h.idle.front(); pg != nil; pg = h.idle.next(pg) {
		if want != nil && !want(pg) {
			continue
		}
		if !pg.mu.TryLock() {
			continue
		}
		if pg.aliases.len() == 0 && pg.replacing == 0 {
			return pg
		}
		pg.mu.Unlock()
	}
	return nil
}

// dropIdle gives up one idle page takeIdleLocked returned locked: its identity
// stops naming it, so the next memory region that inherits it reads it again, and its
// slot is free.
func (h *Host) dropIdle(ctx context.Context, pg *resident) error {
	err := h.release(ctx, pg)
	if err == nil {
		h.mu.Lock()
		h.stats.IdleDrops++
		h.mu.Unlock()
	}
	h.unlockAll([]*resident{pg})
	return err
}

// makeRoom gives up idle pages until want slots of f are free, or no idle
// page is left. The allocations that take free slots only — a store's
// write-ahead run, a load's read-ahead — never evict, so an arena full of idle
// pages would otherwise shrink every one of them to the single page that may.
func (h *Host) makeRoom(ctx context.Context, f *arenaFile, want int) error {
	for {
		h.mu.Lock()
		if h.freeLocked(f) >= want {
			h.mu.Unlock()
			return nil
		}
		pg := h.takeIdleLocked()
		h.mu.Unlock()
		if pg == nil {
			return nil
		}
		if err := h.dropIdle(ctx, pg); err != nil {
			return err
		}
	}
}

// reclaimIdle is the host budget's cache eviction for this pager: it gives up
// one idle page, reporting whether it did. The budget calls it for whichever
// consumer is short, and that may be this pager, from inside an allocation
// that holds h.mu, or the other pager of this host from inside one of its own,
// which is why it never waits for h.mu: an allocation of this pager gives up
// idle pages itself, and another consumer that finds the lock taken waits for
// the budget's next release, which a pager this busy is about to make.
func (h *Host) reclaimIdle(ctx context.Context, _ int64) (bool, error) {
	if !h.mu.TryLock() {
		return false, nil
	}
	pg := h.takeIdleLocked()
	h.mu.Unlock()
	if pg == nil {
		return false, nil
	}
	return true, h.dropIdle(ctx, pg)
}

// DropIdle gives up every idle page this host can take without waiting, and
// reports how many it gave up. A host that wants its memory back rather than
// kept for the next VM to inherit calls it; so does a test whose machine must
// fault every page from scratch.
func (h *Host) DropIdle(ctx context.Context) (int, error) {
	dropped := 0
	for {
		h.mu.Lock()
		pg := h.takeIdleLocked()
		h.mu.Unlock()
		if pg == nil {
			return dropped, nil
		}
		if err := h.dropIdle(ctx, pg); err != nil {
			return dropped, err
		}
		dropped++
	}
}

// reclaimExtent gives up idle pages until an extent of f is free, where none
// is, and reports whether it freed one. A published page stays at the slot the
// placement rule gave it when it goes idle, so it keeps that slot's extent from
// going back after the memory region that placed it has gone: a file full of
// the idle pages of stopped VMs would otherwise have no extent left for a
// running one, and every private page of it would be a page of its own
// somewhere in the file.
func (h *Host) reclaimExtent(ctx context.Context, f *arenaFile) (bool, error) {
	orphaned := func(pg *resident) bool {
		e := pg.file.leases[pg.slot].extent
		return e != nil && e.file == f && h.extents[e.key] != e
	}
	for {
		h.mu.Lock()
		if !h.carving(f) || f.slots.FreeExtents() > 0 {
			freed := h.carving(f) && f.slots.FreeExtents() > 0
			h.mu.Unlock()
			return freed, nil
		}
		pg := h.takeIdleWhereLocked(orphaned)
		h.mu.Unlock()
		if pg == nil {
			return false, nil
		}
		if err := h.dropIdle(ctx, pg); err != nil {
			return false, err
		}
	}
}
