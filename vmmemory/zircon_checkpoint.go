package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// Checkpoints over the zircon core: Zircon's writeback with the plan's
// departures (plans/zircon-pager-port-2026-10-05.md, "Dirty tracking, seal,
// checkpoint and flush").
//
//   - The seal is WritebackBegin with D3: the pause write-protects the runs
//     of the dirty set and takes the set whole, and the walk behind it, with
//     the guest running and the region held, makes each resident page of the
//     set AwaitingClean in the region's layer and hands its reservation to
//     the checkpoint's copy of it, a binding beside the layer.
//   - A store into a page the checkpoint holds splits it (D1): the store's copy
//     is the Dirty page, and the checkpoint keeps the page of its pause
//     beside the page list (zirconvm SplitAwaitingClean).
//   - The retire of a published checkpoint is WritebackEnd and a move of each
//     page out of the layer into the identity root of the checkpoint that
//     published it. A page the volume holds no object for is given back.
//   - The abandon of one that did not land is D4: WritebackAbandon makes each
//     page Dirty again with its own reservation, and its read-only mapping
//     goes.
//   - The settle stays the pager's: a sealed page whose bytes are its origin's
//     leaves the checkpoint, and the guest maps the origin again.
//
// Every transition holds the region as the current core's does: the walk and
// each batch of a retire, an abandon or a settle hold it exclusively, so no
// fault of the region decides meanwhile.

// protectDirtyRuns is the whole of the pause: the runs of the dirty set,
// write-protected, with the region's protection held exclusively, as
// MemoryRegion.protectDirtyRuns holds it.
func (r *MemoryRegion) protectDirtyRuns(ctx context.Context) ([]PageRun, error) {
	if err := r.protectMu.Lock(ctx); err != nil {
		return nil, err
	}
	defer r.protectMu.Unlock()
	r.bindingsMu.Lock()
	runs := r.dirtyRuns.runs(uint64(r.pageCount))
	r.bindingsMu.Unlock()
	return r.protect(ctx, runs)
}

// unprotect takes away the mappings of the runs a failed seal protected, so
// the guest's next store to one faults and maps it writable again. Caller
// holds the region exclusively.
func (r *MemoryRegion) unprotect(ctx context.Context, runs []PageRun) error {
	var bindings []*zbinding
	r.bindingsMu.Lock()
	for _, run := range runs {
		for page := run.Page; page < run.Page+uint64(run.Count); page++ {
			bindings = append(bindings, r.bindingLocked(page))
		}
	}
	r.bindingsMu.Unlock()
	return r.revokeBindings(ctx, bindings)
}

// takeDirtySet hands the whole dirty set to a seal in one step and leaves the
// region with none. Caller holds the region exclusively.
func (r *MemoryRegion) takeDirtySet() map[uint64]*zbinding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	pending := r.dirtySet
	r.dirtySet = nil
	r.dirtyRuns = newPageRuns(r.host.pageSize)
	return pending
}

// take is the walk behind the pause, as MemoryRegionCheckpoint.take: it
// makes every page of the sealed set the checkpoint's, with the guest
// running and the region the seal took held, and gives the region back.
func (r *MemoryRegion) take(ctx context.Context, c *MemoryRegionCheckpoint, pending map[uint64]*zbinding) {
	h := r.host
	defer r.mu.Unlock()
	defer close(c.taken)
	if sealWalkSeam != nil {
		sealWalkSeam()
	}
	defer func(start time.Time) { h.sealWalkLatency.Observe(h.clock.Since(start)) }(h.clock.Now())
	// A cold copy the guest did not change is no part of any checkpoint.
	if _, err := r.leaveOutColdCopies(ctx, pending); err != nil {
		slog.ErrorContext(ctx, "vmmemory: a seal could not compare its cold copies", "kind", r.kind, "error", err)
	}
	copies, err := r.takePages(ctx, pending)
	c.mu.Lock()
	c.copies = copies
	c.mu.Unlock()
	if err != nil {
		slog.ErrorContext(ctx, "vmmemory: a seal could not take its pages into the checkpoint",
			"pages", len(copies), "of", len(pending), "kind", r.kind, "error", err)
	}
}

