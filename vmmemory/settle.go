package vmmemory

import (
	"bytes"
	"context"
	"time"
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
	return c.zircon().settle(ctx, c)
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
