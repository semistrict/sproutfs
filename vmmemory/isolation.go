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
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
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
//   - The public file holds the pages of public templates (control.Public),
//     loaded by their identity. Every VMM receives it read-only, whatever its
//     tenant. Only a page whose identity is a public template's is ever in
//     it, and nothing a guest writes is ever named by one.
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
// private file of its own, its tenant's shared file and the public file.
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

// publicShared is the key of the public file among the shared files. No
// tenant is named it, because a tenant's name has no parentheses.
const publicShared = "(public)"

// joinTenant gives a memory region its tenant's shared file and the public
// file, and makes either where there is none: no memory region that holds it
// is attached and no page of it is resident.
func (h *Host) joinTenant(ctx context.Context, r *MemoryRegion) error {
	shared, err := h.joinShared(ctx, r, r.tenant, sharedFileNumber)
	if err != nil {
		return fmt.Errorf("making a tenant's shared file: %w", err)
	}
	public, err := h.joinShared(ctx, r, publicShared, publicFileNumber)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.leaveSharedLocked(r, shared)
		return fmt.Errorf("making the public file: %w", err)
	}
	r.shared, r.public = shared, public
	return nil
}

// joinShared gives r the shared file of one key, under number, and makes it
// where there is none.
func (h *Host) joinShared(ctx context.Context, r *MemoryRegion, key string, number int) (*arenaFile, error) {
	h.mu.Lock()
	f := h.joinSharedLocked(r, key, number)
	h.mu.Unlock()
	if f != nil {
		return f, nil
	}
	// While h.mu is released another memory region may make the key's file,
	// join it or leave it, so the look is made again under the hold that keeps
	// the new file, and a key never has two.
	file, err := h.arena.File(ctx, h.cfg.ArenaOffsets)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if f := h.joinSharedLocked(r, key, number); f != nil {
		// Another memory region made one first.
		h.giveBack(file)
		return f, nil
	}
	f = h.keepFile(file, slots.New(h.cfg.ArenaOffsets, h.cfg.ResidentPages))
	f.shared, f.tenant, f.holders = true, key, make(map[*MemoryRegion]int)
	h.shared[key] = f
	h.files = append(h.files, f)
	return h.joinSharedLocked(r, key, number), nil
}

// joinSharedLocked gives r the shared file of one key, where there is one.
// Caller holds h.mu.
func (h *Host) joinSharedLocked(r *MemoryRegion, key string, number int) *arenaFile {
	f := h.shared[key]
	if f == nil {
		return nil
	}
	f.holders[r] = number
	f.orphaned = false
	return f
}

// leaveTenantLocked is what a memory region's detach does to its tenant's
// shared file and the public file: each outlives the last memory region that
// holds it while it holds idle pages, and goes back with the last of them.
// Caller holds h.mu.
func (h *Host) leaveTenantLocked(r *MemoryRegion) {
	h.leaveSharedLocked(r, r.shared)
	h.leaveSharedLocked(r, r.public)
}

// leaveSharedLocked takes r off one shared file. Caller holds h.mu.
func (h *Host) leaveSharedLocked(r *MemoryRegion, f *arenaFile) {
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
	f.owner, f.frames, f.digests = r, make(map[int]*zirconvm.VmPage), make(map[int]digest)
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
	return h.lendsLocked(key)
}

// lendsLocked is lends with h.mu held.
func (h *Host) lendsLocked(key pageKey) bool {
	return h.lent[lentKey{key.id.Ref, key.id.Volume}] != nil
}

// mapsLocked reports whether this memory region's process may map a page of
// f as it is, with no copy: any page of a shared arena, and in an isolated one
// a page of its own file, its tenant's shared file, the public file or a fork
// point's file it was given. Caller holds h.mu.
func (r *MemoryRegion) mapsLocked(f *arenaFile) bool {
	if !r.host.isolated() || f == r.private || f == r.shared || f == r.public {
		return true
	}
	_, given := r.forks[f]
	return given
}

