package checkpoint_test

import (
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/rank"
)

// A change of the deployment's code as reads see it: every window is read
// and rebuilt under the code it was stored under, so a deliberate change
// leaves every earlier window readable until it ages out
// (docs/hosting.md, "The code").

// codeChanges are the changes the tests make: a narrower code on six hosts,
// and a wider one round three.
var codeChanges = []struct {
	hosts         int
	before, after rank.Code
}{{6, rank.Code{K: 4, M: 2}, rank.Code{K: 2, M: 1}}, {3, rank.Code{K: 2, M: 1}, rank.Code{K: 4, M: 2}}}

// changeCode has every host hold the list under after, which names the code
// the cluster had as the one before it.
func (c *fillCluster) changeCode(t *testing.T, after rank.Code) rank.List {
	t.Helper()
	held := c.list.Load()
	changed, err := rank.NewList(after, held.Caches(), held.Code())
	if err != nil {
		t.Fatal(err)
	}
	c.hold(changed)
	return changed
}

// placedUnder is, by host, the indices of window's stripes each host holds
// under code.
func (c *fillCluster) placedUnder(window rank.Window, code rank.Code) [][]int {
	held := make([][]int, len(c.hosts))
	for at, h := range c.hosts {
		held[at] = h.cache.HeldIndices(window, 0, code)
	}
	return held
}

// rankedUnder is, by host, the indices of window's stripes list puts on each.
func (c *fillCluster) rankedUnder(list rank.List, window rank.Window) [][]int {
	held := make([][]int, len(c.hosts))
	for index, holder := range list.Holders(window) {
		for at, h := range c.hosts {
			if holder.Identity == h.cache.Identity() {
				held[at] = append(held[at], index)
			}
		}
	}
	return held
}

// The code changes, and every earlier window is still read from the cluster.
// A checkpoint published under one code is read, after the deployment moved
// to another and named the first as earlier, by every host with no request of
// the store but the open of its index object: under 4+2 then 2+1 on six
// hosts, and under 2+1 then 4+2 round three. On six hosts it holds with any
// one host lost too, which 4+2 survives. The first read of each window
// rebuilds it under the earlier code. It then fills it under the new one, so
// the readers after it read it under the new code: all but the windows whose
// rank 1 under the new code is the lost host, which gives no fill right.
func TestAChangedCodeReadsEveryEarlierWindowWithNoStoreRead(t *testing.T) {
	for _, change := range codeChanges {
		for _, lost := range []int{-1, 1} {
			if lost >= 0 && change.hosts < 6 {
				continue
			}
			name := fmt.Sprintf("%s-then-%s/lost-%d", change.before, change.after, lost)
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := newFillCluster(t, fillConfig{hosts: change.hosts, code: change.before, share: 100})
					pages := []uint64{0, 1, 2, 3}
					ref, m := c.filled(t, 0, "vm", pages)
					changed := c.changeCode(t, change.after)
					// unfilled is how many of the windows read stay under the
					// earlier code: those whose rank 1 is the lost host.
					unfilled := uint64(0)
					if lost >= 0 {
						c.hosts[lost].shut()
						windows := []rank.Window{segmentWindow(ref)}
						for _, page := range pages {
							windows = append(windows, pageWindow(ref, page))
						}
						for _, window := range windows {
							if changed.Ranks(window)[0].Identity == c.hosts[lost].cache.Identity() {
								unfilled++
							}
						}
					}
					first := true
					for at, h := range c.hosts {
						if at == lost {
							continue
						}
						if gets := c.readEvery(t, h, ref, m, pages); gets != int64(len(pages)) {
							t.Fatalf("%s made %d requests of the store for %d pages after the code changed, want only the opens",
								h.name, gets, len(pages))
						}
						c.settle(t)
						stats := h.cache.Stats().Read
						want := unfilled
						if first {
							// Every page and the segment that locates them.
							want, first = uint64(len(pages))+1, false
						}
						if stats.Misses != 0 || stats.EarlierHits != want {
							t.Fatalf("%s read %+v, want no miss and %d windows under the earlier code", h.name, stats, want)
						}
					}
				})
			})
		}
	}
}