// takePages makes the checkpoint's copy of every page of the sealed set, in
// ascending page order and in bounded batches: the copy aliases the page the
// guest has, which becomes AwaitingClean in the layer (WritebackBegin), and
// takes the reservation it was admitted under and where it was copied from.
// The guest's binding shares the copy until a store copies away from it.
// Caller holds the region exclusively.
func (r *MemoryRegion) takePages(ctx context.Context, pending map[uint64]*zbinding) ([]*zbinding, error) {
	h := r.host
	ps := h.pageSize
	bindings := make([]*zbinding, 0, len(pending))
	for _, b := range pending {
		bindings = append(bindings, b)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].index < bindings[j].index })
	// One slab of copies rather than an allocation each.
	slab := make([]zbinding, len(bindings))
	copies := make([]*zbinding, 0, len(bindings))
	for len(bindings) > 0 {
		count := min(len(bindings), checkpointBatchPages)
		batch := bindings[:count]
		taken := len(copies)
		var resident []bool
		h.mu.Lock()
		for i, b := range batch {
			held := &slab[taken+i]
			*held = zbinding{region: r, index: b.index, dirty: true}
			own := b.page != nil && frameOf(b.page).layer == r
			if b.page != nil {
				r.host.aliasLocked(held, b.page)
			}
			resident = append(resident, own)
		}
		h.mu.Unlock()
		r.bindingsMu.Lock()
		for i, b := range batch {
			r.holdInCheckpointLocked(b, &slab[taken+i])
		}
		r.bindingsMu.Unlock()
		for i, b := range batch {
			if h.measuring() {
				r.sealSums(b.index, &slab[taken+i])
			}
			copies = append(copies, &slab[taken+i])
		}
		// Each run of consecutive pages the layer holds begins its writeback in
		// one call: its Dirty pages become AwaitingClean.
		var failure error
		for i := 0; i < count; {
			if !resident[i] {
				i++
				continue
			}
			run := 1
			for i+run < count && resident[i+run] && batch[i+run].index == batch[i].index+uint64(run) {
				run++
			}
			if err := r.layer.WritebackBegin(batch[i].index*ps, uint64(run)*ps, false); err != nil {
				failure = errors.Join(failure, err)
			}
			i += run
		}
		h.mu.Lock()
		h.stats.CheckpointPages += uint64(count)
		h.mu.Unlock()
		if failure != nil {
			return copies, failure
		}
		bindings = bindings[count:]
	}
	return copies, nil
}

// holdInCheckpointLocked hands b's reservation, where it was copied from and
// whether write-ahead made it to held, the checkpoint's copy, which b shares
// from here: it stays dirty, and a store must copy away from the checkpoint
// before it can change its bytes. Caller holds r.bindingsMu.
func (r *MemoryRegion) holdInCheckpointLocked(b, held *zbinding) {
	r.uncoldLocked(b)
	b.checkpoint, held.spill, b.spill = held, b.spill, noReservation
	held.ahead, b.ahead = b.ahead, false
	held.origin, b.origin = b.origin, nil
	delete(r.dirtySet, b.index)
	r.noteSealableLocked(b)
}

// copiesOf is the checkpoint's copies under the zircon core, in ascending
// page order, once the walk behind the pause has made them.
func (c *MemoryRegionCheckpoint) copiesOf() []*zbinding {
	<-c.taken
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.copies
}

// copyOf is the checkpoint's copy of one page, nil where it holds none.
func (c *MemoryRegionCheckpoint) copyOf(page uint64) *zbinding {
	copies := c.copiesOf()
	at := sort.Search(len(copies), func(i int) bool { return copies[i].index >= page })
	if at < len(copies) && copies[at].index == page {
		return copies[at]
	}
	return nil
}

// readHeld reads the bytes a checkpoint's copy holds: its page's, or its
// reservation's where it was spilled.
func (r *MemoryRegion) readHeld(ctx context.Context, held *zbinding, dst []byte) error {
	h := r.host
	page, err := r.host.lockedPage(ctx, held)
	if err != nil {
		return err
	}
	if page != nil {
		defer r.host.unlockPage(page)
		f := frameOf(page)
		return f.file.Read(ctx, f.slot, dst)
	}
	r.bindingsMu.Lock()
	spill := held.spill
	r.bindingsMu.Unlock()
	return h.readSpill(ctx, spill, dst)
}

// lentRoot is the temporary identity root of what c lends under key, made
// where there is none.
func (h *Host) lentRoot(c *MemoryRegionCheckpoint, key rootKey) *identityRoot {
	h.mu.Lock()
	defer h.mu.Unlock()
	root := h.rootLocked(key)
	root.lent = c
	return root
}

