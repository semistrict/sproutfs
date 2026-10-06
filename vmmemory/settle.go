package vmmemory

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// A write fault is not always a store. KVM finishes a guest's cold read from a
// worker thread that always asks for the page writable, and an architecture can
// report a guest kernel's cache maintenance on a page it is about to execute as
// a write. The pager cannot tell those faults from real ones while they wait —
// the worker will not finish until the page is writable — so it copies, and
// tells afterwards: a sealed page whose bytes are the ones the page it was
// copied from still holds is not dirty, and the checkpoint publishes nothing
// for it.

// Settle drops from this checkpoint every page whose sealed bytes are the ones
// its origin still holds, and hands the guest's page back to that origin. It
// reports how many pages it dropped.
//
// The publication calls it once, before it enumerates the pages: behind the
// pause, with the guest running, so the seal it belongs to is unchanged and
// costs the same page-table work it always did. A fork point never calls it —
// it publishes nothing, and its children inherit an unchanged page as an
// unpublished one, which the next checkpoint of each settles.
//
// It reads no disk and no store and takes none of the pager's I/O permits, so a
// sealed page the pager has spilled is left alone: what it compares is two
// resident pages, and that comparison is the whole of what the upload is
// waiting for, so the pages are divided between Config.SettleWorkers workers
// rather than queued behind one. The workers share nothing but the count and
// the set this checkpoint will list, so what a settle leaves does not depend on
// the order they finish in.
func (c *MemoryRegionCheckpoint) Settle(ctx context.Context) (int, error) {
	r := c.memoryRegion
	h := r.host
	if err := r.live.RLock(ctx); err != nil {
		return 0, err
	}
	defer r.live.RUnlock()
	if err := r.serving(); err != nil {
		return 0, err
	}
	select {
	case <-c.done:
		return 0, nil
	default:
	}
	if c.held.Load() {
		// A fork point's seal, whose pages its children are reading.
		return 0, nil
	}
	copies := c.copiesOf()
	equal := make([]*zirconvm.VmPage, len(copies))
	dropped := make([]bool, len(copies))
	failures := make([]error, len(copies))
	workers := min(max(h.cfg.SettleWorkers, 1), len(copies))
	measuring := h.measuring()
	var changed, measured, unmeasured atomic.Uint64
	var next atomic.Int64
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var worker settler
			for {
				i := int(next.Add(1)) - 1
				if i >= len(copies) {
					return
				}
				if measuring {
					blocks, known, err := r.changedBlocks(ctx, copies[i])
					if err != nil {
						failures[i] = err
						continue
					}
					if known {
						changed.Add(uint64(blocks))
						measured.Add(1)
					} else {
						unmeasured.Add(1)
					}
				}
				equal[i], failures[i] = r.compare(ctx, &worker, copies[i])
			}
		}()
	}
	wait.Wait()
	if measuring {
		h.mu.Lock()
		h.stats.ChangedBlocks += changed.Load()
		h.stats.MeasuredPages += measured.Load()
		h.stats.UnmeasuredPages += unmeasured.Load()
		h.mu.Unlock()
	}
	if err := r.reshare(ctx, c, copies, equal, dropped); err != nil {
		failures = append(failures, err)
	}
	unchanged := 0
	for _, was := range dropped {
		if was {
			unchanged++
		}
	}
	if unchanged > 0 {
		c.forgetCopies(dropped)
		h.mu.Lock()
		h.stats.UnchangedPages += uint64(unchanged)
		h.signal()
		h.mu.Unlock()
	}
	return unchanged, errors.Join(failures...)
}

// settler is one worker. Where a file cannot compare two of its own slots, or
// the two pages are in different files, the comparison needs a page of each,
// which is made once and reused for every page that worker settles.
type settler struct{ first, second []byte }

// equal reports whether two arena slots hold the same bytes. Caller holds both
// pages' locks, so neither slot can be released or refilled while it runs.
func (s *settler) equal(ctx context.Context, h *Host, first, second fileSlot) (bool, error) {
	if comparing, ok := first.file.ArenaFile.(EqualFile); ok && first.file == second.file {
		return comparing.Equal(ctx, first.slot, second.slot)
	}
	if s.first == nil {
		s.first, s.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	if err := first.file.Read(ctx, first.slot, s.first); err != nil {
		return false, err
	}
	if err := second.file.Read(ctx, second.slot, s.second); err != nil {
		return false, err
	}
	return bytes.Equal(s.first, s.second), nil
}

// since is when the oldest write this checkpoint still holds was made, zero
// where it holds none. A settle can empty the set, so it is read under the
// checkpoint's own lock rather than off the field.
func (c *MemoryRegionCheckpoint) since() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirtySince
}