// A window read under an earlier code is filled under the deployment's code,
// as a read of the store fills it, and its stripes under the earlier code are
// left to age out. Every stripe a fill writes after the change is of the new
// code: those of a publication, of a read of the store, and of a read under
// the earlier code.
func TestFillsAfterACodeChangeAreUnderTheNewCode(t *testing.T) {
	for _, change := range codeChanges {
		t.Run(fmt.Sprintf("%s-then-%s", change.before, change.after), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newFillCluster(t, fillConfig{hosts: change.hosts, code: change.before, share: 100})
				earlier, m := c.filled(t, 0, "vm", []uint64{0})
				changed := c.changeCode(t, change.after)
				published, pm := c.filled(t, 1, "vm-published", []uint64{0})
				index, sm := publish(t, c.publisher, "vm-stored", []uint64{0})
				stored := index.Ref()
				reader := c.hosts[change.hosts-1]
				c.read(t, reader, earlier, m, 0)
				c.read(t, reader, stored, sm, 0)
				c.settle(t)
				for _, ref := range []control.Ref{earlier, published, stored} {
					for _, window := range []rank.Window{pageWindow(ref, 0), segmentWindow(ref)} {
						if got, want := c.placedUnder(window, change.after), c.rankedUnder(changed, window); !slices.EqualFunc(got, want, slices.Equal) {
							t.Fatalf("the stripes of %+v are on %v under %s, want %v", window, got, change.after, want)
						}
					}
				}
				before := c.list.Load().Under(change.before)
				for _, window := range []rank.Window{pageWindow(earlier, 0), segmentWindow(earlier)} {
					if got, want := c.placedUnder(window, change.before), c.rankedUnder(before, window); !slices.EqualFunc(got, want, slices.Equal) {
						t.Fatalf("the earlier stripes of %+v are on %v, want them where they were, %v", window, got, want)
					}
				}
				for _, ref := range []control.Ref{published, stored} {
					if got := c.placedUnder(pageWindow(ref, 0), change.before); !slices.EqualFunc(got, make([][]int, change.hosts), slices.Equal) {
						t.Fatalf("a window filled after the change has stripes %v under the earlier code", got)
					}
				}
				// A host that has read nothing reads all three under the new
				// code.
				other := c.hosts[0]
				for _, read := range []struct {
					ref control.Ref
					m   *model
				}{{earlier, m}, {published, pm}, {stored, sm}} {
					if gets := c.readEvery(t, other, read.ref, read.m, []uint64{0}); gets != 1 {
						t.Fatalf("%s made %d requests of the store for %s, want only the open", other.name, gets, read.ref)
					}
				}
				if stats := other.cache.Stats().Read; stats.EarlierHits != 0 || stats.Misses != 0 || stats.Hits != 6 {
					t.Fatalf("%s read %+v, want six hits under the new code", other.name, stats)
				}
			})
		})
	}
}

// Repair never mixes codes. After a change to 2+1 on six hosts, a window
// read once under 4+2 is filled under 2+1. A holder then loses its 2+1
// stripe, and the next read rebuilds the window under 2+1, finds the index
// no rank holds, and sends it back under 2+1. The 4+2 stripes are where they
// were, and no host was sent one.
func TestRepairAfterACodeChangeStaysUnderTheNewCode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		before, after := rank.Code{K: 4, M: 2}, rank.Code{K: 2, M: 1}
		c := newFillCluster(t, fillConfig{hosts: 6, code: before, share: 100})
		ref, m := c.filled(t, 1, "vm", []uint64{0})
		changed := c.changeCode(t, after)
		window := pageWindow(ref, 0)
		earlier := c.placedUnder(window, before)
		c.read(t, c.hosts[0], ref, m, 0)
		c.settle(t)
		ranked := c.rankedUnder(changed, window)
		if got := c.placedUnder(window, after); !slices.EqualFunc(got, ranked, slices.Equal) {
			t.Fatalf("after the first read the stripes are on %v under 2+1, want %v", got, ranked)
		}
		lost := c.hostOf(changed.Ranks(window)[0])
		index := lost.cache.HeldIndices(window, 0, after)[0]
		if err := lost.cache.Drop(c.ctx(t), peer.Drop{Window: window, Index: index, Code: after}); err != nil {
			t.Fatal(err)
		}
		reader := c.hosts[5]
		c.read(t, reader, ref, m, 0)
		c.settle(t)
		if got := c.placedUnder(window, after); !slices.EqualFunc(got, ranked, slices.Equal) {
			t.Fatalf("after the repair the stripes are on %v under 2+1, want %v", got, ranked)
		}
		if got := c.placedUnder(window, before); !slices.EqualFunc(got, earlier, slices.Equal) {
			t.Fatalf("after the repair the 4+2 stripes are on %v, want them as they were, %v", got, earlier)
		}
		if stats := reader.cache.Stats().Read; stats.Repairs != 1 || stats.EarlierHits != 0 {
			t.Fatalf("the second reader's reads came to %+v, want one repair and nothing under 4+2", stats)
		}
	})
}

// A code the list no longer names is a miss. Once the deployment drops the
// earlier code, a window kept only under it is read from the store, never
// rebuilt from stripes of a code the list does not name.
func TestADroppedEarlierCodeIsAMiss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100})
		ref, m := c.filled(t, 0, "vm", []uint64{0})
		held := c.list.Load()
		dropped, err := rank.NewList(rank.Code{K: 2, M: 1}, held.Caches())
		if err != nil {
			t.Fatal(err)
		}
		c.hold(dropped)
		reader := c.hosts[1]
		if gets := c.readEvery(t, reader, ref, m, []uint64{0}); gets != 3 {
			t.Fatalf("%s made %d requests of the store, want the index, the segment and the page", reader.name, gets)
		}
		if stats := reader.cache.Stats().Read; stats.Misses != 2 || stats.Hits != 0 || stats.EarlierHits != 0 {
			t.Fatalf("%s read %+v, want two misses", reader.name, stats)
		}
	})
}
