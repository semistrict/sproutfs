package vmmemory

import (
	"context"
	"hash/maphash"
	"sync"
)

// A checkpoint publishes whole pages, and a page is dirty from a guest's first
// store into it however little that store changed. What a checkpoint costs
// against what the guest actually changed is therefore a number the pager can
// only know by looking, and Config.MeasureChanges is it looking: when a page
// becomes private its bytes are summed a changeBlock at a time, and when a
// checkpoint settles the page its sealed bytes are summed again, so the blocks
// whose sums differ are the blocks the guest changed. It is instrumentation
// for a benchmark — summing a 2 MiB page is a read of all of it, on the fault
// that copies it and again behind the seal — and nothing decides anything by
// it.

// changeBlock is the unit changes are counted in: the page a filesystem writes.
const changeBlock = 4096

// changeSums are one page's block sums when it became private. Nil sums are a
// page that became private as zeros.
type changeSums struct{ sums []uint64 }

// changes is one region's measurement state: the sums of every private page by
// page, and those a seal has handed to a checkpoint's copy by that copy.
type changes struct {
	mu     sync.Mutex
	byPage map[uint64]changeSums
	byHeld map[*binding]changeSums
}

// measuring reports a pager that counts changed blocks.
func (h *Host) measuring() bool { return h.cfg.MeasureChanges }

// blockSums sums the bytes one arena slot holds, a changeBlock at a time.
func (h *Host) blockSums(ctx context.Context, slot int) ([]uint64, error) {
	data := make([]byte, h.pageSize)
	if err := h.arena.Read(ctx, slot, data); err != nil {
		return nil, err
	}
	sums := make([]uint64, 0, len(data)/changeBlock)
	for at := 0; at < len(data); at += changeBlock {
		sums = append(sums, maphash.Bytes(h.changeSeed, data[at:min(at+changeBlock, len(data))]))
	}
	return sums, nil
}

// noteCopied records what a page held as it became private: the bytes of the
// resident page it now owns, before the guest can store into them.
func (r *Region) noteCopied(ctx context.Context, index uint64, pg *resident) error {
	sums, err := r.host.blockSums(ctx, pg.slot)
	if err != nil {
		return err
	}
	r.noteSums(index, changeSums{sums: sums})
	return nil
}

// noteZeroed records a page that became private as zeros.
func (r *Region) noteZeroed(index uint64) { r.noteSums(index, changeSums{}) }

func (r *Region) noteSums(index uint64, sums changeSums) {
	r.changes.mu.Lock()
	defer r.changes.mu.Unlock()
	if r.changes.byPage == nil {
		r.changes.byPage = make(map[uint64]changeSums)
	}
	r.changes.byPage[index] = sums
}

// sealSums hands a page's sums to the checkpoint copy a seal made of it: the
// next store copies away from that copy and records sums of its own.
func (r *Region) sealSums(index uint64, held *binding) {
	r.changes.mu.Lock()
	defer r.changes.mu.Unlock()
	sums, ok := r.changes.byPage[index]
	if !ok {
		return
	}
	delete(r.changes.byPage, index)
	if r.changes.byHeld == nil {
		r.changes.byHeld = make(map[*binding]changeSums)
	}
	r.changes.byHeld[held] = sums
}

// unsealSums gives an abandoned checkpoint's sums back to the page.
func (r *Region) unsealSums(index uint64, held *binding) {
	r.changes.mu.Lock()
	defer r.changes.mu.Unlock()
	sums, ok := r.changes.byHeld[held]
	if !ok {
		return
	}
	delete(r.changes.byHeld, held)
	if r.changes.byPage == nil {
		r.changes.byPage = make(map[uint64]changeSums)
	}
	r.changes.byPage[index] = sums
}

// takeSums is the sums a checkpoint's copy was sealed with, which the settle
// consumes.
func (r *Region) takeSums(held *binding) (changeSums, bool) {
	r.changes.mu.Lock()
	defer r.changes.mu.Unlock()
	sums, ok := r.changes.byHeld[held]
	delete(r.changes.byHeld, held)
	return sums, ok
}

// changedBlocks counts the blocks of one sealed page whose bytes differ from
// what the page held as it became private, and reports false where that is not
// known: a page private since before it was measured, one another host made
// private, or one spilled since the seal.
func (s *settler) changedBlocks(ctx context.Context, c *RegionCheckpoint, held *binding) (int, bool, error) {
	r := c.region
	h := r.host
	was, ok := r.takeSums(held)
	if !ok {
		return 0, false, nil
	}
	pg, err := h.current(ctx, held)
	if err != nil || pg == nil {
		return 0, false, err
	}
	defer h.unlock(pg)
	now, err := h.blockSums(ctx, pg.slot)
	if err != nil {
		return 0, false, err
	}
	zero := h.zeroBlockSum()
	changed := 0
	for i, sum := range now {
		before := zero
		if was.sums != nil {
			before = was.sums[i]
		}
		if sum != before {
			changed++
		}
	}
	return changed, true, nil
}

// zeroBlockSum is the sum of a block of zeros.
func (h *Host) zeroBlockSum() uint64 {
	return maphash.Bytes(h.changeSeed, make([]byte, min(changeBlock, int(h.pageSize))))
}
