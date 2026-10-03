package checkpoint_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/rank"
)

// refusingBudget is a disk limiter whose share is fixed and whose write budget
// refuses one kind of write, as one that has run low refuses the lower kinds.
type refusingBudget struct {
	share  int64
	refuse checkpoint.WriteKind
}

func (b refusingBudget) Share() int64 { return b.share }

func (b refusingBudget) Admit(_ int64, kind checkpoint.WriteKind) bool { return kind != b.refuse }

// A keep is written at the priority of the fill it carries. With the second
// of two hosts under 1+1 refusing fills from reads of the store, as a write
// budget that runs low does before it refuses fills from publications, a
// window the first host publishes is kept on both, and a window it reads from
// the store is kept on its own disk alone: the second host drops it, and the
// first counts it dropped by its holder.
func TestAKeepIsWrittenAtThePriorityOfItsFill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 2, code: rank.Code{K: 1, M: 1}, share: 100,
			cache: func(host int, config *checkpoint.CacheConfig) {
				if host == 1 {
					config.DiskBytes = 0
					config.Budget = refusingBudget{share: 256 << 20, refuse: checkpoint.WriteFillRead}
				}
			}})
		published, _, err := publishFrom(t, c.hosts[0].store, "published", []uint64{0})
		if err != nil {
			t.Fatal(err)
		}
		read, m := publish(t, c.publisher, "read", []uint64{0})
		c.read(t, c.hosts[0], read.Ref(), m, 0)
		c.settle(t)
		// Under 1+1 each host holds one whole copy of every window, at the
		// index the list puts on it.
		for _, window := range []rank.Window{pageWindow(published.Ref(), 0), segmentWindow(published.Ref())} {
			if got := c.placed(window); !slices.EqualFunc(got, c.ranked(window), slices.Equal) ||
				len(got[0]) != 1 || len(got[1]) != 1 {
				t.Fatalf("the published window %+v is on %v, want a copy on each host", window, got)
			}
		}
		for _, window := range []rank.Window{pageWindow(read.Ref(), 0), segmentWindow(read.Ref())} {
			if got, ranked := c.placed(window), c.ranked(window); !slices.Equal(got[0], ranked[0]) || len(got[1]) != 0 {
				t.Fatalf("the window %+v read from the store is on %v, want the reader's own copy %v alone", window,
					got, ranked[0])
			}
		}
		reader, refuser := c.hosts[0].cache.Stats().Fill, c.hosts[1].cache.Stats().Fill
		if reader.Dropped[checkpoint.DropPeer] != 2 || refuser.Dropped[checkpoint.DropDisk] != 2 ||
			refuser.Kept != 2 {
			t.Fatalf("the reader's fills came to %+v and the refusing host's to %+v; want the read's two windows "+
				"dropped there and the publication's two kept", reader, refuser)
		}
	})
}