// giveFork hands this memory region's process a fork point's file, once,
// before anything is mapped from it.
func (r *MemoryRegion) giveFork(ctx context.Context, f *arenaFile) error {
	if err := r.filesMu.Lock(ctx); err != nil {
		return err
	}
	defer r.filesMu.Unlock()
	number, given := r.forkNumber(f)
	if given {
		return nil
	}
	// filesMu keeps every other give out while h.mu is released, so f is
	// still not given and no give takes number. The seal of f's point cannot
	// end meanwhile and take f away: the caller holds a page of f the point
	// keeps in its root's copies, and the end waits for each of them before
	// it takes the file back (Host.dropLentRoot). The end of another point's
	// seal can: it frees its number here before its DROP_FILE lands, and this
	// may give that number again (TASK-108).
	if err := sim.Admit(ctx, "vmmemory/give-fork"); err != nil {
		return err
	}
	if err := r.mapping.GiveFile(ctx, number, f.ArenaFile, false); err != nil {
		return r.fail(err)
	}
	r.keepFork(f, number)
	return nil
}

// forkNumber is the number a fork point's file is given to this region's
// process under: the one it was given, or the next one free.
func (r *MemoryRegion) forkNumber(f *arenaFile) (number int, given bool) {
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if number, given := r.forks[f]; given {
		return number, true
	}
	number = publicFileNumber + 1
	for _, used := range r.forks {
		number = max(number, used+1)
	}
	return number, false
}