// lend names page, a checkpoint's, in root at index, where the root holds
// nothing there and no point lends it already. The root's page names the
// page's frame, and is never the root's to give back (zlent). Caller holds
// the page's lock.
func (h *Host) lend(ctx context.Context, root *identityRoot, page *zirconvm.VmPage, index uint64) {
	f := frameOf(page)
	h.mu.Lock()
	named := f.lent != nil
	h.mu.Unlock()
	if named {
		return
	}
	lent := zirconvm.NewFramePage(zlent{frame: f})
	if !h.supplyIfEmpty(ctx, root.pages, index, lent) {
		return
	}
	// It is the parent's frame, which the parent's layer queues: the root's
	// page waits where no eviction looks.
	h.node.PageQueues().MoveToWired(lent)
	h.mu.Lock()
	f.lent = lent
	root.lentPages = append(root.lentPages, lent)
	h.mu.Unlock()
}

// supplyIfEmpty supplies one page to an object at index, where the object
// holds nothing there, and reports whether it did. Where it did not, the page
// is still the caller's: a pager's supply would give it back.
func (h *Host) supplyIfEmpty(ctx context.Context, object *zirconvm.CowPages, index uint64,
	page *zirconvm.VmPage) bool {
	ps := h.pageSize
	deferred := zirconvm.NewDeferredOps(object)
	defer deferred.Finish()
	lock := object.Lock()
	lock.Lock()
	defer lock.Unlock()
	if object.PageLocked(index*ps) != nil {
		return false
	}
	list := h.splices.Get().(*zirconvm.PageSpliceList[zirconvm.VmPage])
	list.Initialize(ps)
	if err := list.Insert(0, zirconvm.Page(page)); err != nil {
		panic("vmmemory: building a supply: " + err.Error())
	}
	list.Finalize()
	if err := object.SupplyPagesLocked(zirconvm.CowRange{Offset: index * ps, Len: ps}, list, zirconvm.PagerSupply,
		deferred); err != nil {
		panic("vmmemory: supplying a page to an object that holds none there: " + err.Error())
	}
	list.Reuse()
	h.splices.Put(list)
	return true
}

// compare reports the origin of a checkpoint's copy when the two hold the
// same bytes, and nil where the copy stays in the checkpoint. It changes
// nothing; reshare applies what it decided.
func (r *MemoryRegion) compare(ctx context.Context, s *settler, held *zbinding) (*zirconvm.VmPage, error) {
	h := r.host
	r.bindingsMu.Lock()
	origin, spill := held.origin, held.spill
	r.bindingsMu.Unlock()
	if origin == nil || spill.none() {
		return nil, nil
	}
	if h.wholeRange(r, held.index) {
		// A range the half-private rule filled stays whole.
		return nil, nil
	}
	// The origin first, as every comparison takes them: clean before private.
	if err := r.host.lockPage(ctx, origin); err != nil {
		return nil, err
	}
	defer r.host.unlockPage(origin)
	if !r.host.published(origin) {
		// Evicted, or no longer its identity's page: there is nothing to
		// compare with, and the page is published as it would have been.
		return nil, nil
	}
	page, err := r.host.lockedPage(ctx, held)
	if err != nil || page == nil {
		// Spilled since the seal, which is store I/O the settle does not do.
		return nil, err
	}
	defer r.host.unlockPage(page)
	same, err := s.equal(ctx, h, frameOf(origin).fileSlot, frameOf(page).fileSlot)
	if err != nil || !same {
		return nil, err
	}
	return origin, nil
}

// reshare applies what the comparison decided, in bounded batches with the
// region held exclusively: every page of a batch is revoked by one command
// per run, and only then does each leave the checkpoint. It records in
// dropped which did.
func (r *MemoryRegion) reshare(ctx context.Context, c *MemoryRegionCheckpoint, copies []*zbinding,
	equal []*zirconvm.VmPage, dropped []bool) error {
	pending := make([]int, 0, len(copies))
	for i, origin := range equal {
		if origin != nil {
			pending = append(pending, i)
		}
	}
	for len(pending) > 0 {
		count := min(len(pending), revokeBatchPages)
		batch := pending[:count]
		if err := r.mu.Lock(ctx); err != nil {
			return err
		}
		err := func() error {
			defer r.mu.Unlock()
			if err := r.ready(); err != nil {
				return err
			}
			return r.reshareBatch(ctx, copies, equal, dropped, batch)
		}()
		if err != nil {
			return err
		}
		pending = pending[count:]
	}
	return nil
}

