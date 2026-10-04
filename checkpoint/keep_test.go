package checkpoint

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/stripe"
)

// keepFixture is a cache with a disk, following a list made from its own
// identity, as a host whose peers send it keeps.
type keepFixture struct {
	runtime *sim.Runtime
	cache   *Cache
	clock   *sim.Clock
	list    rank.List
	// m is the membership of the list, which the cache follows and its
	// peers' requests are answered under.
	m membership.Membership
}

// newKeepFixture is a cache whose cluster cache is on for percent of windows,
// following the list caches makes from its identity, on a disk of config.
func newKeepFixture(t *testing.T, percent int, disk sim.DiskConfig,
	caches func(self CacheIdentity) rank.List) *keepFixture {
	t.Helper()
	f := &keepFixture{runtime: sim.New(sim.Config{Seed: 1})}
	f.clock = f.runtime.NewClock("host")
	file, err := f.runtime.NewDisk("host", disk).Open(t.Context(), "cache", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	budget, err := resource.New(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	f.cache, err = NewCache(sim.WithRuntime(t.Context(), f.runtime), budget, CacheConfig{Disk: file,
		DiskBytes: 64 << 20, DiskRegionBytes: 1 << 20, ClusterPercent: percent, Clock: f.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.cache.Close)
	f.list = caches(f.cache.Identity())
	f.m = servingOf(f.list)
	f.cache.FollowMembership(membership.NewFixed(f.m), f.cache.Identity())
	return f
}

// fiveOthers are five caches beside the fixture's own, for a list of six.
var fiveOthers = []CacheIdentity{{0xee, 0x01}, {0xee, 0x02}, {0xee, 0x03}, {0xee, 0x04}, {0xee, 0x05}}

// keepOf is what a peer sends to keep the stripes indices of key's envelope
// under code: each an item as the disk stores it.
func keepOf(t *testing.T, key diskKey, code rank.Code, indices []int) peer.Keep {
	t.Helper()
	stripes, err := stripe.Split(code, payloadOf(key, 3000))
	if err != nil {
		t.Fatal(err)
	}
	window := key.rankWindow()
	keep := peer.Keep{Window: window, Code: code}
	for _, index := range indices {
		item := encodeItem(key, stripes[index])
		keep.Items = append(keep.Items, peer.StripeItem{Page: uint32(key.Page - window.Page(0)), Index: index,
			Length: stripes[index].Length, Size: len(item)})
		keep.Payload = append(keep.Payload, item...)
	}
	return keep
}

// windowWhere is the first page of the first window of vm whose ranks under
// the fixture's list do or do not hold its own cache, as ranked says, and
// that is or is not inside the share of percent, as inShare says.
func (f *keepFixture) windowWhere(t *testing.T, vm string, ranked, inShare bool, percent int) diskKey {
	t.Helper()
	self := f.cache.Identity()
	for at := range 4096 {
		key := keyOf(vm, uint64(at)*512)
		window := key.rankWindow()
		if ranks := f.list.Ranks(window); slices.ContainsFunc(ranks, func(cache rank.Cache) bool {
			return cache.Identity == self
		}) == ranked && window.InShare(percent) == inShare {
			return key
		}
	}
	t.Fatalf("no window of %s is ranked %v and in the share %v", vm, ranked, inShare)
	return diskKey{}
}

// held is the indices of key's stripes under code the cache holds.
func (f *keepFixture) held(key diskKey, code rank.Code) []int {
	var held []int
	for index := range code.Width() {
		if f.cache.disk.holdsStripe(key, indexOf(code, index)) {
			held = append(held, index)
		}
	}
	return held
}

// A cache takes a keep for a window its own list ranks it for, under the
// list's code, and writes every stripe it carries: here the two indices a list
// of two caches under 2+2 puts on it round the list.
func TestACacheKeepsWhatItsListRanksItFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 2, M: 2}
		f := newKeepFixture(t, 100, sim.DiskConfig{}, func(self CacheIdentity) rank.List {
			return listOf(code, self, otherCache)
		})
		self := f.cache.Identity()
		key := keyOf("vm", 0)
		var mine []int
		for index, holder := range f.list.Holders(key.rankWindow()) {
			if holder.Identity == self {
				mine = append(mine, index)
			}
		}
		if err := f.cache.Keep(t.Context(), f.m, f.cache.Identity(), keepOf(t, key, code, mine)); err != nil {
			t.Fatal(err)
		}
		if got := f.held(key, code); !slices.Equal(got, mine) || len(mine) != 2 {
			t.Fatalf("the cache holds %v of the window, want %v", got, mine)
		}
		if fill := f.cache.Stats().Fill; fill.Kept != 2 || fill.Refused != 0 || fill.Duplicates != 0 {
			t.Fatalf("the keep came to %+v, want two stripes kept", fill)
		}
	})
}

// A cache refuses a keep its own list does not rank it for: a window whose
// first k+m ranks are other caches, a keep under another code than its
// list's, a window outside the share the cluster cache is on for, and items
// that are not what the keep says they are. It writes none of them.
func TestACacheRefusesAKeepItsListDoesNotRankItFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Six caches of 2+1: three rank for each window, so this cache is
		// ranked for some windows and not for others.
		code := rank.Code{K: 2, M: 1}
		f := newKeepFixture(t, 50, sim.DiskConfig{}, func(self CacheIdentity) rank.List {
			return listOf(code, self, fiveOthers...)
		})
		unranked := f.windowWhere(t, "vm", false, true, 50)
		ranked := f.windowWhere(t, "vm", true, true, 50)
		outside := f.windowWhere(t, "vm", true, false, 50)
		damaged := keepOf(t, ranked, code, []int{0})
		damaged.Payload[len(damaged.Payload)-1] ^= 0x01
		misnamed := keepOf(t, ranked, code, []int{0})
		misnamed.Items[0].Index = 1
		for _, refused := range []struct {
			name string
			keep peer.Keep
		}{
			{"an unranked window", keepOf(t, unranked, code, []int{0, 1, 2})},
			{"another code", keepOf(t, ranked, rank.Code{K: 1, M: 1}, []int{0})},
			{"a window outside the share", keepOf(t, outside, code, []int{0})},
			{"a damaged item", damaged},
			{"a misnamed item", misnamed},
		} {
			before := f.cache.Stats().Fill.Refused
			if err := f.cache.Keep(t.Context(), f.m, f.cache.Identity(), refused.keep); !errors.Is(err, peer.ErrDropped) {
				t.Fatalf("a keep of %s = %v, want it dropped", refused.name, err)
			}
			if got := f.cache.Stats().Fill.Refused - before; got != uint64(len(refused.keep.Items)) {
				t.Fatalf("a keep of %s refused %d stripes, want %d", refused.name, got, len(refused.keep.Items))
			}
		}
		for _, key := range []diskKey{unranked, ranked, outside} {
			for _, code := range []rank.Code{code, {K: 1, M: 1}} {
				if held := f.held(key, code); len(held) != 0 {
					t.Fatalf("the cache holds %v of %+v under %s", held, key.rankWindow(), code)
				}
			}
		}
		if fill := f.cache.Stats().Fill; fill.Kept != 0 {
			t.Fatalf("the refused keeps came to %+v, want nothing kept", fill)
		}
	})
}
