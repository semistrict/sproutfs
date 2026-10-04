package checkpoint

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// A cache drops a keep for stripes it already holds, and for stripes it is
// already writing: a second keep of a window, sent once the first is written,
// and one sent while the first is still being written, a second into a disk
// write, are each answered dropped at once, write nothing, and count each of
// their stripes a duplicate.
func TestACacheDropsAKeepItHoldsOrIsWriting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 1, M: 1}
		f := newKeepFixture(t, 100, sim.DiskConfig{WriteLatency: time.Second}, func(self CacheIdentity) rank.List {
			return listOf(code, self, otherCache)
		})
		held := keyOf("held", 0)
		if err := f.cache.Keep(t.Context(), f.m, f.cache.Identity(), keepOf(t, held, code, []int{0, 1})); err != nil {
			t.Fatal(err)
		}
		if err := f.cache.Keep(t.Context(), f.m, f.cache.Identity(), keepOf(t, held, code, []int{0, 1})); !errors.Is(err, peer.ErrDropped) {
			t.Fatalf("a keep of stripes the cache holds = %v, want it dropped", err)
		}
		if fill := f.cache.Stats().Fill; fill.Kept != 2 || fill.Duplicates != 2 {
			t.Fatalf("a keep sent twice came to %+v, want two stripes kept and two duplicates", fill)
		}
		writing := keyOf("writing", 0)
		first := make(chan error, 1)
		go func() {
			first <- f.cache.Keep(t.Context(), f.m, f.cache.Identity(), keepOf(t, writing, code, []int{0, 1}))
		}()
		synctest.Wait()
		start := time.Now()
		if err := f.cache.Keep(t.Context(), f.m, f.cache.Identity(), keepOf(t, writing, code, []int{0, 1})); !errors.Is(err, peer.ErrDropped) {
			t.Fatalf("a keep of stripes the cache is writing = %v, want it dropped", err)
		}
		if took := time.Since(start); took != 0 {
			t.Fatalf("a keep of stripes the cache is writing waited %v for the write", took)
		}
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if fill := f.cache.Stats().Fill; fill.Kept != 4 || fill.Duplicates != 4 {
			t.Fatalf("a keep sent while the first was written came to %+v, want four kept and four duplicates", fill)
		}
		if got := f.held(writing, code); !slices.Equal(got, []int{0, 1}) {
			t.Fatalf("the cache holds %v of the window, want both copies", got)
		}
	})
}

