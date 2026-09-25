package vmmemory

import "context"

// A store replaces a mapping; it does not take one away.
//
// The guest maps the page a copy-on-write copies from, and something has to
// make that mapping stop naming that page's memory before the memory can go.
// A revocation does it, and so does the MAP that installs the copy — the
// protocol's MAP over a range replaces whatever the pages of it had — and only
// the second is work the store had to do anyway. A revocation is for taking a
// page away with nothing to put in its place: a reclaim taking a victim, a
// settle handing a page back to its origin, an abandoned checkpoint. Never a
// store, which always has the copy to put there.
//
// What that costs is an ordering. Between the moment a store takes its binding
// off the page it copied from and the moment its mapping command lands, the
// guest goes on reading that page's offset while nothing names it any more, so
// the page stays exactly where it is: no reclaim may take it, and where its last
// binding was this store's its memory goes back after the command rather than at
// the unlink. replacement is that interval — the pages one store is replacing
// the mappings of, held from the copy until the command.
//
// It holds no page's lock, because a store copies the pages of its run in page
// order after the page it faulted on: two stores of overlapping runs in two
// memory regions would take the same origins in two orders and wait on each other.
type replacement struct {
	memoryRegion *MemoryRegion
	// pages are the pages being replaced and guests the bindings whose mappings
	// of them the command replaces, in the order they were taken.
	pages  []*resident
	guests []*binding
}

// hold keeps one page the guest still maps where it is until this store's
// mapping command lands. Caller holds pg's lock and the binding is mapped.
func (p *replacement) hold(b *binding, pg *resident) {
	h := p.memoryRegion.host
	h.mu.Lock()
	pg.replacing++
	h.mu.Unlock()
	p.pages = append(p.pages, pg)
	p.guests = append(p.guests, b)
}

// done gives up every page this store was replacing, now that the guest maps
// none of them any more: each goes back to being an ordinary resident page, and
// one whose last binding went while it was held has its memory released here
// rather than at the unlink that took that binding away.
func (p *replacement) done(ctx context.Context) error {
	h := p.memoryRegion.host
	pages := p.pages
	p.pages, p.guests = nil, nil
	// Nothing about giving a page back is the caller's context to abandon: a
	// store whose guest is gone still owes the arena its offsets.
	ctx = context.WithoutCancel(ctx)
	var failure error
	for _, pg := range pages {
		h.mu.Lock()
		// The page stays held across its own release, so no reclaim can take it
		// between the decision and the lock.
		drop := pg.replacing == 1 && pg.dropped && pg.slot >= 0
		if !drop {
			pg.replacing--
		}
		h.mu.Unlock()
		if !drop {
			continue
		}
		// It is reachable from no binding and by no identity, so nothing else
		// would ever release it and its lock is nobody's to be holding.
		if err := pg.mu.Lock(ctx); err != nil {
			failure = err
			continue
		}
		err := h.release(ctx, pg)
		h.mu.Lock()
		pg.replacing--
		pg.dropped = false
		h.mu.Unlock()
		h.unlock(pg)
		if err != nil {
			failure = err
		}
	}
	return failure
}

// revoke is what a store that could not map its run does. The pages it copied
// from are still what the guest maps and their memory is about to go back, and
// with no command to put the copies in their place a revocation — one per run
// of consecutive pages — is the only thing that stops the guest reading them.
// It is the one revocation a store ever issues, on the one path where the
// client refused its mapping or the command failed.
//
// A revocation that fails is a terminal memory region, and then the pages stay held:
// nothing reclaims them and nothing releases them, which is what a terminal
// memory region's pages are until it is detached. MemoryRegion.heldPages is what says so.
func (p *replacement) revoke(ctx context.Context) error {
	guests := p.guests
	if err := p.memoryRegion.revokeBindings(ctx, guests); err != nil {
		p.pages, p.guests = nil, nil
		return err
	}
	return p.done(ctx)
}
