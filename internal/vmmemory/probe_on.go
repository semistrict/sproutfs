//go:build sproutfsprobe

// Package build tag `sproutfsprobe` turns on the pager's own audit of what it
// hands a guest. It exists for one open defect: a fan-out of two children at a
// 4 KiB RAM page panics a child's guest kernel on a data structure the guest
// itself wrote, which is a guest reading bytes that are not its page's. See
// docs/open-work.md.
//
// Three things to know before using it.
//
//   - What it checks. A resident page holding a published page identity is
//     immutable while it holds that name, so its bytes must never change: if
//     they do, something wrote into memory a guest only reads. And a private
//     page is one region's own, so two regions reaching one is one guest
//     writing into another's memory. Neither check has ever fired, across
//     sixteen runs that produced eight guest panics between them — so the
//     corruption is not a shared page being overwritten.
//
//   - What the lost-write check adds. The guest kernel dies on a poisoned list
//     node, which is not random bytes: it is an OLDER version of a page the
//     guest itself had already written. So the audit gives every page a
//     generation, raises it whenever the pager copies a page or grants a guest
//     the right to store into one, and refuses to install into that guest any
//     page whose generation is older than the newest it was last given
//     writable. It also refuses a page that arrives in a guest's own memory
//     from outside that guest's store path at all, because every path that
//     gives a guest memory of its own dates it in the same breath. The one
//     place the pager legitimately hands a guest back an
//     older page is the settle's re-share of an unchanged page onto its origin,
//     and that is not exempted: the two pages' bytes are compared there, and
//     the origin only inherits the copy's generation once they are equal. A
//     lost write therefore fails inside the pager, with a stack, at the moment
//     it is handed over rather than milliseconds later in the guest.
//
//   - That it is an accelerator, not the cause. Reading and summing a page at
//     every release of its lock slows the pager down, and that widens whatever
//     window the defect lives in: the same fixture panics about one run in five
//     without it and about one in two with it. It is the only way found so far
//     to make the defect frequent enough to reduce against, which is what it is
//     for. Nothing it does is a fix and nothing it reports is a cause.
//
// Build with `-tags sproutfsprobe`; scripts/fanout-reduce-lima.sh does.
package vmmemory

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"sync"
	"time"
)

// probeState is the audit's own memory: what each published page held when it
// was last looked at, and how old the bytes of each page are. It is keyed by
// the page rather than kept on it so that an ordinary build carries none of it,
// and it takes its own lock rather than the host's so that a check may run
// wherever the pager hands something over.
type probeState struct {
	mu   sync.Mutex
	sums map[*resident]uint32
	// generation is the age of the bytes a page holds: higher is newer. A page
	// the pager has never copied from and never made writable has none, and is
	// not compared — nothing is known about how old it is.
	generation map[*resident]uint64
	// writable is the newest generation each binding was last given the right
	// to store into. Installing anything older into it is a lost write.
	writable map[*binding]uint64
	// pending is a page installed into memory a guest still owns that the pager
	// never went on to date as newer. Every path that gives a guest a page of
	// its own — a copy, a refault, an abandoned checkpoint — dates it in the
	// same breath, so an install left sitting here is one that came from
	// somewhere else: a post-copy or a read-ahead putting a page over one the
	// guest had already stored into. It is reported at that binding's next
	// transition, because there is nothing at the install itself to tell the
	// two apart.
	pending map[*binding]*resident
	counter uint64
}

// Every check returns what it found rather than panicking where it found it.
// A check runs at the one moment the pager is holding the host lock or a page's
// own lock, and a panic thrown there unwinds through deferred releases of locks
// it does not hold in the right order: the process stops dead with nothing
// printed, which is worse than the defect. The caller releases what it holds
// and then fails, with the finding and a stack still at the transition.

// stable checks that a published page's bytes have not changed since the last
// time anything held its lock. Caller holds that lock.
func (p *probeState) stable(ctx context.Context, h *Host, pg *resident, where string) string {
	if pg == nil || !pg.published() || pg.slot < 0 {
		return ""
	}
	buf := make([]byte, h.pageSize)
	if err := h.arena.Read(ctx, pg.slot, buf); err != nil {
		return ""
	}
	sum := crc32.ChecksumIEEE(buf)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sums == nil {
		p.sums = make(map[*resident]uint32)
	}
	previous, seen := p.sums[pg]
	if !seen {
		p.sums[pg] = sum
		return ""
	}
	if previous != sum {
		return fmt.Sprintf("probe %s: published page %+v in slot %d changed its bytes",
			where, pg.key.id, pg.slot)
	}
	return ""
}