// reshareBatch is one batch of reshare, with the region held exclusively.
// It holds every page it will touch for the whole batch, so the revocation
// that covers them all is issued while none of them can change.
func (r *MemoryRegion) reshareBatch(ctx context.Context, copies []*zbinding, equal []*zirconvm.VmPage,
	dropped []bool, batch []int) error {
	locked := make(map[*zirconvm.VmPage]bool, 2*len(batch))
	defer func() {
		for page := range locked {
			r.host.unlockPage(page)
		}
	}()
	type ready struct {
		index  int
		copied *zirconvm.VmPage
		origin *zirconvm.VmPage
		guest  *zbinding
	}
	var applying []ready
	var guests []*zbinding
	for _, i := range batch {
		origin := equal[i]
		if !locked[origin] {
			if err := r.host.lockPage(ctx, origin); err != nil {
				return err
			}
			locked[origin] = true
		}
		if !r.host.published(origin) {
			continue
		}
		page, err := r.host.lockedPage(ctx, copies[i])
		if err != nil {
			return err
		}
		if page == nil {
			continue
		}
		if locked[page] {
			r.host.unlockPage(page)
		} else {
			locked[page] = true
		}
		entry := ready{index: i, copied: page, origin: origin}
		if b := r.lookupBinding(copies[i].index); b != nil && r.checkpointCopy(b) == copies[i] {
			entry.guest = b
			if r.isMapped(b) {
				guests = append(guests, b)
			}
		}
		applying = append(applying, entry)
	}
	// The guest's mapping of the copy it is losing is taken away rather than
	// swapped underneath it, as the current core's settle does.
	if err := r.revokeBindings(ctx, guests); err != nil {
		return err
	}
	for _, entry := range applying {
		if err := r.dropCopy(ctx, copies[entry.index], entry.copied, entry.origin, entry.guest); err != nil {
			return err
		}
		dropped[entry.index] = true
	}
	return nil
}

// dropCopy takes one unchanged page out of the checkpoint. A guest that
// still shares the checkpoint's copy goes back on the origin and is clean.
// The copy's page leaves the layer, AwaitingClean in its page list or held
// beside it, and goes back, and so does its reservation. Caller holds the
// region exclusively and both pages.
func (r *MemoryRegion) dropCopy(ctx context.Context, held *zbinding, page, origin *zirconvm.VmPage, guest *zbinding) error {
	h := r.host
	ps := h.pageSize
	if guest != nil {
		// The one place the pager hands a guest back an older page on
		// purpose: the audit checks the bytes here rather than trusting the
		// comparison that chose this page.
		if found := h.probe.reshared(ctx, h, frameOf(page), frameOf(origin)); found != "" {
			panic(found)
		}
	}
	h.mu.Lock()
	if guest != nil {
		r.host.unaliasLocked(guest)
		r.host.aliasLocked(guest, origin)
	}
	r.host.unaliasLocked(held)
	others := frameOf(page).aliases.len() > 0
	h.mu.Unlock()
	if guest != nil {
		r.bindingsMu.Lock()
		r.retireFromCheckpointLocked(guest)
		r.bindingsMu.Unlock()
		r.host.node.PageQueues().MarkAccessed(origin)
	}
	if others {
		return fmt.Errorf("vmmemory: a settled page is still mapped at page %d", held.index)
	}
	if r.layer.RemovePage(held.index*ps, page) {
		r.host.releaseFrame(page)
	}
	r.bindingsMu.Lock()
	spill := held.spill
	held.spill, held.dirty, held.origin = noReservation, false, nil
	r.bindingsMu.Unlock()
	h.releaseSpill(spill)
	return nil
}

// retireFromCheckpointLocked ends b's dirty epoch: its bytes are the volume's
// now, or its origin's. Caller holds r.bindingsMu.
func (r *MemoryRegion) retireFromCheckpointLocked(b *zbinding) {
	r.host.probe.retired(b)
	r.uncoldLocked(b)
	b.checkpoint, b.dirty, b.origin = nil, false, nil
	delete(r.dirtySet, b.index)
	r.noteSealableLocked(b)
}

