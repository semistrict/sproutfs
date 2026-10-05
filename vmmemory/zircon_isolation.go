package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// The isolated arena over the zircon core, as isolation.go keeps it for the
// current core: which object a page belongs to changes with the port, and the
// file and slot it sits in, and what each VMM is given, do not. A page a
// region cannot map where it is, because it is in another region's private
// file, is moved into the file its identity's pages live in, with the digest
// of its upload checked, or for a page a fork point lends, copied into that
// point's file; the root then holds the copy in its place.

// reachable reports whether this region's process may map page where it is.
func (z *zirconRegion) reachable(page *zirconvm.VmPage) bool {
	r := z.region
	if !r.host.isolated() {
		return true
	}
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return r.mapsLocked(frameOf(page).file)
}

// reach is MemoryRegion.reach over the zircon core: a page this region may
// map in place of page, a root's page holding key's bytes, which the caller
// holds locked. It is page itself where this region's process may read its
// file; a fork point's file is given to the process first. A page another
// region's private file holds is moved or copied out of it, and the copy
// comes back locked in its place, page unlocked. Where neither can be, it
// reports nil, page unlocked, and the region reads its own copy.
func (z *zirconRegion) reach(ctx context.Context, page *zirconvm.VmPage, key pageKey) (*zirconvm.VmPage, error) {
	r := z.region
	h := r.host
	f := frameOf(page)
	if !h.isolated() || f.file == r.private || f.file == r.shared || f.file == r.public {
		return page, nil
	}
	if f.file.shared {
		z.host.unlockPage(page)
		return nil, fmt.Errorf("%w: a %s memory region of tenant %q reached tenant %q's shared file",
			ErrOtherTenant, r.kind, r.tenant, f.file.tenant)
	}
	if f.file.owner == nil {
		// A fork point's file, which a child of that point may read.
		if err := r.giveFork(ctx, f.file); err != nil {
			z.host.unlockPage(page)
			return nil, err
		}
		return page, nil
	}
	if isLent(page) {
		return z.forkCopy(ctx, page, key)
	}
	return z.move(ctx, page, key)
}

