package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/rank"
)

// The cache answers its peers through the host's peer server: it is the
// peer.Cache the server hands the cache's requests to. A host serves it only
// while its cache keeps a disk, so every method below has one.
var _ peer.Cache = (*Cache)(nil)

// errNoDisk is what a peer's request of a cache that keeps no disk is told.
var errNoDisk = errors.New("checkpoint: the cache keeps no disk")

// Identity is the cache's identity in the list of caches: its disk's, zero
// for a cache that keeps none.
func (c *Cache) Identity() rank.Identity {
	if c.disk == nil {
		return rank.Identity{}
	}
	return c.disk.identity
}

// ReadStripes answers a peer's read of a window's stripes. It answers with no
// stripes yet: no host reads stripes from its peers until the cluster is read
// from (plans/disk-cache-2026-10-02.md, step 7), and a read with nothing in it
// is a miss. What it answers already is the fill right, which this cache gives
// the first reader that asks while it ranks first for the window, holds nothing
// of the pages asked for, and has not given the window's right out this
// interval.
func (c *Cache) ReadStripes(ctx context.Context, read peer.StripeRead) (peer.Stripes, error) {
	if c.disk == nil {
		return peer.Stripes{}, errNoDisk
	}
	return peer.Stripes{FillRight: c.filler.grant(ctx, read.Window, read.Pages, read.Code)}, nil
}

// Keep writes the stripes a peer's keep carries, where this cache's own list
// ranks it for the window under the keep's code: every one it does not hold
// or write already, through the host's queue of writes to its disk, at the
// keep's priority. It reports peer.ErrDropped when it writes none.
func (c *Cache) Keep(ctx context.Context, keep peer.Keep) error {
	if c.disk == nil {
		return fmt.Errorf("%w: %w", peer.ErrDropped, errNoDisk)
	}
	return c.filler.keep(ctx, keep)
}

// Drop forgets one stripe a reader found wrong. A stripe the cache does not
// hold is already forgotten.
func (c *Cache) Drop(ctx context.Context, drop peer.Drop) error {
	if c.disk == nil {
		return errNoDisk
	}
	if !validWindow(drop.Window, []uint32{drop.Page}) || drop.Code.Validate() != nil || drop.Index < 0 ||
		drop.Index >= drop.Code.Width() {
		return fmt.Errorf("%w: a drop of stripe %d of %s of page %d of %+v", errKeepRefused, drop.Index, drop.Code,
			drop.Page, drop.Window)
	}
	c.disk.forgetStripe(ctx, windowKey(drop.Window, drop.Page), indexOf(drop.Code, drop.Index),
		fmt.Errorf("a reader found stripe %d of %s wrong", drop.Index, drop.Code))
	return nil
}

// Presence reports, for each window asked, the pages of it the cache holds a
// stripe of under the code, of any index.
func (c *Cache) Presence(_ context.Context, presence peer.Presence) ([][]uint32, error) {
	if c.disk == nil {
		return nil, errNoDisk
	}
	held := make([][]uint32, 0, len(presence.Windows))
	for _, window := range presence.Windows {
		if !validWindow(window, nil) {
			return nil, fmt.Errorf("%w: presence of %+v", errKeepRefused, window)
		}
		held = append(held, c.disk.heldPages(window, nil, presence.Code))
	}
	return held, nil
}