// restoreFromCheckpointLocked hands an abandoned checkpoint's copy back to
// the page it was taken from: its reservation, where it was copied from and
// whether write-ahead made it, and the page is dirty again, exactly as it was
// before the seal. Caller holds r.bindingsMu.
func (r *MemoryRegion) restoreFromCheckpointLocked(b, held *zbinding) {
	r.uncoldLocked(b)
	b.checkpoint, b.spill, b.dirty, b.ahead = nil, held.spill, true, held.ahead
	b.origin, held.origin = held.origin, nil
	held.spill, held.dirty, held.ahead = noReservation, false, false
	if r.dirtySet == nil {
		r.dirtySet = make(map[uint64]*zbinding)
	}
	r.dirtySet[b.index] = b
	r.noteSealableLocked(b)
}

// forgetCopies rebuilds the checkpoint's copies without those a settle
// dropped, as forget does for the current core's.
func (c *MemoryRegionCheckpoint) forgetCopies(dropped []bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copies := make([]*zbinding, 0, len(c.copies))
	for i, held := range c.copies {
		if i < len(dropped) && dropped[i] {
			continue
		}
		copies = append(copies, held)
	}
	c.copies = copies
	if len(c.copies) == 0 {
		c.dirtySince = time.Time{}
	}
}

// storedIdentities is MemoryRegion.storedIdentities over the zircon core:
// the identity the volume now gives each page of one retire batch, located
// once per read-ahead window, with neither the region nor any page held.
func (r *MemoryRegion) storedIdentities(ctx context.Context, batch []*zbinding) (map[uint64]storedPage, error) {
	result := make(map[uint64]storedPage, len(batch))
	var window *zplan
	for _, held := range batch {
		if r.spillOf(held).none() {
			continue
		}
		index := held.index
		if window == nil || index < window.start || index >= window.end {
			start, end := r.window(index)
			var err error
			if window, err = r.plan(ctx, start, end, end); err != nil {
				return nil, err
			}
		}
		id, stored := window.identity(index)
		result[index] = storedPage{id, stored}
	}
	return result, nil
}

// spillOf is the reservation b holds.
func (r *MemoryRegion) spillOf(b *zbinding) reservation {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.spill
}

// finalizeCheckpoint retires every page of one batch of a published
// checkpoint, as MemoryRegion.finalizeCheckpoint does. A page the guest still
// shares with the checkpoint is the volume's now: it becomes Clean, leaves
// the layer for the identity root of its identity, and stays mapped. A page
// the guest copied away from keeps only the checkpoint's copy, which goes.
// Either way the reservation goes back. Caller holds the region exclusively.
func (r *MemoryRegion) finalizeCheckpoint(ctx context.Context, c *MemoryRegionCheckpoint, batch []*zbinding,
	identities map[uint64]storedPage) error {
	h := r.host
	if err := r.revokeHandedBack(ctx, batch, identities); err != nil {
		return err
	}
	for _, held := range batch {
		spill := r.spillOf(held)
		if spill.none() {
			continue
		}
		b := r.lookupBinding(held.index)
		shared := b != nil && r.checkpointCopy(b) == held
		now := identities[held.index]
		page, err := r.host.lockedPage(ctx, held)
		if err != nil {
			return err
		}
		if shared {
			r.bindingsMu.Lock()
			r.retireFromCheckpointLocked(b)
			r.bindingsMu.Unlock()
		}
		if page != nil {
			if shared {
				err = r.publish(ctx, c, b, held, page, now)
			} else {
				err = r.retireCopy(ctx, held, page, now)
			}
			r.host.unlockPage(page)
			if err != nil {
				return err
			}
		}
		h.releaseSpill(spill)
		r.bindingsMu.Lock()
		held.spill, held.dirty = noReservation, false
		r.bindingsMu.Unlock()
	}
	return nil
}

// revokeHandedBack is MemoryRegion.revokeHandedBack over the zircon core: the
// guest's mapping of every page of one retire batch the volume holds no
// object for goes, one command per run, before the walk, once the page is
// known to be zeros.
func (r *MemoryRegion) revokeHandedBack(ctx context.Context, batch []*zbinding, identities map[uint64]storedPage) error {
	var guests []*zbinding
	for _, held := range batch {
		if r.spillOf(held).none() {
			continue
		}
		now := identities[held.index]
		if now.stored && !now.id.zero() {
			continue
		}
		b := r.lookupBinding(held.index)
		if b == nil || r.checkpointCopy(b) != held {
			continue
		}
		if err := r.droppable(ctx, held, now); err != nil {
			return err
		}
		if r.isMapped(b) {
			guests = append(guests, b)
		}
	}
	return r.revokeBindings(ctx, guests)
}