// forkCopy is MemoryRegion.forkCopy over the zircon core: a page a fork point
// lends is copied into that point's file, at the page's own index, so a child
// on this host maps the copy and never its parent's private file. The copy
// takes the lent page's place in the point's temporary root, for every later
// child. Caller holds lent, a page of that root naming its parent's frame.
func (z *zirconRegion) forkCopy(ctx context.Context, lent *zirconvm.VmPage, key pageKey) (*zirconvm.VmPage, error) {
	r := z.region
	h := r.host
	parent := frameOf(lent)
	h.mu.Lock()
	c := h.lent[lentKey{key.id.Ref, key.id.Volume}]
	root := z.host.roots[rootOf(key)]
	h.mu.Unlock()
	if c == nil || root == nil {
		z.host.unlockPage(lent)
		return nil, nil
	}
	fork, err := c.forkFile(ctx)
	if err != nil {
		z.host.unlockPage(lent)
		return nil, err
	}
	at := fileSlot{fork, int(key.id.Page)}
	h.mu.Lock()
	taken := at.slot < fork.slots.Offsets() && fork.slots.IsFree(at.slot) && h.takeFree(at, 1)
	h.mu.Unlock()
	if !taken {
		z.host.unlockPage(lent)
		return nil, nil
	}
	data := make([]byte, h.pageSize)
	if err := parent.file.Read(ctx, parent.slot, data); err != nil {
		z.host.unlockPage(lent)
		return nil, errors.Join(err, h.abandonSlots(ctx, at, 1, nil))
	}
	copied, err := z.host.newFrame(ctx, at, data, parent.kind)
	if err != nil {
		z.host.unlockPage(lent)
		return nil, err
	}
	// The copy takes the lent page's place in the root.
	offset := key.id.Page * h.pageSize
	lock := root.pages.Lock()
	lock.Lock()
	removed := root.pages.RemovePageLocked(offset, lent)
	lock.Unlock()
	if !removed || !z.host.supplyIfEmpty(ctx, root.pages, key.id.Page, copied) {
		z.host.releaseFrame(copied)
		z.host.unlockPage(copied)
		z.host.unlockPage(lent)
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
	z.host.rootPages++
	root.copies = append(root.copies, copied)
	h.stats.ForkCopies++
	h.mu.Unlock()
	z.host.unlockPage(lent)
	if err := r.giveFork(ctx, fork); err != nil {
		// The copy is the point's now, for its next child; this region is
		// terminal.
		z.host.unlockPage(copied)
		return nil, err
	}
	return copied, nil
}

// move is MemoryRegion.move over the zircon core: a published page in the
// private file it was published in is copied into the file its identity's
// pages live in, because another region inherits it, checked against the
// digest of the bytes its upload read. The copy takes its place in its root,
// the owner's mapping of it is replaced by one of the copy, and its slot goes
// back. A page that cannot be moved at once stops being its root's: it is the
// owner's own page again, and the region that wants it reads it from its own
// volume. Caller holds page.
func (z *zirconRegion) move(ctx context.Context, page *zirconvm.VmPage, key pageKey) (*zirconvm.VmPage, error) {
	r := z.region
	h := r.host
	f := frameOf(page)
	owner := f.file.owner
	target := r.loadFile(key)
	h.mu.Lock()
	sum, digested := f.file.digests[f.slot]
	h.mu.Unlock()
	var at fileSlot
	count := 0
	if digested {
		if err := z.host.makeRoom(ctx, target, 1); err != nil {
			z.host.unlockPage(page)
			return nil, err
		}
		at, count = h.allocateFree(target, 1)
	}
	if count == 0 {
		z.host.unindex(ctx, page, key)
		z.host.unlockPage(page)
		return nil, nil
	}
	data := make([]byte, h.pageSize)
	if err := f.file.Read(ctx, f.slot, data); err != nil {
		z.host.unlockPage(page)
		return nil, errors.Join(err, h.abandonSlots(ctx, at, 1, nil))
	}
	if digestOf(data) != sum {
		z.host.unindex(ctx, page, key)
		h.mu.Lock()
		h.stats.Tampered++
		h.mu.Unlock()
		z.host.unlockPage(page)
		err := fmt.Errorf("%w: page %d of a %s memory region", ErrTampered, key.id.Page, owner.kind)
		slog.ErrorContext(ctx, "vmmemory: a VMM wrote a page it holds read-only", "error", err)
		owner.fail(err)
		return nil, h.abandonSlots(ctx, at, 1, nil)
	}
	copied, err := z.host.newFrame(ctx, at, data, f.kind)
	if err != nil {
		z.host.unlockPage(page)
		return nil, err
	}
	root := z.host.root(rootOf(key))
	offset := key.id.Page * h.pageSize
	lock := root.pages.Lock()
	lock.Lock()
	removed := root.pages.RemovePageLocked(offset, page)
	lock.Unlock()
	if !removed || !z.host.supplyIfEmpty(ctx, root.pages, key.id.Page, copied) {
		z.host.releaseFrame(copied)
		z.host.unlockPage(copied)
		z.host.unlockPage(page)
		return nil, fmt.Errorf("vmmemory: a published page left its root while it was moved")
	}
	h.mu.Lock()
	delete(f.file.digests, f.slot)
	z.host.rootPages++
	h.stats.MovedPages++
	h.mu.Unlock()
	if err := z.host.rebind(ctx, page, copied); err != nil {
		z.host.unlockPage(page)
		z.host.unlockPage(copied)
		return nil, err
	}
	z.host.unlockPage(page)
	return copied, nil
}

// unindex stops a published page in a private file being its identity's
// page, as Host.unindexLocked does: it leaves its root, and is its owner's
// own Clean page again where the owner still maps it, and given back where
// nothing maps it. Caller holds page.
func (z *zirconHost) unindex(ctx context.Context, page *zirconvm.VmPage, key pageKey) {
	h := z.host
	root := z.root(rootOf(key))
	offset := key.id.Page * h.pageSize
	lock := root.pages.Lock()
	lock.Lock()
	removed := root.pages.RemovePageLocked(offset, page)
	lock.Unlock()
	if !removed {
		return
	}
	f := frameOf(page)
	owner := f.file.owner.zircon
	h.mu.Lock()
	z.rootPages--
	z.notIdleLocked(f)
	mapped := f.mappedBy(owner)
	h.mu.Unlock()
	if mapped && z.supplyIfEmpty(ctx, owner.pages, key.id.Page, page) {
		h.mu.Lock()
		f.layer = owner
		h.mu.Unlock()
		return
	}
	if err := z.dropSharers(ctx, page); err != nil {
		slog.WarnContext(ctx, "vmmemory: a page that left its root could not be taken from its regions", "error", err)
		return
	}
	h.mu.Lock()
	f.layer = owner
	h.mu.Unlock()
	z.releaseFrame(page)
}

// rebind is Host.rebind over the zircon core: to, which holds the same bytes,
// in place of every mapping of from, and each of from's aliases bound to to.
// A region that can neither be given to nor have its mapping taken away keeps
// the page it has, which is then its alone. Caller holds both pages.
func (z *zirconHost) rebind(ctx context.Context, from, to *zirconvm.VmPage) error {
	h := z.host
	z.moveCold(from, to)
	byRegion := make(map[*zirconRegion][]*zbinding)
	for _, b := range z.aliasesOf(frameOf(from)) {
		byRegion[b.region] = append(byRegion[b.region], b)
	}
	for q, bindings := range byRegion {
		if err := q.remap(ctx, bindings, to); err != nil {
			q.region.heldPages(ctx, err)
			continue
		}
		h.mu.Lock()
		for _, b := range bindings {
			if b.page == from {
				// from is in no object any more: it is never idle.
				b.page = nil
				z.forgetGoingAliasLocked(b, from)
				z.aliasLocked(b, to)
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
		z.releaseFrame(from)
	}
	return nil
}

// remap is MemoryRegion.remap over the zircon core: to, read-only, wherever
// this region maps one of bindings, installed so the guest reads on without
// a fault; where the client refuses the mapping, the mappings are taken
// away instead.
func (z *zirconRegion) remap(ctx context.Context, bindings []*zbinding, to *zirconvm.VmPage) error {
	for _, b := range bindings {
		err := z.mapInPlace(ctx, b, to)
		if errors.Is(err, ErrMappingRefused) {
			return z.revokeBindings(ctx, bindings)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
