package checkpoint_test

import (
	"bytes"
	"math/rand/v2"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// bandwidthRuntime is a simulation whose network carries 32 MiB a second in
// each frame, after a millisecond's latency and no jitter: a stripe of a
// 2 MiB page takes 16 ms to arrive and one of a 4 KiB page next to nothing,
// so a read of the cluster takes about as long as the bytes it asks for.
var bandwidthRuntime = sim.Config{Seed: 1,
	Network: sim.NetworkConfig{Latency: time.Millisecond, ConnectLatency: time.Millisecond, BytesPerSecond: 32 << 20},
	ObjectStore: sim.ObjectStoreConfig{GetLatency: 5 * time.Millisecond, HeadLatency: time.Millisecond,
		PutLatency: 5 * time.Millisecond, BytesPerSecond: 1 << 40}}

// publishSizes publishes from store a checkpoint of vm with a volume "large"
// of four 2 MiB pages of noise, which no encoder shrinks, and a volume "small"
// of sixteen 4 KiB pages, and returns it and the model of what it holds.
func publishSizes(t *testing.T, store *checkpoint.Store, vm string) (*checkpoint.Index, *model) {
	t.Helper()
	specs := map[string]checkpoint.VolumeSpec{
		"large": {Size: 4 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB},
		"small": {Size: 16 * checkpoint.PageSize4KiB, PageSize: checkpoint.PageSize4KiB}}
	root, err := store.Root(t.Context(), control.Ref{VM: vm, Sequence: 1}, specs)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(specs)
	p := store.Begin(root, control.Ref{VM: vm, Sequence: 2})
	noise := rand.NewChaCha8([32]byte{2})
	for page := range uint64(4) {
		for sector := range uint32(sectorsPerPage) {
			data := make([]byte, checkpoint.SectorSize)
			_, _ = noise.Read(data)
			m.dirty(p, "large", page, sector, data)
		}
	}
	for page := range uint64(16) {
		m.dirty(p, "small", page, 0, sectorData(vm, page, 0))
	}
	published, err := p.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	return published, m
}

// readPage has store read page of volume, and checks it against m.
func readPage(t *testing.T, store *checkpoint.Store, index *checkpoint.Index, m *model, volume string, page uint64) {
	t.Helper()
	size := m.pageSize[volume]
	got := make([]byte, size)
	if err := store.Read(t.Context(), index, volume, page*size, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, m.contents[volume][page*size:(page+1)*size]) {
		t.Fatalf("page %d of %s differs from the model", page, volume)
	}
}

// A read of a size the reader has seen take long is not hedged to the store
// for taking that long after a burst of small fast reads, while one that is
// truly slow still is. Under 4+2, on a network where a read takes about as
// long as its bytes, a reader reads 2 MiB pages, about 30 ms each, until
// their class has its delay, then 512 pages of 4 KiB, about 2 ms each. One
// delay for every size would then be a few milliseconds and its bound four
// of them, which a 2 MiB read passes: four more 2 MiB reads would each read
// the store too. Each class keeps its own, so they read no part of the
// store. With every link to the holders then a tenth of a second slower, a
// 2 MiB read passes its own class's bound, reads the store once, and takes
// the store's answer.
func TestALargeReadAfterManySmallFastReadsIsNotHedgedToTheStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100,
			runtime: bandwidthRuntime, cache: func(_ int, cache *checkpoint.CacheConfig) {
				cache.ClusterHedgeFloor = 100 * time.Microsecond
				cache.ClusterBound = time.Millisecond
				cache.ClusterStripeTimeout = time.Hour
			}})
		published, m := publishSizes(t, c.hosts[1].store, "vm")
		c.settle(t)
		reader := c.hosts[0]
		index, err := reader.store.Open(c.ctx(t), published.Ref())
		if err != nil {
			t.Fatal(err)
		}
		// The first reads of 2 MiB pass the floor's bound and the store
		// answers them, which teaches their class nothing, until the bucket
		// runs out: 64 of them leave the class more than the 32 reads it
		// needs.
		for at := range 64 {
			readPage(t, reader.store, index, m, "large", uint64(at%4))
		}
		for at := range 512 {
			readPage(t, reader.store, index, m, "small", uint64(at%16))
		}
		before := reader.cache.Stats().Read
		gets := reader.partGets()
		for at := range 4 {
			readPage(t, reader.store, index, m, "large", uint64(at))
		}
		after := reader.cache.Stats().Read
		large := classOf(after, checkpoint.PageSize2MiB)
		if hedges := after.StoreHedges - before.StoreHedges; hedges != 0 || gets.Load() != 0 {
			t.Fatalf("after 512 reads of 4 KiB, four reads of 2 MiB read the store %d times and %d parts of it, "+
				"want none; the reader's classes: %+v", hedges, gets.Load(), after.Classes)
		}
		for _, h := range c.hosts[1:] {
			c.runtime.Network().SetLink(platform.Address(reader.name), h.address, simLink(100*time.Millisecond))
			c.runtime.Network().SetLink(h.address, platform.Address(reader.name), simLink(100*time.Millisecond))
		}
		readPage(t, reader.store, index, m, "large", 0)
		slow := reader.cache.Stats().Read
		if hedges, won := slow.StoreHedges-after.StoreHedges, slow.StoreHedgesWon-after.StoreHedgesWon; hedges != 1 ||
			won != 1 || gets.Load() != 1 {
			t.Fatalf("a 2 MiB read past its bound of %v read the store %d times, won %d, and read %d parts, "+
				"want once, won by the store", large.Bound, hedges, won, gets.Load())
		}
	})
}
