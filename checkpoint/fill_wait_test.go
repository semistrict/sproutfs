package checkpoint_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// latencyRuntime is a simulation whose object store and network take times a
// read's latency can be told apart by: no jitter, so two runs of one read take
// exactly as long.
var latencyRuntime = sim.Config{Seed: 1,
	Network: sim.NetworkConfig{Latency: time.Millisecond, ConnectLatency: time.Millisecond},
	ObjectStore: sim.ObjectStoreConfig{GetLatency: 5 * time.Millisecond, HeadLatency: time.Millisecond,
		PutLatency: 5 * time.Millisecond, BytesPerSecond: 1 << 40}}

// readLatency is how long the first host of a cluster of config takes to read
// page 0 of a published checkpoint it has opened, with before done to the
// cluster first, what the cluster's fills came to once they settled, and the
// indices of the page's window the list puts on each host.
func readLatency(t *testing.T, config fillConfig, before func(c *fillCluster)) (time.Duration, checkpoint.FillStats,
	[][]int) {
	t.Helper()
	var took time.Duration
	var fills checkpoint.FillStats
	var ranked [][]int
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, config)
		index, m := publish(t, c.publisher, "vm", []uint64{0})
		reader := c.hosts[0]
		opened, err := reader.store.Open(c.ctx(t), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if before != nil {
			before(c)
		}
		start := time.Now()
		readCachedPage(t, reader.store, opened, m, 0)
		took = time.Since(start)
		c.settle(t)
		fills, ranked = c.fills(), c.ranked(pageWindow(index.Ref(), 0))
	})
	return took, fills, ranked
}

// Nothing waits on a fill. A fault reads a page from the store in exactly the
// time it takes with the cluster cache off, whether the fill behind it is slow
// — every link from the reader to its peers held for a second, so the fill
// right and the keeps wait for it — or dropped, the reader's rate of keeps
// spent.
func TestAFaultIsNotSlowedByItsFill(t *testing.T) {
	config := fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, runtime: latencyRuntime}
	off, offFills, _ := readLatency(t, config, nil)
	if offFills.FromReads != 0 || offFills.WithoutRight != 0 || offFills.Kept != 0 {
		t.Fatalf("with the cluster cache off the fills came to %+v, want none", offFills)
	}
	config.share = 100
	held, heldFills, _ := readLatency(t, config, func(c *fillCluster) {
		reader := platform.Address(c.hosts[0].name)
		for _, h := range c.hosts[1:] {
			c.runtime.Network().HoldBoth(reader, h.address, time.Now().Add(time.Second))
		}
	})
	if heldFills.FromReads != 2 || heldFills.Kept != 12 || dropped(heldFills) != 0 {
		t.Fatalf("behind held links the fills came to %+v, want both windows filled once the links let go", heldFills)
	}
	config.cache = func(host int, cache *checkpoint.CacheConfig) {
		if host == 0 {
			cache.FillBytesPerSecond = 1
		}
	}
	spent, spentFills, ranked := readLatency(t, config, nil)
	// The reader keeps on its own disk what the list puts on it; every other
	// stripe of the page's and the segment's windows is dropped for the rate.
	if rate := spentFills.Dropped[checkpoint.DropRate]; spentFills.FromReads != 2 || rate == 0 ||
		rate+spentFills.Kept != 12 || dropped(spentFills) != rate || spentFills.Kept < uint64(len(ranked[0])) {
		t.Fatalf("with its rate spent the fills came to %+v, want every stripe for a peer dropped for the rate",
			spentFills)
	}
	if held != off || spent != off {
		t.Fatalf("a fault took %v with the cluster cache off, %v behind a slow fill and %v behind a dropped one; want the same",
			off, held, spent)
	}
}
