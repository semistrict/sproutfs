package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/slots"
	"lukechampine.com/blake3"
)

// An isolated arena splits the pages by who may read them, so that a VMM,
// which may be compromised, reaches no other VM's memory through the files it
// is given.
//
//   - A memory region's private file holds its private pages: dirty, sealed,
//     written ahead, spilled back in, and loaded privately. Only its VMM
//     receives it, read-write. A page's offset in it is its index, and the
//     second half of the file is each page's other place, for a store that
//     cannot use the page's own offset.
//   - A tenant's shared file holds the pages another memory region of the
//     tenant may map: pages loaded by identity, and published pages once
//     another region inherits them. The tenant's VMMs receive it read-only.
//     A page's identity names its tenant, and a memory region is refused an
//     identity of another tenant, so no page is ever in two tenants' files.
//   - A fork file holds the pages a fork point lends, copied there the first
//     time a child on this host maps one. The children receive it read-only.
//
// A published page stays in the private file it was published in, and the
// guest keeps mapping it there. The first time another memory region inherits
// it, the pager copies it into the shared file and checks the copy against the
// digest of the bytes its upload read. Only the owner's VMM can have changed
// it, and it holds the page read-only, so a copy that differs ends that VMM's
// session. A private slot only ever holds its own region's pages and a shared
// slot only pages its tenant may read, so a mapping a VMM keeps past a
// revocation reaches nothing its descriptors do not already reach.

// isolated reports an arena split by who may read each page.
func (h *Host) isolated() bool { return h.cfg.Arena == ArenaIsolated }

// digest is what a page's bytes hash to.
type digest = [32]byte

// digestOf is the digest of one page's bytes.
func digestOf(data []byte) digest { return blake3.Sum256(data) }

// newFiles gives a memory region the files an isolated arena keeps for it: a
// private file of its own, and its tenant's shared file.
func (h *Host) newFiles(ctx context.Context, r *MemoryRegion) error {
	if err := h.joinTenant(ctx, r); err != nil {
		return err
	}
	if err := h.newPrivateFile(ctx, r); err != nil {
		h.mu.Lock()
		h.leaveTenantLocked(r)
		h.mu.Unlock()
		return err
	}
	return nil
}

