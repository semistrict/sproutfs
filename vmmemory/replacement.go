package vmmemory

import (
	"context"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// replacement is the pages a store's copy replaces the guest's mapping of,
// which no eviction may take and no idle drop may give up before the command
// that replaces them lands, and the pages the store made, which it holds the
// locks of until then too.
type replacement struct {
	region *MemoryRegion
	pages  []*zirconvm.VmPage
	guests []*binding
	made   []*zirconvm.VmPage
}

// holdLocked keeps page, which b mapped before the store copied it, where it
// is until the store's command lands. Caller holds h.mu.
func (p *replacement) holdLocked(b *binding, page *zirconvm.VmPage) {
	frameOf(page).replacing++
	p.pages = append(p.pages, page)
	p.guests = append(p.guests, b)
}

// keep holds a page locked until the store's command lands: one the store
// made, and one the guest maps that the command replaces.
func (p *replacement) keep(page *zirconvm.VmPage) { p.made = append(p.made, page) }

// done gives up every page held, now that the guest maps none of them: a
// root's page nothing maps is idle from here, as any other is.
func (p *replacement) done() {
	h := p.region.host
	var dropped []*zirconvm.VmPage
	h.mu.Lock()
	for _, page := range p.pages {
		f := frameOf(page)
		f.replacing--
		if f.replacing == 0 && f.dropped && f.aliases.len() == 0 {
			f.dropped = false
			dropped = append(dropped, page)
			continue
		}
		p.region.host.idleLocked(page)
	}
	h.signal()
	h.mu.Unlock()
	// A page that left its object while it was replaced goes back now that
	// nothing reads it. It is given back with h.mu not held, but the mark
	// was cleared under it, so no other caller gives it back, and a page in
	// no object is one nothing finds to bind meanwhile.
	for _, page := range dropped {
		p.region.host.releaseFrame(page)
	}
	p.pages, p.guests = nil, nil
}

// unlock gives back the locks of the pages the store made, once the guest's
// access to them has completed.
func (p *replacement) unlock() {
	for _, page := range p.made {
		frameOf(page).mu.Unlock()
	}
	p.made = nil
	h := p.region.host
	h.mu.Lock()
	h.signal()
	h.mu.Unlock()
}

// revoke takes the guest's mappings of the held pages away, which a store
// whose command did not land does, and gives the pages up.
func (p *replacement) revoke(ctx context.Context) error {
	r := p.region
	for _, b := range p.guests {
		if err := r.revokePage(ctx, b.index); err != nil {
			p.done()
			return r.fail(err)
		}
	}
	p.done()
	return nil
}