// droppable is Host.droppable over the zircon core: a page given up because
// the volume holds no object for it must be zeros.
func (r *MemoryRegion) droppable(ctx context.Context, held *zbinding, now storedPage) error {
	h := r.host
	if now.stored && !now.id.zero() {
		return nil
	}
	page, err := r.host.lockedPage(ctx, held)
	if err != nil || page == nil {
		return err
	}
	defer r.host.unlockPage(page)
	f := frameOf(page)
	data := make([]byte, h.pageSize)
	if err := f.file.Read(ctx, f.slot, data); err != nil {
		return err
	}
	if allZero(data) {
		return nil
	}
	return fmt.Errorf("%w: page %d of %s", ErrUndroppable, held.index, r.kind)
}

// publish retires a page the guest still shares with the checkpoint: its
// writeback ends, so it is Clean, and it leaves the layer for the identity
// root of the identity the volume now gives it, where the guest goes on
// mapping it. A page the volume holds no object for, or whose identity
// another page holds already, is given back. A page of the region's private
// file the publication read no digest of stays the region's own Clean page:
// another region could not check a copy of it. Caller holds the region
// exclusively and the page's lock.
func (r *MemoryRegion) publish(ctx context.Context, c *MemoryRegionCheckpoint, b, held *zbinding,
	page *zirconvm.VmPage, now storedPage) error {
	h := r.host
	ps := h.pageSize
	h.mu.Lock()
	r.host.unaliasLocked(held)
	h.mu.Unlock()
	if err := r.layer.WritebackEnd(b.index*ps, ps); err != nil {
		return err
	}
	f := frameOf(page)
	if now.stored && !now.id.zero() {
		sum := c.digestOf(b.index)
		if f.file.owner != nil && sum == nil {
			return nil
		}
		if r.host.adopt(ctx, r, page, b.index, now.id) {
			if f.file.owner != nil {
				h.mu.Lock()
				f.file.digests[f.slot] = *sum
				h.mu.Unlock()
			}
			return nil
		}
	}
	// The one hand-back that arrives alone: a page whose identity another
	// page holds. One the volume holds no object for was revoked with its
	// batch (revokeHandedBack), and this skips it.
	if err := r.revoke(ctx, b); err != nil {
		return err
	}
	h.mu.Lock()
	r.host.unaliasLocked(b)
	h.mu.Unlock()
	if err := r.host.dropSharers(ctx, page); err != nil {
		return err
	}
	r.layer.RemovePage(b.index*ps, page)
	r.host.releaseFrame(page)
	return nil
}

// retireCopy ends the checkpoint's own copy of a page the guest has stored
// into since the seal. It goes back with the checkpoint, unless children of
// a fork point on this host still map it: it holds what the checkpoint
// published, so it becomes the page of its identity where the volume gives
// it one and no other page holds it, and those children keep it. Caller
// holds the region exclusively and the page's lock.
func (r *MemoryRegion) retireCopy(ctx context.Context, held *zbinding, page *zirconvm.VmPage, now storedPage) error {
	h := r.host
	ps := h.pageSize
	h.mu.Lock()
	r.host.unaliasLocked(held)
	mapped := frameOf(page).aliases.len() > 0
	h.mu.Unlock()
	if !mapped {
		// The end of the writeback frees what the checkpoint held beside the
		// page list (D1).
		return r.layer.WritebackEnd(held.index*ps, ps)
	}
	if !r.layer.RemovePage(held.index*ps, page) {
		return fmt.Errorf("vmmemory: the checkpoint's copy of page %d is not in its region's layer", held.index)
	}
	if now.stored && !now.id.zero() && r.host.adopt(ctx, nil, page, held.index, now.id) {
		return nil
	}
	if err := r.host.dropSharers(ctx, page); err != nil {
		return err
	}
	r.host.releaseFrame(page)
	return nil
}

