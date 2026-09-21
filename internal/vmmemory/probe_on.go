//go:build sproutfsprobe

// Package build tag `sproutfsprobe` turns on the pager's own audit of what it
// hands a guest. It exists for one open defect: a fan-out of two children at a
// 4 KiB RAM page panics a child's guest kernel on a data structure the guest
// itself wrote, which is a guest reading bytes that are not its page's. See
// docs/open-work.md.
//
// Two things to know before using it.
//
//   - What it checks. A resident page holding a published page identity is
//     immutable while it holds that name, so its bytes must never change: if
//     they do, something wrote into memory a guest only reads. And a private
//     page is one region's own, so two regions reaching one is one guest
//     writing into another's memory. Neither check has ever fired, across
//     sixteen runs that produced eight guest panics between them — so the
//     corruption is not a shared page being overwritten.
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
	"context"
	"fmt"
	"hash/crc32"
)

// probeState is the audit's own memory: what each published page held when it
// was last looked at. It is keyed by the page rather than kept on it so that an
// ordinary build carries none of it.
type probeState struct {
	sums map[*resident]uint32
}

// stable checks that a published page's bytes have not changed since the last
// time anything held its lock. Caller holds that lock.
func (p *probeState) stable(ctx context.Context, h *Host, pg *resident, where string) {
	if pg == nil || !pg.published() || pg.slot < 0 {
		return
	}
	buf := make([]byte, h.pageSize)
	if err := h.arena.Read(ctx, pg.slot, buf); err != nil {
		return
	}
	sum := crc32.ChecksumIEEE(buf)
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.sums == nil {
		p.sums = make(map[*resident]uint32)
	}
	previous, seen := p.sums[pg]
	if !seen {
		p.sums[pg] = sum
		return
	}
	if previous != sum {
		panic(fmt.Sprintf("probe %s: published page %+v in slot %d changed its bytes (%d aliases)",
			where, pg.key.id, pg.slot, len(pg.aliases)))
	}
}

// bind checks that a private page is reached from one region only. Caller holds
// the host lock.
func (p *probeState) bind(h *Host, b *binding, pg *resident) {
	if !pg.private {
		return
	}
	for other := range pg.aliases {
		if other.region != b.region {
			panic(fmt.Sprintf("probe bind: private slot %d is reached from two regions, pages %d and %d",
				pg.slot, other.index, b.index))
		}
	}
}
