package checkpoint_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/rank"
)

// otherHosts are caches the pull tests list beside the fixture's own.
var otherHosts = []rank.Identity{{0xee, 0x02}, {0xee, 0x03}, {0xee, 0x04}, {0xee, 0x05}, {0xee, 0x06}}

// clusterPull runs test inside a synctest bubble over a pull fixture whose
// cache turns the cluster cache on for percent of windows. A read of the
// cluster reads the store as well once its bound has passed, so its clock
// must be the simulation's: on the wall clock, how long the fixture's disk
// takes is up to the machine running it.
func clusterPull(t *testing.T, percent int, test func(t *testing.T, f *pullFixture)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		test(t, newPullFixtureWith(t, 64<<20, cachedPages, []uint64{0, 1, 2, 3},
			func(config *checkpoint.CacheConfig) { config.ClusterPercent = percent }))
	})
}

// follow has the fixture's cache keep and read stripes by a list of the
// caches named, each of weight one, under code, and returns it. withSelf puts
// the fixture's own cache in it.
func (f *pullFixture) follow(t *testing.T, code rank.Code, withSelf bool, others ...rank.Identity) rank.List {
	t.Helper()
	var caches []rank.Cache
	if withSelf {
		caches = append(caches, rank.Cache{Identity: f.cache.Stats().Disk.Identity, Weight: 1})
	}
	for _, other := range others {
		caches = append(caches, rank.Cache{Identity: other, Weight: 1})
	}
	list, err := rank.NewList(code, caches)
	if err != nil {
		t.Fatal(err)
	}
	f.cache.FollowList(list)
	return list
}

// pulled is what a pull and two reads of its checkpoint came to.
type pulled struct {
	stats checkpoint.PullStats
	// gets is the requests the reads made of the store, entries the stripes
	// the disk holds, and hits the envelopes it served.
	gets    int64
	entries int
	hits    uint64
}

// pullAndRead pulls the fixture's checkpoint, then reads it twice.
func (f *pullFixture) pullAndRead(t *testing.T) pulled {
	t.Helper()
	pull := f.pull(t)
	if err := pull.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A window inside the share is the cluster's fill, which the pull hands
	// over and does not wait for.
	if err := f.cache.SettleFills(t.Context()); err != nil {
		t.Fatal(err)
	}
	stats := pull.Stats()
	if !stats.Done || stats.Err != nil || stats.Bytes == 0 {
		t.Fatalf("the pull ended at %+v", stats)
	}
	f.objects.gets.Store(0)
	f.readAll(t)
	f.readAll(t)
	disk := f.cache.Stats().Disk
	if disk.Lost != 0 {
		t.Fatalf("the disk lost %d stripes", disk.Lost)
	}
	// A read of this host's own disk takes its simulated time, which is far
	// inside the bound past which a read of the cluster reads the store as
	// well. On the wall clock a loaded machine could take longer than the
	// bound, and the read then also asked the store.
	if hedges := f.cache.Stats().Read.StoreHedges; hedges != 0 {
		t.Fatalf("%d reads of the cluster read the store as well, want none", hedges)
	}
	return pulled{stats: stats, gets: f.objects.gets.Load(), entries: disk.Entries, hits: disk.Hits}
}

// heldBy is how many stripes of the fixture's windows list puts on the
// fixture's own cache: of each page's window and of the segment's.
func (f *pullFixture) heldBy(list rank.List) int {
	self := f.cache.Stats().Disk.Identity
	ref := f.index.Ref()
	windows := []rank.Window{rank.SegmentWindow(ref, "root", 0)}
	for _, page := range f.pages {
		windows = append(windows, rank.PageWindow(control.Identity{Ref: ref, Volume: "root", Page: page},
			checkpoint.PageSize2MiB))
	}
	held := 0
	for _, window := range windows {
		for _, holder := range list.Holders(window) {
			if holder.Identity == self {
				held++
			}
		}
	}
	return held
}

// With the cluster cache turned on for no window, the default, a host keeps
// a pulled checkpoint whole on its own disk, whatever its list of caches says,
// as it did before there were stripes. Under a list of six caches of 4+2 it
// keeps every page and the segment whole, and reads them back twice with no
// request of the store.
func TestAPulledCheckpointIsKeptWholeOutsideTheClusterShare(t *testing.T) {
	clusterPull(t, 0, func(t *testing.T, f *pullFixture) {
		f.follow(t, rank.Code{K: 4, M: 2}, true, otherHosts...)
		got := f.pullAndRead(t)
		if got.stats.Pulled != got.stats.Bytes || got.gets != 0 || got.entries != cachedPages+1 || got.hits != 9 {
			t.Fatalf("the pull came to %+v; want every envelope kept whole and read with no request", got)
		}
	})
}

// With the cluster cache on for every window, a host in a list of six caches
// of 4+2 keeps the one stripe of each window the list puts on it, and no
// more. One stripe of four rebuilds nothing, so every read goes to the store
// until hosts read from each other.
func TestAPulledCheckpointIsPlacedByTheListInsideTheClusterShare(t *testing.T) {
	clusterPull(t, 100, func(t *testing.T, f *pullFixture) {
		list := f.follow(t, rank.Code{K: 4, M: 2}, true, otherHosts...)
		got := f.pullAndRead(t)
		if held := f.heldBy(list); got.gets != 9 || got.entries != held || got.hits != 0 || held == 0 {
			t.Fatalf("the pull came to %+v; want the %d stripes the list puts here and every read of the store",
				got, held)
		}
	})
}

// A cache alone in a list of 4+2 keeps all six stripes of each envelope a
// pull copies, the list going round it, and a read rebuilds each page from
// them with no request of the store: the four pages and the segment are 30
// stripes, and the reads are the nine hits a whole copy serves.
func TestAPulledCheckpointIsRebuiltFromItsStripes(t *testing.T) {
	clusterPull(t, 100, func(t *testing.T, f *pullFixture) {
		f.follow(t, rank.Code{K: 4, M: 2}, true)
		got := f.pullAndRead(t)
		if got.stats.Pulled != got.stats.Bytes || got.gets != 0 || got.entries != 6*(cachedPages+1) || got.hits != 9 {
			t.Fatalf("the pull came to %+v; want every page rebuilt from 30 stripes with no request", got)
		}
	})
}

// Two hosts under 1+1 each keep one whole copy of every window, so a pulled
// checkpoint is read from this host's own disk alone.
func TestOnTwoHostsAPulledCheckpointIsReadFromThisHostsCopy(t *testing.T) {
	clusterPull(t, 100, func(t *testing.T, f *pullFixture) {
		f.follow(t, rank.Code{K: 1, M: 1}, true, otherHosts[0])
		got := f.pullAndRead(t)
		if got.stats.Pulled != got.stats.Bytes || got.gets != 0 || got.entries != cachedPages+1 || got.hits != 9 {
			t.Fatalf("the pull came to %+v; want one copy of each window and no request", got)
		}
	})
}

// A host whose list does not rank its cache for a window keeps nothing of it:
// the pull hands the whole checkpoint to the cluster's fills, which keep
// nothing here, and every read goes to the store.
func TestAPullKeepsNothingAHostIsNotRankedFor(t *testing.T) {
	clusterPull(t, 100, func(t *testing.T, f *pullFixture) {
		f.follow(t, rank.Code{K: 1, M: 1}, false, otherHosts[0])
		got := f.pullAndRead(t)
		if got.stats.Pulled != got.stats.Bytes || got.gets != 9 || got.entries != 0 || got.hits != 0 {
			t.Fatalf("the pull came to %+v; want it all handed over, nothing kept and every read of the store", got)
		}
	})
}
