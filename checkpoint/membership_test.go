package checkpoint_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/rank"
)

// moveOn has the membership take a step that moves no window, a member with
// no disk joining, and every host but behind read it: the hosts ahead hold a
// newer generation than behind does.
func (c *fillCluster) moveOn(t *testing.T, behind *fillHost) membership.Membership {
	t.Helper()
	ctx := c.ctx(t)
	m, err := c.members.Update(ctx, func(m membership.Membership) (membership.Membership, error) {
		return m.Join(membership.Member{ID: rank.Identity{0xfe}, Address: "host-without-a-disk"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range c.hosts {
		if h == behind {
			continue
		}
		if _, err := h.view.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// A reader behind the holders of its windows names its generation in every
// request, and every holder answers that it is stale. The reader reads the
// membership and reads each window again under the newer generation: every
// page comes from the cluster, with no request of the store but the open of
// the index object.
func TestAReaderBehindItsHoldersReadsTheMembershipAndAsksAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 3, code: rank.Code{K: 2, M: 1}, share: 100})
		pages := []uint64{0, 1, 2}
		ref, m := c.filled(t, 0, "vm", pages)
		reader := c.hosts[1]
		ahead := c.moveOn(t, reader)
		if gets := c.readEvery(t, reader, ref, m, pages); gets != int64(len(pages)) {
			t.Fatalf("a reader behind its holders made %d requests of the store for %d pages, want only an open "+
				"of the index for each", gets, len(pages))
		}
		if got := reader.view.Current().Generation(); got != ahead.Generation() {
			t.Fatalf("the reader holds generation %d, want %d", got, ahead.Generation())
		}
		probes := c.runtime.Probes()
		if probes[membership.ProbeStaleAnswered] == 0 || probes[membership.ProbeSenderCaughtUp] == 0 {
			t.Fatalf("probes %v, want stale answers and a reader that caught up", probes)
		}
	})
}

// A publisher behind the holders it fills sends each keep under its
// generation, and each holder answers that it is stale. The publisher reads
// the membership and sends the keep again: every stripe lands on the disk the
// membership ranks for it, and none is dropped as stale.
func TestAFillToHoldersAheadIsSentAgainUnderTheirGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 3, code: rank.Code{K: 2, M: 1}, share: 100})
		publisher := c.hosts[0]
		c.moveOn(t, publisher)
		ref, _ := c.filled(t, 0, "vm", []uint64{0, 1})
		for _, window := range []rank.Window{pageWindow(ref, 0), pageWindow(ref, 1), segmentWindow(ref)} {
			if placed, ranked := c.placed(window), c.ranked(window); !slices.EqualFunc(placed, ranked, slices.Equal) {
				t.Fatalf("the stripes of %+v are on %v, want %v", window, placed, ranked)
			}
		}
		if stale := c.fills().Dropped[checkpoint.DropStale]; stale != 0 {
			t.Fatalf("%d stripes were dropped as stale", stale)
		}
	})
}