// adopt moves page into the identity root of id at the page id names, where
// that root holds nothing there, and reports whether it did. from is the
// layer that held it at index, out of which it is taken first, nil where it
// is out of every object already; a page adopt took out and could not move
// is out of every object, and the caller's to give back. The page is a
// root's from here: Clean, named by its identity, idle once nothing maps it.
// Caller holds the page's lock.
func (h *Host) adopt(ctx context.Context, from *MemoryRegion, page *zirconvm.VmPage, index uint64, id pageKey) bool {
	ps := h.pageSize
	root := h.root(rootOf(id))
	lock := root.pages.Lock()
	lock.Lock()
	existing := root.pages.PageLocked(id.id.Page * ps)
	if existing != nil && h.lentHere(root, existing) {
		// A fork point published the name it lent: its parent's own page
		// takes the name back, and the root is a published checkpoint's from
		// here, whose lent pages go when the seal ends.
		root.pages.RemovePageLocked(id.id.Page*ps, existing)
		h.mu.Lock()
		if f := frameOf(existing); isLent(existing) && f.lent == existing {
			f.lent = nil
		}
		root.published = true
		h.mu.Unlock()
		existing = nil
	}
	lock.Unlock()
	if existing != nil {
		return false
	}
	if from != nil && !from.layer.RemovePage(index*ps, page) {
		return false
	}
	if !h.supplyIfEmpty(ctx, root.pages, id.id.Page, page) {
		return false
	}
	h.mu.Lock()
	frameOf(page).layer = nil
	h.rootPages++
	h.idleLocked(page)
	h.mu.Unlock()
	return true
}

// abandonCopies is MemoryRegion.abandonPages over the zircon core (D4): a
// page the guest still shares with the checkpoint takes its copy's
// reservation back and is Dirty again in the layer, and its read-only
// mapping goes so the next store maps it writable; a page the guest copied
// away from needs only its own newer state, so the checkpoint's copy goes.
// Caller holds the region exclusively.
func (r *MemoryRegion) abandonCopies(ctx context.Context, batch []*zbinding) error {
	h := r.host
	ps := h.pageSize
	var restored []*zbinding
	for _, held := range batch {
		spill := r.spillOf(held)
		if spill.none() {
			continue
		}
		b := r.lookupBinding(held.index)
		shared := b != nil && r.checkpointCopy(b) == held
		page, err := r.host.lockedPage(ctx, held)
		if err != nil {
			return err
		}
		if page != nil {
			// The guest takes the page back as dirty state it may store into in
			// place, so nothing else may still be reading it.
			if err := r.host.dropSharers(ctx, page, b, held); err != nil {
				r.host.unlockPage(page)
				return err
			}
			h.mu.Lock()
			r.host.unaliasLocked(held)
			h.mu.Unlock()
		}
		if shared {
			r.bindingsMu.Lock()
			r.restoreFromCheckpointLocked(b, held)
			r.bindingsMu.Unlock()
			// An abandoned checkpoint gives the page straight back: the guest
			// may store into it again, and into these very bytes.
			h.probe.granted(b, probeFrame(page), nil)
			if h.measuring() {
				r.unsealSums(b.index, held)
			}
			if page != nil {
				// AwaitingClean is Dirty again, with the reservation it holds
				// (D4).
				err = r.layer.WritebackAbandon(ctx, held.index*ps, ps)
			}
			if r.isMapped(b) {
				restored = append(restored, b)
			}
		} else {
			r.bindingsMu.Lock()
			held.spill, held.dirty = noReservation, false
			r.bindingsMu.Unlock()
			if page != nil {
				// The abandon frees what the checkpoint held beside the page
				// list.
				err = r.layer.WritebackAbandon(ctx, held.index*ps, ps)
			}
			h.releaseSpill(spill)
		}
		if page != nil {
			r.host.unlockPage(page)
		}
		if err != nil {
			return err
		}
	}
	return r.revokeBindings(ctx, restored)
}

// discardCheckpoint drops a checkpoint entirely, which detaching a region
// does: its pages go with the region's layer, and their reservations and the
// pages they were copied from go now.
func (r *MemoryRegion) discardCheckpoint(ctx context.Context, c *MemoryRegionCheckpoint) error {
	h := r.host
	for _, held := range c.copiesOf() {
		page, err := r.host.lockedPage(ctx, held)
		if err != nil {
			return err
		}
		if page != nil {
			err = r.host.dropSharers(ctx, page)
			h.mu.Lock()
			if held.page != nil {
				r.host.unaliasLocked(held)
			}
			h.mu.Unlock()
			r.host.unlockPage(page)
			if err != nil {
				return err
			}
		}
		r.bindingsMu.Lock()
		origin, spill := held.origin, held.spill
		held.origin, held.spill, held.dirty = nil, noReservation, false
		r.bindingsMu.Unlock()
		if origin != nil {
			r.host.dropOrigin(origin)
		}
		if !spill.none() {
			h.releaseSpill(spill)
		}
	}
	if err := r.endFork(ctx, c); err != nil {
		return err
	}
	c.finish(ErrClosed)
	return nil
}

