package checkpoint

import (
	"bytes"
	"context"
	"fmt"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/rank"
)

// The cache answers its peers through the host's peer server: it is the
// peer.Cache the server hands the cache's requests to. Every request names the
// disk it is for, and is answered from that disk while the cache keeps it: the
// host's own, or a shard it serves. The peer server asks only under a
// membership that has this host serve the disk.
var _ peer.Cache = (*Cache)(nil)

// Identity is the identity of the host's own disk, which the membership ranks
// windows by: the one in its file's header, zero for a cache that keeps none.
func (c *Cache) Identity() rank.Identity {
	if c.disk == nil {
		return rank.Identity{}
	}
	return c.disk.identity
}

// Disks is the identity of every disk the cache keeps now, its own and the
// shards it serves, in order.
func (c *Cache) Disks() []rank.Identity { return c.cluster.disks() }

// held is the disk a peer's request names, held until release is called, or
// ErrNotKept.
func (c *Cache) held(disk rank.Identity) (*cacheDisk, func(), error) {
	found, release, kept := c.cluster.hold(disk)
	if !kept {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotKept, disk)
	}
	return found, release, nil
}

// ReadStripes answers a peer's read of a window's stripes with every stripe
// disk holds of the pages asked for under the read's code, of any index, each
// as the disk stores it: its header and its own checksum, which the reader
// checks. A holder never forwards a read and never reads the store for a
// reader: what it does not hold, it says nothing of. A read that wants no
// bytes asks only for the fill right, which this cache gives the first reader
// that asks while m, the membership at the read's generation, ranks the disk
// first for the window, while it holds nothing of the pages asked for, and
// once a window an interval.
func (c *Cache) ReadStripes(ctx context.Context, m membership.Membership, disk rank.Identity,
	read peer.StripeRead) (peer.Stripes, error) {
	found, release, err := c.held(disk)
	if err != nil {
		return peer.Stripes{}, err
	}
	defer release()
	if read.MaxBytes == 0 {
		return peer.Stripes{FillRight: c.filler.grant(ctx, found, m, read.Window, read.Pages, read.Code)}, nil
	}
	if !validWindow(read.Window, read.Pages) || read.Code.Validate() != nil || read.MaxBytes < 0 {
		return peer.Stripes{}, fmt.Errorf("%w: a read of %+v under %s", errKeepRefused, read.Window, read.Code)
	}
	items, payload := found.serveStripes(ctx, read.Window, read.Pages, read.Code, read.MaxBytes)
	return peer.Stripes{Items: items, Payload: bytes.NewReader(payload), Size: int64(len(payload))}, nil
}

// Keep writes the stripes a peer's keep of disk carries, where m, the
// membership at the keep's generation, ranks the disk for the window under the
// keep's code: every one it does not hold or write already, through the
// host's queue of writes to its disks, at the keep's priority. It reports
// peer.ErrDropped when it writes none.
func (c *Cache) Keep(ctx context.Context, m membership.Membership, disk rank.Identity, keep peer.Keep) error {
	found, release, err := c.held(disk)
	if err != nil {
		return fmt.Errorf("%w: %w", peer.ErrDropped, err)
	}
	defer release()
	return c.filler.keep(ctx, m, found, keep)
}

// Drop forgets one stripe of disk a reader found wrong. A stripe the disk
// does not hold is already forgotten.
func (c *Cache) Drop(ctx context.Context, disk rank.Identity, drop peer.Drop) error {
	found, release, err := c.held(disk)
	if err != nil {
		return err
	}
	defer release()
	if !validWindow(drop.Window, []uint32{drop.Page}) || drop.Code.Validate() != nil || drop.Index < 0 ||
		drop.Index >= drop.Code.Width() {
		return fmt.Errorf("%w: a drop of stripe %d of %s of page %d of %+v", errKeepRefused, drop.Index, drop.Code,
			drop.Page, drop.Window)
	}
	found.forgetStripe(ctx, windowKey(drop.Window, drop.Page), indexOf(drop.Code, drop.Index),
		fmt.Errorf("a reader found stripe %d of %s wrong", drop.Index, drop.Code))
	return nil
}

// Presence reports, for each window asked, the stripes of it disk holds under
// the code: for each index, the pages it holds that index of. A pull asks it
// of a window's ranks to learn whether the cluster holds k distinct indices
// of each page, and reads from the store only the pages it does not.
func (c *Cache) Presence(_ context.Context, disk rank.Identity, presence peer.Presence) ([]peer.Present, error) {
	found, release, err := c.held(disk)
	if err != nil {
		return nil, err
	}
	defer release()
	if presence.Code.Validate() != nil {
		return nil, fmt.Errorf("%w: presence under %s", errKeepRefused, presence.Code)
	}
	held := make([]peer.Present, 0, len(presence.Windows))
	for _, window := range presence.Windows {
		if !validWindow(window, nil) {
			return nil, fmt.Errorf("%w: presence of %+v", errKeepRefused, window)
		}
		held = append(held, found.present(window, nil, presence.Code))
	}
	return held, nil
}
