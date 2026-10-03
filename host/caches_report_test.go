package host

import (
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/rank"
)

// /status reports this host's cache and the list it holds: the code, the
// caches, when it was last read, and why a read failed. A host that keeps no
// cache reports none, and one that has read no list reports no time.
func TestStatusReportsTheCacheAndTheListHeld(t *testing.T) {
	self := rank.Cache{Identity: rank.Identity{0xab, 1}, Weight: 4, Address: "10.0.0.1:8081"}
	other := rank.Cache{Identity: rank.Identity{0x0c}, Weight: 1, Address: "10.0.0.2:8081"}
	list, err := rank.NewList(rank.Code{K: 1, M: 1}, []rank.Cache{self, other})
	if err != nil {
		t.Fatal(err)
	}
	read := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
	cache, held := cacheReport(self, rank.FollowerStatus{List: list, Read: read, Reads: 4, Failures: 2,
		Error: "connection refused"})
	wantCache := hostapi.Cache{Identity: "ab010000000000000000000000000000", Weight: 4, Address: "10.0.0.1:8081"}
	if cache == nil || *cache != wantCache {
		t.Fatalf("the cache is reported as %+v, want %+v", cache, wantCache)
	}
	want := hostapi.Caches{K: 1, M: 1, Caches: []hostapi.Cache{
		{Identity: "0c000000000000000000000000000000", Weight: 1, Address: "10.0.0.2:8081"}, wantCache}}
	if held.K != want.K || held.M != want.M || len(held.Caches.Caches) != 2 ||
		held.Caches.Caches[0] != want.Caches[0] || held.Caches.Caches[1] != want.Caches[1] ||
		held.Read == nil || !held.Read.Equal(read) || held.Reads != 4 || held.Failures != 2 ||
		held.Error != "connection refused" {
		t.Fatalf("the list is reported as %+v, want %+v read at %s", held, want, read)
	}
	cache, held = cacheReport(rank.Cache{}, rank.FollowerStatus{List: rank.Alone(rank.Cache{})})
	if cache != nil || held.Read != nil || held.K != 1 || held.M != 0 || held.Caches.Caches == nil ||
		len(held.Caches.Caches) != 0 {
		t.Fatalf("a host with no cache that read no list reports %+v and %+v", cache, held)
	}
}