// endFork takes back what a checkpoint lent when its seal ends: its names go,
// with every temporary identity root it lent its pages under. A child that
// maps one of its pages keeps mapping it: the page is its identity's now, or
// the region's own again, and a child that faults on it next reads it through
// its own backing.
func (r *MemoryRegion) endFork(ctx context.Context, c *MemoryRegionCheckpoint) error {
	h := r.host
	h.mu.Lock()
	for key, lender := range h.lent {
		if lender == c {
			delete(h.lent, key)
		}
	}
	var roots []*identityRoot
	for key, root := range r.host.roots {
		if root.lent != c {
			continue
		}
		roots = append(roots, root)
		root.lent = nil
		if !root.published {
			delete(r.host.roots, key)
		}
	}
	h.mu.Unlock()
	for _, root := range roots {
		if err := r.host.dropLentRoot(ctx, root); err != nil {
			return err
		}
	}
	return r.endForkFile(ctx, c)
}

// lentHere reports a page of a lent root that is not a published one: a page
// naming its parent's frame, or a copy in the point's file. Caller holds the
// root's lock.
func (h *Host) lentHere(root *identityRoot, page *zirconvm.VmPage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if root.lent == nil {
		return false
	}
	return isLent(page) || slices.Contains(root.copies, page)
}

// dropLentRoot takes what a temporary identity root lent away: each of its
// pages naming a parent's frame stops naming it, and each copy in the point's
// file goes, with every mapping of it. A root the point published keeps the
// pages it published; any other goes.
func (h *Host) dropLentRoot(ctx context.Context, root *identityRoot) error {
	// A copy in the point's file goes with the root: every child that maps
	// it reads it through its own backing from here.
	h.mu.Lock()
	copies := root.copies
	root.copies = nil
	h.mu.Unlock()
	for _, page := range copies {
		if err := h.lockPage(ctx, page); err != nil {
			return err
		}
		err := h.dropSharers(ctx, page)
		if err == nil && frameOf(page).slot >= 0 {
			h.removeFromObject(page)
			h.releaseFrame(page)
		}
		h.unlockPage(page)
		if err != nil {
			return err
		}
	}
	h.mu.Lock()
	lent := root.lentPages
	root.lentPages = nil
	for _, page := range lent {
		if f := frameOf(page); f.lent == page {
			f.lent = nil
		}
	}
	published := root.published
	h.mu.Unlock()
	if !published {
		root.object.Destroy()
		return context.Cause(ctx)
	}
	for _, page := range lent {
		if link, ok := h.node.PageQueues().Backlink(page); ok {
			lock := link.Cow.Lock()
			lock.Lock()
			link.Cow.RemovePageLocked(link.Offset, page)
			lock.Unlock()
		}
	}
	return context.Cause(ctx)
}

// dropSharers takes page away from every binding that maps it but keep: a
// page the guest takes back as dirty state, or that goes back to the arena,
// must be no other region's. Caller holds the page's lock.
func (h *Host) dropSharers(ctx context.Context, page *zirconvm.VmPage, keep ...*zbinding) error {
	var sharers []*zbinding
	h.mu.Lock()
	for b := range frameOf(page).aliases.all() {
		if !slices.Contains(keep, b) && b.page != nil {
			sharers = append(sharers, b)
		}
	}
	h.mu.Unlock()
	for _, b := range sharers {
		if err := b.region.revoke(ctx, b); err != nil {
			return err
		}
		h.mu.Lock()
		if b.page != nil {
			h.unaliasLocked(b)
		}
		h.mu.Unlock()
	}
	return nil
}

// published reports a page of an identity root, still resident: what a copy
// may remember as its origin, and what a settle or a give-back may put the
// guest back on.
func (h *Host) published(page *zirconvm.VmPage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := frameOf(page)
	return f.layer == nil && f.slot >= 0 && !isLent(page)
}

// endForkFile is the part of endFork an isolated arena's fork file needs:
// each child closes the file, and the file goes back to the arena once its
// copies, which went with the point's temporary root, are gone.
func (r *MemoryRegion) endForkFile(ctx context.Context, c *MemoryRegionCheckpoint) error {
	h := r.host
	c.mu.Lock()
	f := c.fork
	c.fork = nil
	c.mu.Unlock()
	if f == nil {
		return nil
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
		// region, and the file goes back once that region is closed.
		f.orphaned = true
	}
	return nil
}
