package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// IndexBytes is an index's encoded form, so a test can require that two indexes
// are the same table rather than merely that they behave alike.
func IndexBytes(index *Index) ([]byte, error) { return index.encode() }

// LiveBuilders reports how many publications hold a part builder right now,
// which is what the host-wide builder budget bounds.
func LiveBuilders(s *Store) int {
	s.builderMu.Lock()
	defer s.builderMu.Unlock()
	return s.liveBuilders
}

// HeldIndices is the indices of stripes of page at of window the cache's disk
// holds under code, in index order: what a test of where fills put stripes
// asks of each cache.
func (c *Cache) HeldIndices(window rank.Window, at uint32, code rank.Code) []int {
	if c.disk == nil {
		return nil
	}
	var held []int
	for index := range code.Width() {
		if c.disk.holdsStripe(windowKey(window, at), indexOf(code, index)) {
			held = append(held, index)
		}
	}
	return held
}

// FillSites are the fault-injection sites of fills, which the fill campaign
// must fire.
var FillSites = []string{buggifyFillQueueFull, buggifyFillLoseRight, buggifyFillRanksChange, buggifyFillSendTwice,
	buggifyFillRefuseWrite, buggifyKeepDrop}

// SpoilStripe replaces stripe index of page at of window, which the cache's
// disk holds under code, with one whose bytes are wrong and whose checksum,
// written with them, holds: what a peer that answers with a wrong stripe
// holds.
func (c *Cache) SpoilStripe(ctx context.Context, window rank.Window, at uint32, code rank.Code, index int) error {
	key := windowKey(window, at)
	s, outcome := c.disk.readStripe(ctx, key, code, index)
	if outcome != diskHit {
		return fmt.Errorf("the disk holds no stripe %d of %s of page %d of %+v", index, code, at, window)
	}
	spoiled := bytes.Clone(s.Bytes)
	spoiled[len(spoiled)/2] ^= 0x40
	s.Bytes = spoiled
	c.disk.forgetStripe(ctx, key, indexOf(code, index), errors.New("spoiled by the test"))
	_, err := c.disk.writeStripes(ctx, key, []stripe.Stripe{s}, WriteFillPublication)
	return err
}

// ReadSites are the fault-injection sites of reads of the cluster, which the
// read campaign must fire.
var ReadSites = []string{buggifyClusterWrongStripe, buggifyClusterDamagedItem, buggifyClusterLoseAnswer,
	buggifyClusterStoreHedgeNow, buggifyClusterFalseTimeout}

// ServingOf is the membership in which every cache of list is a member of its
// own serving its own disk, at the cache's address or at one named for it:
// what a test that places windows by a fixed set of disks follows.
func ServingOf(list rank.List) membership.Membership { return servingOf(list) }

// FollowList has the cache follow ServingOf(list), as the member of its own
// disk.
func (c *Cache) FollowList(list rank.List) {
	c.FollowMembership(membership.NewFixed(servingOf(list)), c.Identity())
}
