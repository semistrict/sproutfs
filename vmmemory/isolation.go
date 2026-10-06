package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/semistrict/sproutfs/control"
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
	number := publicFileNumber + 1
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