// keepFork records that this region's process holds a fork point's file
// under number.
func (r *MemoryRegion) keepFork(f *arenaFile, number int) {
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.forks == nil {
		r.forks = make(map[*arenaFile]int)
	}
	r.forks[f] = number
	f.holders[r] = number
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
	f.frames, f.holders = make(map[int]*zirconvm.VmPage), make(map[*MemoryRegion]int)
	h.files = append(h.files, f)
	h.mu.Unlock()
	c.fork = f
	return f, nil
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
// tenant's shared file once no memory region of the tenant is left, and the
// public file once no memory region is; what its
// reads made either allocate goes back now; and it holds no fork point's file
// any more. Caller holds h.mu.
func (h *Host) forgetFilesLocked(r *MemoryRegion) {
	for f := range r.forks {
		delete(f.holders, r)
	}
	r.forks = nil
	if f := r.private; f != nil {
		if err := errors.Join(h.punchUnheldLocked(f), h.punchUnheldLocked(r.shared),
			h.punchUnheldLocked(r.public)); err != nil {
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
	return errors.Join(h.punchUnheldLocked(r.shared), h.punchUnheldLocked(r.public))
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

// Which object a page belongs to is Zircon's; the file and slot it sits in,
// and what each VMM is given, are the isolated arena's. A page a region cannot
// map where it is, because it is in another region's private file, is moved
// into the file its identity's pages live in, with the digest of its upload
// checked, or for a page a fork point lends, copied into that point's file;
// the root then holds the copy in its place.

// reachable reports whether this region's process may map page where it is.
func (r *MemoryRegion) reachable(page *zirconvm.VmPage) bool {
	if !r.host.isolated() {
		return true
	}
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return r.mapsLocked(frameOf(page).file)
}

// reach is a page this region may map in place of page, a root's page holding
// key's bytes, which the caller holds locked. It is page itself where this
// region's process may read its file; a fork point's file is given to the
// process first. A page another region's private file holds is moved or copied
// out of it, and the copy comes back locked in its place, page unlocked. Where
// neither can be, it reports nil, page unlocked, and the region reads its own
// copy.
func (r *MemoryRegion) reach(ctx context.Context, page *zirconvm.VmPage, key pageKey) (*zirconvm.VmPage, error) {
	h := r.host
	f := frameOf(page)
	if !h.isolated() || f.file == r.private || f.file == r.shared || f.file == r.public {
		return page, nil
	}
	if f.file.shared {
		r.host.unlockPage(page)
		return nil, fmt.Errorf("%w: a %s memory region of tenant %q reached tenant %q's shared file",
			ErrOtherTenant, r.kind, r.tenant, f.file.tenant)
	}
	if f.file.owner == nil {
		// A fork point's file, which a child of that point may read.
		if err := r.giveFork(ctx, f.file); err != nil {
			r.host.unlockPage(page)
			return nil, err
		}
		return page, nil
	}
	if isLent(page) {
		return r.forkCopy(ctx, page, key)
	}
	return r.move(ctx, page, key)
}

// forkCopy copies a page a fork point lends into that point's file, at the
// page's own index, so a child on this host maps the copy and never its
// parent's private file. The copy takes the lent page's place in the point's
// temporary root, for every later child. Caller holds lent, a page of that
// root naming its parent's frame.
func (r *MemoryRegion) forkCopy(ctx context.Context, lent *zirconvm.VmPage, key pageKey) (*zirconvm.VmPage, error) {
	h := r.host
	parent := frameOf(lent)
	h.mu.Lock()
	c := h.lent[lentKey{key.id.Ref, key.id.Volume}]
	root := r.host.roots[rootOf(key)]
	h.mu.Unlock()
	if c == nil || root == nil {
		r.host.unlockPage(lent)
		return nil, nil
	}
	// The point's seal cannot end before this copy is done: the end takes each
	// page's lent name away under that page's lock (Host.unlend), and lent is
	// held to the end of the copy, so the end waits for it, and a fault that
	// takes lent's lock after the name went no longer finds it. So c still
	// lends root's pages under each hold here, and the copy joins root.copies
	// before endFork takes them. Before unlend the name went only at endFork,
	// a seal could end inside a copy, and the copy looked again whether its
	// point still lent; that look and its guards went with TASK-108.
	if err := sim.Admit(ctx, "vmmemory/fork-file"); err != nil {
		r.host.unlockPage(lent)
		return nil, err
	}
	fork, err := c.forkFile(ctx)
	if err != nil {
		r.host.unlockPage(lent)
		return nil, err
	}
	at := fileSlot{fork, int(key.id.Page)}
	h.mu.Lock()
	taken := at.slot < fork.slots.Offsets() && fork.slots.IsFree(at.slot) && h.takeFree(at, 1)
	h.mu.Unlock()
	if !taken {
		r.host.unlockPage(lent)
		return nil, nil
	}
	data := make([]byte, h.pageSize)
	if err := parent.file.Read(ctx, parent.slot, data); err != nil {
		r.host.unlockPage(lent)
		return nil, errors.Join(err, h.abandonSlots(ctx, at, 1, nil))
	}
	copied, err := r.host.newFrame(ctx, at, data, parent.kind)
	if err != nil {
		r.host.unlockPage(lent)
		return nil, err
	}
	// A copy made for a root counts as one of its pages from the start, so
	// releaseFrame counts it out right whichever way it goes back.
	h.mu.Lock()
	r.host.rootPages++
	h.mu.Unlock()
	if forkCopySeam != nil {
		forkCopySeam(key.id.Page)
	}
	if err := sim.Admit(ctx, "vmmemory/fork-copy"); err != nil {
		r.host.releaseFrame(copied)
		r.host.unlockPage(copied)
		r.host.unlockPage(lent)
		return nil, err
	}
	// The copy takes the lent page's place in the root.
	offset := key.id.Page * h.pageSize
	lock := root.pages.Lock()
	lock.Lock()
	removed := root.pages.RemovePageLocked(offset, lent)
	lock.Unlock()
	if !removed || !r.host.supplyIfEmpty(ctx, root.pages, key.id.Page, copied) {
		r.host.releaseFrame(copied)
		r.host.unlockPage(copied)
		r.host.unlockPage(lent)
		return nil, nil
	}
	h.mu.Lock()
	if parent.lent == lent {
		parent.lent = nil
	}
	for i, p := range root.lentPages {
		if p == lent {
			root.lentPages = append(root.lentPages[:i], root.lentPages[i+1:]...)
			break
		}
	}
	root.copies = append(root.copies, copied)
	h.stats.ForkCopies++
	h.mu.Unlock()
	r.host.unlockPage(lent)
	if err := r.giveFork(ctx, fork); err != nil {
		// The copy is the point's now, for its next child; this region is
		// terminal.
		r.host.unlockPage(copied)
		return nil, err
	}
	return copied, nil
}

// forkCopySeam runs in a fork copy between its making the copy and the copy's
// taking the lent page's place, so a test can start the end of the point's
// seal there.
var forkCopySeam func(page uint64)

// move copies a published page in the private file it was published in into
// the file its identity's pages live in, because another region inherits it,
// checked against the digest of the bytes its upload read. The copy takes its
// place in its root, the owner's mapping of it is replaced by one of the copy,
// and its slot goes back. A page that cannot be moved at once stops being its
// root's: it is the owner's own page again, and the region that wants it reads
// it from its own volume. Caller holds page.
func (r *MemoryRegion) move(ctx context.Context, page *zirconvm.VmPage, key pageKey) (*zirconvm.VmPage, error) {
	h := r.host
	f := frameOf(page)
	owner := f.file.owner
	target := r.loadFile(key)
	h.mu.Lock()
	sum, digested := f.file.digests[f.slot]
	h.mu.Unlock()
	// The page's lock is held from here to the end, and what that hold read
	// changes only under it: the page's slot, its digest and its place in its
	// root go only with a move, an eviction, an idle drop or an unindex of the
	// page, each of which holds it. Its owner may detach meanwhile, which
	// unindex and rebind each allow for, and the target file is held by this
	// region, so it stays.
	if err := sim.Admit(ctx, "vmmemory/move"); err != nil {
		r.host.unlockPage(page)
		return nil, err
	}
	var at fileSlot
	count := 0
	if digested {
		if err := r.host.makeRoom(ctx, target, 1); err != nil {
			r.host.unlockPage(page)
			return nil, err
		}
		at, count = h.allocateFree(target, 1)
	}
	if count == 0 {
		r.host.unindex(ctx, page, key)
		r.host.unlockPage(page)
		return nil, nil
	}
	data := make([]byte, h.pageSize)
	if err := f.file.Read(ctx, f.slot, data); err != nil {
		r.host.unlockPage(page)
		return nil, errors.Join(err, h.abandonSlots(ctx, at, 1, nil))
	}
	if digestOf(data) != sum {
		r.host.unindex(ctx, page, key)
		h.mu.Lock()
		h.stats.Tampered++
		h.mu.Unlock()
		r.host.unlockPage(page)
		err := fmt.Errorf("%w: page %d of a %s memory region", ErrTampered, key.id.Page, owner.kind)
		slog.ErrorContext(ctx, "vmmemory: a VMM wrote a page it holds read-only", "error", err)
		owner.fail(err)
		return nil, h.abandonSlots(ctx, at, 1, nil)
	}
	copied, err := r.host.newFrame(ctx, at, data, f.kind)
	if err != nil {
		r.host.unlockPage(page)
		return nil, err
	}
	// A copy made for a root counts as one of its pages from the start, so
	// releaseFrame counts it out right whichever way it goes back.
	h.mu.Lock()
	r.host.rootPages++
	h.mu.Unlock()
	root := r.host.root(rootOf(key))
	offset := key.id.Page * h.pageSize
	lock := root.pages.Lock()
	lock.Lock()
	removed := root.pages.RemovePageLocked(offset, page)
	lock.Unlock()
	if !removed || !r.host.supplyIfEmpty(ctx, root.pages, key.id.Page, copied) {
		r.host.releaseFrame(copied)
		r.host.unlockPage(copied)
		r.host.unlockPage(page)
		return nil, fmt.Errorf("vmmemory: a published page left its root while it was moved")
	}
	h.mu.Lock()
	delete(f.file.digests, f.slot)
	h.stats.MovedPages++
	h.mu.Unlock()
	if err := r.host.rebind(ctx, page, copied); err != nil {
		r.host.unlockPage(page)
		r.host.unlockPage(copied)
		return nil, err
	}
	r.host.unlockPage(page)
	return copied, nil
}

// unindex stops a published page in a private file being its identity's
// page, as Host.unindexLocked does: it leaves its root, and is its owner's
// own Clean page again where the owner still maps it, and given back where
// nothing maps it. Caller holds page.
func (h *Host) unindex(ctx context.Context, page *zirconvm.VmPage, key pageKey) {
	root := h.root(rootOf(key))
	offset := key.id.Page * h.pageSize
	lock := root.pages.Lock()
	lock.Lock()
	removed := root.pages.RemovePageLocked(offset, page)
	lock.Unlock()
	if !removed {
		return
	}
	f := frameOf(page)
	owner := f.file.owner
	h.mu.Lock()
	h.rootPages--
	h.notIdleLocked(f)
	h.mu.Unlock()
	unheld := sim.Bug(ctx, "pager-unindex-into-a-detached-owner")
	mapped := unheld && h.mapsPage(owner, f)
	if unindexSeam != nil {
		unindexSeam(key.id.Page)
	}
	admitGoingOn(ctx, "vmmemory/unindex")
	if !unheld && owner.live.TryRLock() {
		// The owner's detach is kept out from the look at its mapping to the
		// page's return to its layer, which a detach in between would have
		// destroyed. Where a detach holds it or waits for it, the page goes
		// back below, and the owner's mapping of it with it.
		defer owner.live.RUnlock()
		mapped = h.mapsPage(owner, f)
	}
	// The page's lock keeps every other change to it out: until f.layer is
	// set it is in the owner's layer and counted as no root's, and nothing
	// but the owner's detach would give it back from there.
	if mapped && h.supplyIfEmpty(ctx, owner.pages, key.id.Page, page) {
		h.mu.Lock()
		f.layer = owner
		h.mu.Unlock()
		return
	}
	if err := h.dropSharers(ctx, page); err != nil {
		slog.WarnContext(ctx, "vmmemory: a page that left its root could not be taken from its regions", "error", err)
		return
	}
	h.mu.Lock()
	f.layer = owner
	h.mu.Unlock()
	h.releaseFrame(page)
}

// unindexSeam runs in an unindex between its taking a page from its root and
// its look at the owner's mapping of it, so a test can detach the owner there.
var unindexSeam func(page uint64)

// mapsPage reports whether r maps the page of f.
func (h *Host) mapsPage(r *MemoryRegion, f *frame) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return f.mappedBy(r)
}

// admitGoingOn is an admission point on a path that must finish once it has
// begun: in a controlled run another task may go on here, and a cancelled
// caller goes on all the same once the run admits it.
func admitGoingOn(ctx context.Context, resource string) {
	if err := sim.Admit(context.WithoutCancel(ctx), resource); err != nil {
		slog.WarnContext(ctx, "vmmemory: an admission point failed", "resource", resource, "error", err)
	}
}

// rebind puts to, which holds the same bytes, in place of every mapping of
// from, and binds each of from's aliases to to. A region that can neither be
// given to nor have its mapping taken away keeps the page it has, which is
// then its alone. Caller holds both pages.
func (h *Host) rebind(ctx context.Context, from, to *zirconvm.VmPage) error {
	h.moveCold(from, to)
	byRegion := make(map[*MemoryRegion][]*binding)
	for _, b := range h.aliasesOf(frameOf(from)) {
		byRegion[b.region] = append(byRegion[b.region], b)
	}
	// Both pages' locks are held to the end, and from is in no object, so no
	// fault binds it again and nothing but a detach takes a binding off it;
	// each region's bindings are looked at again under the hold that rebinds
	// them, and from's aliases under the one that decides it is idle.
	for q, bindings := range byRegion {
		admitGoingOn(ctx, "vmmemory/rebind")
		if err := q.remap(ctx, bindings, to); err != nil {
			q.heldPages(ctx, err)
			continue
		}
		h.mu.Lock()
		for _, b := range bindings {
			if b.page == from {
				// from is in no object any more: it is never idle.
				b.page = nil
				h.forgetGoingAliasLocked(b, from)
				h.aliasLocked(b, to)
			}
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	f := frameOf(from)
	idle := f.slot >= 0 && f.aliases.len() == 0
	if idle && f.replacing > 0 {
		// A store of the owner is replacing its mapping of the page, and the
		// memory goes back when that command lands.
		f.dropped, idle = true, false
	}
	h.mu.Unlock()
	if idle {
		h.releaseFrame(from)
	}
	return nil
}

// remap maps to, read-only, wherever this region maps one of bindings,
// installed so the guest reads on without a fault; where the client refuses
// the mapping, the mappings are taken away instead.
func (r *MemoryRegion) remap(ctx context.Context, bindings []*binding, to *zirconvm.VmPage) error {
	for _, b := range bindings {
		err := r.mapInPlace(ctx, b, to)
		if errors.Is(err, ErrMappingRefused) {
			return r.revokeBindings(ctx, bindings)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