// joinTenant gives a memory region its tenant's shared file, and makes the
// file where the tenant has none: no memory region of it is attached and no
// page of it is resident.
func (h *Host) joinTenant(ctx context.Context, r *MemoryRegion) error {
	h.mu.Lock()
	joined := h.joinTenantLocked(r)
	h.mu.Unlock()
	if joined {
		return nil
	}
	file, err := h.arena.File(ctx, h.cfg.ArenaOffsets)
	if err != nil {
		return fmt.Errorf("making a tenant's shared file: %w", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.joinTenantLocked(r) {
		// Another memory region of the tenant made one first.
		h.giveBack(file)
		return nil
	}
	f := h.keepFile(file, slots.New(h.cfg.ArenaOffsets, h.cfg.ResidentPages))
	f.shared, f.tenant, f.holders = true, r.tenant, make(map[*MemoryRegion]int)
	h.shared[r.tenant] = f
	h.files = append(h.files, f)
	h.joinTenantLocked(r)
	return nil
}

// joinTenantLocked gives r its tenant's shared file, where the tenant has one.
// Caller holds h.mu.
func (h *Host) joinTenantLocked(r *MemoryRegion) bool {
	f := h.shared[r.tenant]
	if f == nil {
		return false
	}
	f.holders[r] = sharedFileNumber
	f.orphaned = false
	r.shared = f
	return true
}

// leaveTenantLocked is what a memory region's detach does to its tenant's
// shared file: the file outlives the tenant's last memory region while it
// holds idle pages, and goes back with the last of them. Caller holds h.mu.
func (h *Host) leaveTenantLocked(r *MemoryRegion) {
	f := r.shared
	if f == nil {
		return
	}
	delete(f.holders, r)
	f.orphaned = len(f.holders) == 0
	h.emptiedLocked(f)
}

// newPrivateFile makes a memory region's private file: a place of its own for
// each of its pages and a second one beside it. Only the pager and the
// region's own VMM ever hold it.
func (h *Host) newPrivateFile(ctx context.Context, r *MemoryRegion) error {
	offsets := 2 * r.pageCount
	file, err := h.arena.File(ctx, offsets)
	if err != nil {
		return fmt.Errorf("making the private file of a memory region: %w", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.keepFile(file, slots.NewSparse(offsets, min(h.cfg.ResidentPages, offsets)))
	f.owner, f.pages, f.digests = r, make(map[int]*resident), make(map[int]digest)
	h.files = append(h.files, f)
	r.private = f
	return nil
}

// emptiedLocked gives a file back once it has nothing left to hold: a file
// nothing will be given again whose last page has gone. Caller holds h.mu.
func (h *Host) emptiedLocked(f *arenaFile) {
	if f.orphaned && f.slots.Held() == 0 {
		h.dropFileLocked(f)
	}
}

// dropFileLocked stops keeping a file and gives it back to its arena. Caller
// holds h.mu, and nothing of the file is held.
func (h *Host) dropFileLocked(f *arenaFile) {
	h.files = slices.DeleteFunc(h.files, func(other *arenaFile) bool { return other == f })
	if f.shared && h.shared[f.tenant] == f {
		delete(h.shared, f.tenant)
	}
	h.giveBack(f.ArenaFile)
}

// giveBack gives a file the pager keeps nothing in back to its arena.
func (h *Host) giveBack(file ArenaFile) {
	if closing, ok := file.(ClosableFile); ok {
		if err := closing.Close(); err != nil {
			slog.Warn("vmmemory: an arena file could not be given back", "error", err)
		}
	}
}

// ownPlaces is where a page of r's private file may be, in the order a page
// takes them: its own offset and then its other place, or the reverse for a
// clean page, which leaves its own offset to the private copy a store makes.
func (r *MemoryRegion) ownPlaces(index uint64, clean bool) [2]fileSlot {
	home := fileSlot{r.private, int(index)}
	other := fileSlot{r.private, r.pageCount + int(index)}
	if clean {
		return [2]fileSlot{other, home}
	}
	return [2]fileSlot{home, other}
}

// takeOwnLocked takes one place of page index in r's private file without
// evicting, reporting the place or slot -1. Its own offset is taken by the
// placement rule, so that the page is part of its range's run. Caller holds
// h.mu.
func (h *Host) takeOwnLocked(r *MemoryRegion, index uint64, at fileSlot) fileSlot {
	if at.slot == int(index) {
		taken, _ := h.place(r, index)
		return taken
	}
	if at.file.slots.IsFree(at.slot) && h.takeFree(at, 1) {
		return at
	}
	return fileSlot{at.file, -1}
}

// allocateOwn takes a place of page index in r's private file, evicting where
// the page budget rather than the place is what is missing. A page never needs
// a third place: where both are taken, one holds a clean page the guest no
// longer maps, which is given up.
func (r *MemoryRegion) allocateOwn(ctx context.Context, index uint64, clean bool) (fileSlot, error) {
	h := r.host
	places := r.ownPlaces(index, clean)
	// Taking a victim while the place is free is legal and merely wasteful, and
	// it is how a pager sized to hold its whole guest reaches the eviction paths
	// at all.
	preferEviction := sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5)
	for range loadAttempts {
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return fileSlot{}, err
		}
		for _, at := range places {
			if preferEviction {
				break
			}
			if taken := h.takeOwnLocked(r, index, at); taken.slot >= 0 {
				h.mu.Unlock()
				return taken, nil
			}
		}
		var free *fileSlot
		for _, at := range places {
			if _, held := at.file.leases[at.slot]; !held {
				free = &at
				break
			}
		}
		var idle *resident
		if free == nil {
			idle = h.idleOwnLocked(places)
		}
		h.mu.Unlock()
		if free != nil {
			// The place is this page's; what is missing is a page of the budget,
			// which an eviction anywhere gives back.
			at := *free
			return h.allocate(ctx, r, at.file, func() int { return h.takeOwnLocked(r, index, at).slot }, preferEviction)
		}
		if idle == nil {
			return fileSlot{}, fmt.Errorf("%w: both places of page %d of a memory region hold a page it maps", ErrCapacity, index)
		}
		if err := idle.mu.Lock(ctx); err != nil {
			return fileSlot{}, err
		}
		h.mu.Lock()
		still := idle.slot >= 0 && idle.aliases.len() == 0 && idle.replacing == 0
		h.mu.Unlock()
		if !still {
			h.unlock(idle)
			continue
		}
		if err := h.dropIdle(ctx, idle); err != nil {
			return fileSlot{}, err
		}
	}
	return fileSlot{}, ErrContended
}

// idleOwnLocked is the page at one of the two places that no memory region
// maps, nil where both are mapped. Caller holds h.mu.
func (h *Host) idleOwnLocked(places [2]fileSlot) *resident {
	for _, at := range places {
		if pg := at.file.pages[at.slot]; pg != nil && pg.aliases.len() == 0 && pg.replacing == 0 {
			return pg
		}
	}
	return nil
}

// reclaimOwn is allocateOwn with the memory region given up, as every
// allocation that may evict is.
func (r *MemoryRegion) reclaimOwn(ctx context.Context, index uint64, clean bool) (fileSlot, error) {
	return r.reclaimWith(ctx, func() (fileSlot, error) { return r.allocateOwn(ctx, index, clean) })
}

// own reports a page an isolated arena loads into this memory region's own
// file rather than the shared one: one whose bytes are no identity another
// memory region may inherit, one a fork point lends to its children, and one
// another host still holds.
func (p *windowPlan) own(page uint64) bool {
	h := p.memoryRegion.host
	if !h.isolated() {
		return false
	}
	id, named := p.identity(page)
	if named && id.zero() {
		return false
	}
	return !named || p.unpublished(page) || h.lends(id)
}

// reserveOwn takes places in this memory region's own file for the pages of
// the window it loads there, without evicting.
func (p *windowPlan) reserveOwn() {
	r := p.memoryRegion
	h := r.host
	for page := p.start; page < p.end; page++ {
		i := page - p.start
		if p.pages[i] != nil || p.zeros[i] || p.reserved[i].slot >= 0 || !p.own(page) || !p.eligible(page) {
			continue
		}
		h.mu.Lock()
		for _, at := range r.ownPlaces(page, !p.unpublished(page)) {
			if taken := h.takeOwnLocked(r, page, at); taken.slot >= 0 {
				p.reserve(page, taken)
				break
			}
		}
		h.mu.Unlock()
	}
}

// ownInstead moves a page's reservation from the shared file to this memory
// region's own file, which a page the load found another host's needs. It
// takes nothing it cannot have at once.
func (p *windowPlan) ownInstead(page uint64) (fileSlot, bool) {
	r := p.memoryRegion
	h := r.host
	i := page - p.start
	h.mu.Lock()
	defer h.mu.Unlock()
	h.putFree(p.reserved[i])
	p.reserved[i] = fileSlot{slot: -1}
	for _, at := range r.ownPlaces(page, false) {
		if taken := h.takeOwnLocked(r, page, at); taken.slot >= 0 {
			return taken, true
		}
	}
	return fileSlot{}, false
}

// lentKey names what one fork point lends: the pages of one volume, under the
// reference that point took.
type lentKey struct {
	ref    control.Ref
	volume string
}

// lends reports an identity a fork point lends to its children on this host.
func (h *Host) lends(key pageKey) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lent[lentKey{key.id.Ref, key.id.Volume}] != nil
}

// reach is a resident page this memory region may map in place of pg, which
// holds the same identity and which the caller holds locked. It is pg itself
// where this region's process may read pg's file. Otherwise pg is in another
// memory region's private file: a page a fork point lends is copied into that
// point's file, and a published page into the shared file, and the copy comes
// back locked in pg's place. Where neither can be, it reports nil, pg is
// unlocked, and the page is read from this region's own volume.
func (r *MemoryRegion) reach(ctx context.Context, pg *resident, key pageKey) (*resident, error) {
	h := r.host
	f := pg.file
	if !h.isolated() || f == r.private || f == r.shared {
		return pg, nil
	}
	if f.shared {
		// Another tenant's page, which no identity this region is given names.
		h.unlock(pg)
		return nil, fmt.Errorf("%w: a %s memory region of tenant %q reached tenant %q's shared file",
			ErrOtherTenant, r.kind, r.tenant, f.tenant)
	}
	if f.owner == nil {
		// A fork point's file, which a child of that point may read.
		if err := r.giveFork(ctx, f); err != nil {
			h.unlock(pg)
			return nil, err
		}
		return pg, nil
	}
	if pg.private {
		return r.forkCopy(ctx, pg, key)
	}
	return r.move(ctx, pg, key)
}

// giveFork hands this memory region's process a fork point's file, once,
// before anything is mapped from it.
func (r *MemoryRegion) giveFork(ctx context.Context, f *arenaFile) error {
	h := r.host
	if err := r.filesMu.Lock(ctx); err != nil {
		return err
	}
	defer r.filesMu.Unlock()
	h.mu.Lock()
	if _, given := r.forks[f]; given {
		h.mu.Unlock()
		return nil
	}
	number := sharedFileNumber + 1
	for _, used := range r.forks {
		number = max(number, used+1)
	}
	h.mu.Unlock()
	if err := r.mapping.GiveFile(ctx, number, f.ArenaFile, false); err != nil {
		return r.fail(err)
	}
	h.mu.Lock()
	if r.forks == nil {
		r.forks = make(map[*arenaFile]int)
	}
	r.forks[f] = number
	f.holders[r] = number
	h.mu.Unlock()
	return nil
}

// forkCopy copies a page a fork point lends into that point's file, at the
// page's own index, so that a child on this host maps the copy and never the
// parent's private file. Every later child finds it there. The parent keeps
// its own page and is never remapped.
func (r *MemoryRegion) forkCopy(ctx context.Context, pg *resident, key pageKey) (*resident, error) {
	h := r.host
	h.mu.Lock()
	c := h.lent[lentKey{key.id.Ref, key.id.Volume}]
	h.mu.Unlock()
	if c == nil {
		h.unlock(pg)
		return nil, nil
	}
	fork, err := c.forkFile(ctx)
	if err != nil {
		h.unlock(pg)
		return nil, err
	}
	at := fileSlot{fork, int(key.id.Page)}
	h.mu.Lock()
	taken := at.slot < fork.slots.Offsets() && fork.slots.IsFree(at.slot) && h.takeFree(at, 1)
	h.mu.Unlock()
	if !taken {
		h.unlock(pg)
		return nil, nil
	}
	data := make([]byte, h.pageSize)
	if err := pg.file.Read(ctx, pg.slot, data); err != nil {
		h.unlock(pg)
		return nil, errors.Join(err, h.abandonSlots(ctx, at, 1, nil))
	}
	copied, err := h.create(ctx, at, data, key, true, pg.kind)
	if err != nil {
		h.unlock(pg)
		return nil, err
	}
	h.mu.Lock()
	if h.clean[key] == pg {
		h.clean[key] = copied
		h.cleanVersion++
	}
	h.stats.ForkCopies++
	h.mu.Unlock()
	h.unlock(pg)
	if err := r.giveFork(ctx, fork); err != nil {
		// The copy is the point's now, for its next child; this region is
		// terminal.
		h.unlock(copied)
		return nil, err
	}
	return copied, nil
}

// move copies a published page out of the private file it was published in
// and into its tenant's shared file, because another memory region of the
// tenant inherits it. The copy is checked against the digest of the bytes the
// page's upload read: the owner's VMM holds the page read-only, so a copy that
// differs is a VMM that wrote where it may not, and its session ends. The
// owner's mapping of the page is taken away, and its next fault maps the copy.
//
// A page that cannot be moved at once — there is no free slot of the shared
// file, or no digest — stops being named by its identity, and the region that
// wants it reads it from its own volume. The owner is of this region's tenant,
// because the page's identity names that tenant.
func (r *MemoryRegion) move(ctx context.Context, pg *resident, key pageKey) (*resident, error) {
	h := r.host
	owner := pg.file.owner
	h.mu.Lock()
	sum, digested := pg.file.digests[pg.slot]
	h.mu.Unlock()
	var at fileSlot
	count := 0
	if digested {
		if err := h.makeRoom(ctx, r.shared, 1); err != nil {
			h.unlock(pg)
			return nil, err
		}
		at, count = h.allocateFree(r.shared, 1)
	}
	if count == 0 {
		h.mu.Lock()
		h.unindexLocked(pg)
		h.mu.Unlock()
		h.unlock(pg)
		return nil, nil
	}
	data := make([]byte, h.pageSize)
	if err := pg.file.Read(ctx, pg.slot, data); err != nil {
		h.unlock(pg)
		return nil, errors.Join(err, h.abandonSlots(ctx, at, 1, nil))
	}
	if digestOf(data) != sum {
		h.mu.Lock()
		h.unindexLocked(pg)
		h.stats.Tampered++
		h.mu.Unlock()
		h.unlock(pg)
		err := fmt.Errorf("%w: page %d of a %s memory region", ErrTampered, key.id.Page, owner.kind)
		slog.ErrorContext(ctx, "vmmemory: a VMM wrote a page it holds read-only", "error", err)
		owner.fail(err)
		return nil, h.abandonSlots(ctx, at, 1, nil)
	}
	copied, err := h.create(ctx, at, data, key, false, pg.kind)
	if err != nil {
		h.unlock(pg)
		return nil, err
	}
	h.mu.Lock()
	if h.clean[key] == pg {
		h.clean[key] = copied
		h.cleanVersion++
	}
	delete(pg.file.digests, pg.slot)
	h.stats.MovedPages++
	h.mu.Unlock()
	if err := h.rebind(ctx, pg, copied); err != nil {
		h.unlock(pg)
		h.unlock(copied)
		return nil, err
	}
	h.unlock(pg)
	return copied, nil
}

// rebind takes every mapping of from away and binds each of its aliases to to,
// which holds the same bytes, and then gives from up. It is what moving a page
// does to the memory region that mapped it: a revocation, which the next fault
// of that region answers with the copy. A region that cannot take its mapping
// away keeps the page it has, which is then its alone. Caller holds both pages'
// locks.
func (h *Host) rebind(ctx context.Context, from, to *resident) error {
	byRegion := make(map[*MemoryRegion][]*binding)
	for _, b := range h.aliases(from) {
		byRegion[b.memoryRegion] = append(byRegion[b.memoryRegion], b)
	}
	for q, bindings := range byRegion {
		if err := q.revokeBindings(ctx, bindings); err != nil {
			q.heldPages(ctx, err)
			continue
		}
		for _, b := range bindings {
			if err := h.unlink(ctx, b, from); err != nil {
				return err
			}
			h.bind(b, to)
		}
	}
	h.mu.Lock()
	idle := from.slot >= 0 && from.aliases.len() == 0
	if idle && from.replacing > 0 {
		// A store of the owner is replacing its mapping of the page, and the
		// memory goes back when that command lands.
		from.dropped, idle = true, false
	}
	h.mu.Unlock()
	if idle {
		return h.release(ctx, from)
	}
	return nil
}

// unindexLocked stops a page being named by its identity. It stays the page of
// whatever maps it. Caller holds h.mu.
func (h *Host) unindexLocked(pg *resident) {
	if pg.key != (pageKey{}) && h.clean[pg.key] == pg {
		delete(h.clean, pg.key)
		h.cleanVersion++
	}
}

// forkFile is the file this checkpoint lends its pages to children on this
// host in, made the first time one is copied there. It has a slot for each
// page of the memory region the checkpoint was taken of.
func (c *MemoryRegionCheckpoint) forkFile(ctx context.Context) (*arenaFile, error) {
	h := c.memoryRegion.host
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fork != nil {
		return c.fork, nil
	}
	offsets := c.memoryRegion.pageCount
	file, err := h.arena.File(ctx, offsets)
	if err != nil {
		return nil, fmt.Errorf("making a fork point's file: %w", err)
	}
	h.mu.Lock()
	f := h.keepFile(file, slots.NewSparse(offsets, min(h.cfg.ResidentPages, offsets)))
	f.pages, f.holders = make(map[int]*resident), make(map[*MemoryRegion]int)
	h.files = append(h.files, f)
	h.mu.Unlock()
	c.fork = f
	return f, nil
}

// endFork takes back what a checkpoint lent when its seal ends: the name goes,
// every child's mapping of a copy in the fork file is taken away, the copies
// are given up, each child closes the file, and the file goes back to the
// arena. A child reads such a page from its own backing from then on.
func (c *MemoryRegionCheckpoint) endFork(ctx context.Context) error {
	h := c.memoryRegion.host
	h.mu.Lock()
	for key, lender := range h.lent {
		if lender == c {
			delete(h.lent, key)
		}
	}
	h.mu.Unlock()
	c.mu.Lock()
	f := c.fork
	c.fork = nil
	c.mu.Unlock()
	if f == nil {
		return nil
	}
	h.mu.Lock()
	var pages []*resident
	for _, pg := range f.pages {
		pages = append(pages, pg)
	}
	h.mu.Unlock()
	for _, pg := range pages {
		if err := pg.mu.Lock(ctx); err != nil {
			return err
		}
		err := h.dropSharers(ctx, pg)
		if err == nil && pg.slot >= 0 {
			err = h.release(ctx, pg)
		}
		h.unlock(pg)
		if err != nil {
			return err
		}
	}
	h.mu.Lock()
	holders := f.holders
	f.holders = nil
	for q := range holders {
		delete(q.forks, f)
	}
	h.mu.Unlock()
	for q, number := range holders {
		if q.closed || q.terminal.Load() != nil {
			continue
		}
		if err := q.mapping.DropFile(ctx, number); err != nil {
			q.fail(err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if f.slots.Held() == 0 {
		h.dropFileLocked(f)
	} else {
		// A copy a revocation could not take away belongs to a terminal
		// memory region, and the file goes back once that region is closed.
		f.orphaned = true
	}
	return nil
}

// digestOf is what the publication's read of one page hashed to, nil where it
// read none.
func (c *MemoryRegionCheckpoint) digestOf(page uint64) *digest {
	c.mu.Lock()
	defer c.mu.Unlock()
	sum, ok := c.digests[page]
	if !ok {
		return nil
	}
	return &sum
}

// forgetFilesLocked is what detaching a memory region does to the files: its
// private file is given back once its idle pages have gone, and so is its
// tenant's shared file once no memory region of the tenant is left; what its
// reads made either allocate goes back now; and it holds no fork point's file
// any more. Caller holds h.mu.
func (h *Host) forgetFilesLocked(r *MemoryRegion) {
	for f := range r.forks {
		delete(f.holders, r)
	}
	r.forks = nil
	if f := r.private; f != nil {
		if err := errors.Join(h.punchUnheldLocked(f), h.punchUnheldLocked(r.shared)); err != nil {
			slog.Warn("vmmemory: memory a detached VMM allocated could not be given back", "error", err)
		}
		f.orphaned = true
		h.emptiedLocked(f)
		h.leaveTenantLocked(r)
	}
}

// countAllocated ends this memory region's session where its private file
// holds more memory than the pager put there, which only its VMM can have
// done, and gives back whatever a VMM's reads made its tenant's shared file
// allocate. It is part of every verification.
func (r *MemoryRegion) countAllocated() error {
	h := r.host
	if r.private == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if over, err := h.overLocked(r.private); err != nil || over {
		if err == nil {
			err = fmt.Errorf("%w: a %s memory region's private file", ErrUncounted, r.kind)
		}
		return r.fail(err)
	}
	return h.punchUnheldLocked(r.shared)
}

// overLocked reports a file that holds more memory than the pages the pager
// put there. The two are read together under h.mu: the pager counts a slot
// before it allocates it and gives it back after it punches it, so a file the
// pager alone has touched never holds more than it counts. Caller holds h.mu.
func (h *Host) overLocked(f *arenaFile) (bool, error) {
	counted, ok := f.ArenaFile.(CountedFile)
	if !ok {
		return false, nil
	}
	allocated, err := counted.AllocatedBytes()
	if err != nil {
		return false, err
	}
	return allocated > uint64(f.slots.Held())*h.pageSize, nil
}

// punchUnheldLocked gives back the memory of every slot of a file that holds
// no page, where the file holds more than its pages. Caller holds h.mu.
func (h *Host) punchUnheldLocked(f *arenaFile) error {
	over, err := h.overLocked(f)
	if err != nil || !over {
		return err
	}
	return f.ArenaFile.(CountedFile).Punch(func(slot int) bool {
		_, held := f.leases[slot]
		return held
	})
}