// The window's rank 1 gives its fill right to the first reader that asks, and
// to no other until the interval is over; then to the next that asks, while
// it still holds nothing of the pages asked for. A cache gives none for a
// window another cache ranks first for, under another code, or for pages
// past the window.
func TestRankOneGivesOneFillRightPerWindowPerInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 2, M: 1}
		f := newKeepFixture(t, 100, sim.DiskConfig{}, func(self CacheIdentity) rank.List {
			return listOf(code, self, fiveOthers...)
		})
		self := f.cache.Identity()
		first := func(want bool) func(ranks []rank.Cache) bool {
			return func(ranks []rank.Cache) bool { return (ranks[0].Identity == self) == want }
		}
		var mine, fresh, theirs diskKey
		for at := range 4096 {
			key := keyOf("vm", uint64(at)*512)
			ranks := f.list.Ranks(key.rankWindow())
			switch {
			case first(true)(ranks) && mine == (diskKey{}):
				mine = key
			case first(true)(ranks) && fresh == (diskKey{}):
				fresh = key
			case first(false)(ranks) && theirs == (diskKey{}):
				theirs = key
			}
		}
		right := func(key diskKey, pages []uint32, code rank.Code) bool {
			t.Helper()
			stripes, err := f.cache.ReadStripes(t.Context(), f.m, f.cache.Identity(), peer.StripeRead{Window: key.rankWindow(), Pages: pages,
				Code: code})
			if err != nil {
				t.Fatal(err)
			}
			if len(stripes.Items) != 0 || stripes.Size != 0 {
				t.Fatalf("a read of stripes was answered with %d of them", len(stripes.Items))
			}
			return stripes.FillRight
		}
		page := []uint32{0}
		if !right(mine, page, code) {
			t.Fatal("rank 1 gave no right to the first reader that asked")
		}
		if right(mine, page, code) || right(mine, nil, code) {
			t.Fatal("rank 1 gave a window's right twice in one interval")
		}
		f.clock.Advance(DefaultFillRightInterval - time.Nanosecond)
		if right(mine, page, code) {
			t.Fatal("rank 1 gave the right again before its interval was over")
		}
		f.clock.Advance(time.Nanosecond)
		if !right(mine, page, code) {
			t.Fatal("rank 1 gave no right once the interval was over")
		}
		if right(theirs, page, code) || right(fresh, page, rank.Code{K: 1, M: 1}) || right(fresh, []uint32{512}, code) {
			t.Fatal("a cache gave a right for a window it does not rank first, another code, or a page past the window")
		}
		if !right(fresh, []uint32{511}, code) {
			t.Fatal("rank 1 gave no right for the last page of a window")
		}
		// Once it holds a stripe of the page asked for, it gives none.
		if err := f.cache.Keep(t.Context(), f.m, f.cache.Identity(), keepOf(t, mine, code, []int{0})); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(DefaultFillRightInterval)
		if right(mine, page, code) {
			t.Fatal("rank 1 gave the right to fill a page it holds")
		}
		if fill := f.cache.Stats().Fill; fill.RightsGranted != 3 {
			t.Fatalf("rank 1 reports %d rights given, want 3", fill.RightsGranted)
		}
	})
}

// A cache tells a peer which stripes of a window it holds, by index, and
// forgets a stripe a reader tells it is wrong.
func TestACacheReportsItsPagesAndDropsAWrongStripe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 1, M: 1}
		f := newKeepFixture(t, 100, sim.DiskConfig{}, func(self CacheIdentity) rank.List {
			return listOf(code, self, otherCache)
		})
		third := keyOf("vm", 3)
		if err := f.cache.Keep(t.Context(), f.m, f.cache.Identity(), keepOf(t, third, code, []int{0, 1})); err != nil {
			t.Fatal(err)
		}
		window := third.rankWindow()
		held, err := f.cache.Presence(t.Context(), f.cache.Identity(), peer.Presence{Windows: []rank.Window{window, keyOf("vm", 512).rankWindow()},
			Code: code})
		if err != nil {
			t.Fatal(err)
		}
		want := []peer.Present{{{3}, {3}}, {nil, nil}}
		if !slices.EqualFunc(held, want, func(a, b peer.Present) bool { return slices.EqualFunc(a, b, slices.Equal) }) {
			t.Fatalf("the cache reports %v of the two windows, want both indices of page 3 of the first", held)
		}
		if err := f.cache.Drop(t.Context(), f.cache.Identity(), peer.Drop{Window: window, Page: 3, Index: 1, Code: code}); err != nil {
			t.Fatal(err)
		}
		if got := f.held(third, code); !slices.Equal(got, []int{0}) {
			t.Fatalf("after a drop of stripe 1 the cache holds %v, want stripe 0", got)
		}
		if err := f.cache.Drop(t.Context(), f.cache.Identity(), peer.Drop{Window: window, Page: 3, Index: 0, Code: code}); err != nil {
			t.Fatal(err)
		}
		if got := f.held(third, code); len(got) != 0 {
			t.Fatalf("after a drop of both stripes the cache holds %v", got)
		}
		if err := f.cache.Drop(t.Context(), f.cache.Identity(), peer.Drop{Window: window, Page: 3, Index: 2, Code: code}); err == nil {
			t.Fatal("a drop of an index past the code was taken")
		}
	})
}