// bind checks that a private page nothing has named is reached from one region
// only, and that the page being installed is not older than what this guest was
// last given writable. A private page a seal has named is not checked for the
// first of those: naming one is exactly how a fork on this host lets the
// machines that inherit the identity map the page instead of reading it, so
// more than one region reaching it is the sharing working. Caller holds the
// host lock.
func (p *probeState) bind(h *Host, b *binding, pg *resident) string {
	if pg.private && pg.key == (pageKey{}) {
		for other := range pg.aliases {
			if other.region != b.region {
				return fmt.Sprintf("probe bind: unnamed private slot %d is reached from two regions, pages %d and %d",
					pg.slot, other.index, b.index)
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	given, owed := p.writable[b]
	if !owed {
		return ""
	}
	if held, dated := p.generation[pg]; dated {
		if held < given {
			return fmt.Sprintf("probe bind: page %d of region %p is being given generation %d in slot %d, "+
				"older than generation %d it was last given writable — a lost write",
				b.index, b.region, held, pg.slot, given)
		}
		return ""
	}
	if stale, waiting := p.pending[b]; waiting && stale != pg {
		return fmt.Sprintf("probe bind: page %d of region %p was given slot %d from outside its own "+
			"store path while it owned generation %d, and now takes slot %d — a lost write",
			b.index, b.region, stale.slot, given, pg.slot)
	}
	if p.pending == nil {
		p.pending = make(map[*binding]*resident)
	}
	p.pending[b] = pg
	return ""
}

// granted records that the pager has just made pg this binding's own memory to
// store into. origin, where there is one, is the page pg was copied from, which
// is dated here too so that re-sharing it later is recognisably a step back.
func (p *probeState) granted(b *binding, pg, origin *resident) {
	if pg == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.generation == nil {
		p.generation = make(map[*resident]uint64)
		p.writable = make(map[*binding]uint64)
	}
	if origin != nil {
		if _, dated := p.generation[origin]; !dated {
			p.counter++
			p.generation[origin] = p.counter
		}
	}
	p.counter++
	p.generation[pg] = p.counter
	p.writable[b] = p.counter
	delete(p.pending, b)
}

// retired records that a binding's private epoch has ended: the volume holds
// its bytes, so the pager owes that guest nothing newer and any page it is
// given from here is chosen by identity rather than by age.
func (p *probeState) retired(b *binding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.writable, b)
	delete(p.pending, b)
}

// reshared checks the settle's one legitimate step back. An unchanged page is
// dropped by putting the guest back on the page it was copied from, which is an
// older generation by construction; it is sound only because the two hold the
// same bytes, so that is what is checked, here, with both pages held and the
// guest's mapping already revoked. Once they are equal the origin inherits the
// copy's generation, so the guest is not being handed anything older and any
// other path that would is still caught.
func (p *probeState) reshared(ctx context.Context, h *Host, copied, origin *resident) string {
	if copied == nil || origin == nil || copied.slot < 0 || origin.slot < 0 {
		return ""
	}
	was := make([]byte, h.pageSize)
	now := make([]byte, h.pageSize)
	if err := h.arena.Read(ctx, copied.slot, was); err != nil {
		return ""
	}
	if err := h.arena.Read(ctx, origin.slot, now); err != nil {
		return ""
	}
	if !bytes.Equal(was, now) {
		at := 0
		for at < len(was) && was[at] == now[at] {
			at++
		}
		return fmt.Sprintf("probe reshared: the settle is re-sharing slot %d onto origin %+v in slot %d, "+
			"but they differ from byte %d (%#x against %#x) — a lost write",
			copied.slot, origin.key.id, origin.slot, at, was[at], now[at])
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.generation != nil && p.generation[copied] > p.generation[origin] {
		p.generation[origin] = p.generation[copied]
	}
	return ""
}

// The ring is what the pager did to one page, in order. A guest that dies on a
// page it wrote leaves a kernel address in its oops, which is a page number of
// its RAM region; this answers what happened to that page and the pages beside
// it, which is the whole question a reduction ends at.
//
// It is bounded and lossy on purpose: the interesting window is the last
// fraction of a second of a child's life, and keeping every event of a
// half-million-fault run would cost more than the pager.
const ringEvents = 1 << 16

// ringEvent is one thing the pager did, in the order it did it.
type ringEvent struct {
	at     time.Time
	region *Region
	page   uint64
	what   string
	slot   int
	// other is a second slot the event is about: the origin a page is dropped
	// onto, or the page a copy was made from.
	other int
}

type probeRing struct {
	mu     sync.Mutex
	events [ringEvents]ringEvent
	next   uint64
}

var ring probeRing

// note records one event. It takes no page lock and no host lock, so it may be
// called from anywhere the pager is already holding something.
func note(r *Region, page uint64, what string, slot, other int) {
	ring.mu.Lock()
	ring.events[ring.next%ringEvents] = ringEvent{
		at: time.Now(), region: r, page: page, what: what, slot: slot, other: other}
	ring.next++
	ring.mu.Unlock()
}

// Ring reports, oldest first, everything the pager did to the pages within
// radius of page in this region, and everything it did to every arena slot
// those pages ever occupied — a slot handed to another page is how a guest
// reaches bytes that are not its own, so the slot's own history is part of the
// page's. It is for a test that has just watched a guest die.
func Ring(r *Region, page uint64, radius uint64) []string {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	first := uint64(0)
	if ring.next > ringEvents {
		first = ring.next - ringEvents
	}
	slots := make(map[int]bool)
	for i := first; i < ring.next; i++ {
		e := ring.events[i%ringEvents]
		if e.region != r || e.page+radius < page || e.page > page+radius {
			continue
		}
		if e.slot >= 0 {
			slots[e.slot] = true
		}
		if e.other >= 0 {
			slots[e.other] = true
		}
	}
	var lines []string
	for i := first; i < ring.next; i++ {
		e := ring.events[i%ringEvents]
		if e.region != r {
			continue
		}
		near := e.page+radius >= page && e.page <= page+radius
		if !near && !slots[e.slot] && !slots[e.other] {
			continue
		}
		line := fmt.Sprintf("%s page %d %s slot %d", e.at.Format("15:04:05.000000"), e.page, e.what, e.slot)
		if e.other >= 0 {
			line += fmt.Sprintf(" other %d", e.other)
		}
		if !near {
			line += " (a page that shared one of these slots)"
		}
		lines = append(lines, line)
	}
	return lines
}
