package checkpoint_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// fillHost is one host of a simulated cluster: a page cache on a disk of its
// own, whose memory tier no page fits in, the store it reads through, the peer
// server that answers its peers from its cache, and its table of peers.
type fillHost struct {
	name    string
	address platform.Address
	cache   *checkpoint.Cache
	store   *checkpoint.Store
	objects *cacheStore
	clock   *sim.Clock
	table   *peer.Table
	server  *peer.Server
	file    platform.File
	// list is the list of caches this host holds.
	list atomic.Pointer[rank.List]
	// up says the host's server, table and cache are open.
	up bool
}

// fillCluster is hosts that follow one list of caches over one simulated
// network and object store, and a publisher that keeps no cache.
type fillCluster struct {
	runtime *sim.Runtime
	config  fillConfig
	hosts   []*fillHost
	// list is the list the orchestrator serves, which each host holds once
	// it has read it.
	list      atomic.Pointer[rank.List]
	publisher *checkpoint.Store
	puts      *heldPuts
	closed    sync.Once
}

// fillConfig is one cluster: its hosts, its code, the share the cluster cache
// is on for, and what each host's cache is given beside.
type fillConfig struct {
	hosts int
	code  rank.Code
	share int
	// cache has a last say over each host's cache, table over its table of
	// peers and server over its peer server.
	cache  func(host int, config *checkpoint.CacheConfig)
	table  func(config *peer.TableConfig)
	server func(host int, config *peer.ServerConfig)
	// runtime is the simulation the cluster runs in, and disk each host's
	// disk.
	runtime sim.Config
	disk    sim.DiskConfig
}

// newFillCluster starts the hosts of config and the list they follow, and
// closes them with the test.
func newFillCluster(t *testing.T, config fillConfig) *fillCluster {
	t.Helper()
	c := &fillCluster{runtime: sim.New(config.runtime), config: config}
	t.Cleanup(c.close)
	ctx := c.ctx(t)
	c.puts = &heldPuts{ObjectStore: c.runtime.ObjectStore()}
	c.publisher = mustStore(t, checkpoint.Config{ObjectStore: c.puts})
	var caches []rank.Cache
	for index := range config.hosts {
		name := fmt.Sprintf("host-%d", index)
		h := &fillHost{name: name, address: platform.Address(name + "/pages"), clock: c.runtime.NewClock(name),
			objects: &cacheStore{ObjectStore: c.puts}}
		file, err := c.runtime.NewDisk(name, config.disk).Open(ctx, "cache", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		h.file = file
		c.open(t, index, h)
		c.hosts = append(c.hosts, h)
		caches = append(caches, rank.Cache{Identity: h.cache.Identity(), Weight: 1, Address: h.address})
	}
	list, err := rank.NewList(config.code, caches)
	if err != nil {
		t.Fatal(err)
	}
	c.hold(list)
	return c
}

// open opens a host's table of peers, its cache over its file, its peer
// server and its store, and has its cache follow the list the host holds.
func (c *fillCluster) open(t *testing.T, index int, h *fillHost) {
	t.Helper()
	ctx := c.ctx(t)
	tableConfig := peer.TableConfig{Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		return c.runtime.Network().Dial(ctx, platform.Address(h.name), to)
	}}
	if c.config.table != nil {
		c.config.table(&tableConfig)
	}
	var err error
	h.table, err = peer.NewTable(ctx, tableConfig)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := resource.New(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	cacheConfig := checkpoint.CacheConfig{Disk: h.file, DiskBytes: 256 << 20, DiskRegionBytes: pullRegionBytes,
		ClusterPercent: c.config.share, Peers: h.table, Clock: h.clock, Entropy: c.runtime.NewEntropy(h.name)}
	if c.config.cache != nil {
		c.config.cache(index, &cacheConfig)
	}
	h.cache, err = checkpoint.NewCache(ctx, budget, cacheConfig)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := peer.ServerConfig{Network: c.runtime.Network(), Address: h.address,
		PageSize: checkpoint.PageSize2MiB, Cache: h.cache}
	if c.config.server != nil {
		c.config.server(index, &serverConfig)
	}
	h.server, err = peer.NewServer(ctx, serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	h.store = mustStore(t, checkpoint.Config{ObjectStore: h.objects, Cache: h.cache})
	h.cache.FollowCaches(func() rank.List { return *h.list.Load() })
	h.up = true
}

// shut closes a host as a host closes: its peer server, then its table of
// peers, then its cache. Its file stays, as a host's cache file outlives it.
func (h *fillHost) shut() {
	if !h.up {
		return
	}
	h.up = false
	_ = h.server.Close()
	_ = h.table.Close()
	h.cache.Close()
}

// restart shuts a host and opens it again over the same file, at the same
// address, under the list it held: a host process that restarted.
func (c *fillCluster) restart(t *testing.T, index int) {
	t.Helper()
	h := c.hosts[index]
	h.shut()
	c.open(t, index, h)
}

// hold has the orchestrator serve list, and the hosts named, or every host
// when none is, read it. The rest hold the list they held, as hosts that have
// not read it yet do.
func (c *fillCluster) hold(list rank.List, hosts ...*fillHost) {
	c.list.Store(&list)
	if len(hosts) == 0 {
		hosts = c.hosts
	}
	for _, h := range hosts {
		h.list.Store(&list)
	}
}

// close closes every host as a host closes, and then the cache files. It is
// safe to call again.
func (c *fillCluster) close() {
	c.closed.Do(func() {
		for _, h := range c.hosts {
			h.shut()
		}
		for _, h := range c.hosts {
			_ = h.file.Close()
		}
	})
}

// ctx is the test's context carrying the cluster's runtime, which the sites
// and the guards consult.
func (c *fillCluster) ctx(t *testing.T) context.Context {
	return sim.WithRuntime(t.Context(), c.runtime)
}

// heldPuts is the object store with the PUTs of parts held while hold is set,
// each telling entered as it begins and waiting for release, and failed
// instead while fail is set.
type heldPuts struct {
	platform.ObjectStore
	hold, fail atomic.Bool
	entered    chan struct{}
	release    chan struct{}
}

func (s *heldPuts) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if strings.Contains(request.Key.String(), "/part/") {
		if s.fail.Load() {
			return platform.PutResult{}, platform.ErrInjectedFault
		}
		if s.hold.Load() {
			s.entered <- struct{}{}
			select {
			case <-s.release:
			case <-ctx.Done():
				return platform.PutResult{}, context.Cause(ctx)
			}
		}
	}
	return s.ObjectStore.Put(ctx, request)
}

// publish publishes pages of vm's one volume, root, from store, and returns
// the checkpoint and the model of what it holds.
func publish(t *testing.T, store *checkpoint.Store, vm string, pages []uint64) (*checkpoint.Index, *model) {
	t.Helper()
	sizes := map[string]uint64{"root": (slices.Max(pages) + 1) * checkpoint.PageSize2MiB}
	root, err := store.Root(t.Context(), control.Ref{VM: vm, Sequence: 1}, volumes2MiB(sizes))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(volumes2MiB(sizes))
	p := store.Begin(root, control.Ref{VM: vm, Sequence: 2})
	for _, page := range pages {
		for sector := range uint32(sectorsPerPage) {
			m.dirty(p, "root", page, sector, sectorData(vm, page, sector))
		}
	}
	published, err := p.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	return published, m
}

// read has host read page of a published checkpoint from its own store, and
// checks it against the model.
func (c *fillCluster) read(t *testing.T, h *fillHost, ref control.Ref, m *model, page uint64) {
	t.Helper()
	index, err := h.store.Open(c.ctx(t), ref)
	if err != nil {
		t.Fatal(err)
	}
	readCachedPage(t, h.store, index, m, page)
}

// settle waits until every host's fills have been written or dropped.
func (c *fillCluster) settle(t *testing.T) {
	t.Helper()
	for _, h := range c.hosts {
		if err := h.cache.SettleFills(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

// pageWindow and segmentWindow are the windows of a page of the published
// volume and of its one segment.
func pageWindow(ref control.Ref, page uint64) rank.Window {
	return rank.PageWindow(control.Identity{Ref: ref, Volume: "root", Page: page}, checkpoint.PageSize2MiB)
}

func segmentWindow(ref control.Ref) rank.Window { return rank.SegmentWindow(ref, "root", 0) }

// placed is, by host, the indices of window's stripes each host holds.
func (c *fillCluster) placed(window rank.Window) [][]int {
	code := c.list.Load().Code()
	held := make([][]int, len(c.hosts))
	for at, h := range c.hosts {
		held[at] = h.cache.HeldIndices(window, 0, code)
	}
	return held
}

// ranked is, by host, the indices of window's stripes the list puts on each.
func (c *fillCluster) ranked(window rank.Window) [][]int {
	held := make([][]int, len(c.hosts))
	for index, holder := range c.list.Load().Holders(window) {
		for at, h := range c.hosts {
			if holder.Identity == h.cache.Identity() {
				held[at] = append(held[at], index)
			}
		}
	}
	return held
}

// fills is the sum of every host's fill stats.
func (c *fillCluster) fills() checkpoint.FillStats {
	var sum checkpoint.FillStats
	for _, h := range c.hosts {
		stats := h.cache.Stats().Fill
		sum.FromReads += stats.FromReads
		sum.FromPublications += stats.FromPublications
		sum.WithoutRight += stats.WithoutRight
		sum.RightsGranted += stats.RightsGranted
		sum.Sent += stats.Sent
		sum.Kept += stats.Kept
		sum.Duplicates += stats.Duplicates
		sum.Refused += stats.Refused
		for reason, dropped := range stats.Dropped {
			sum.Dropped[reason] += dropped
		}
	}
	return sum
}

// dropped is every stripe dropped, for any reason.
func dropped(stats checkpoint.FillStats) uint64 {
	total := uint64(0)
	for _, stripes := range stats.Dropped {
		total += stripes
	}
	return total
}

// clusterCodes are the codes of the table the placement tests run under, with
// as many hosts as each is for: whole copies on two, stripes round a list
// of three under 2+2, and 4+2 on six.
var clusterCodes = []struct {
	hosts int
	code  rank.Code
}{{2, rank.Code{K: 1, M: 1}}, {3, rank.Code{K: 2, M: 2}}, {6, rank.Code{K: 4, M: 2}}}

// After one host reads a page from the store, the stripes of the page's window
// and of the segment that located it are on exactly the hosts the list ranks
// for each, every index on the host it puts it on and on no other, whichever
// host read it. Nothing is dropped and nothing is a duplicate.
func TestAStoreReadFillsExactlyTheRankedCaches(t *testing.T) {
	for _, cluster := range clusterCodes {
		for reader := range cluster.hosts {
			t.Run(fmt.Sprintf("%s/reader-%d", cluster.code, reader), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := newFillCluster(t, fillConfig{hosts: cluster.hosts, code: cluster.code, share: 100})
					index, m := publish(t, c.publisher, "vm", []uint64{0, 1})
					ref := index.Ref()
					c.read(t, c.hosts[reader], ref, m, 1)
					c.settle(t)
					for _, window := range []rank.Window{pageWindow(ref, 1), segmentWindow(ref)} {
						if got, want := c.placed(window), c.ranked(window); !slices.EqualFunc(got, want, slices.Equal) {
							t.Fatalf("the stripes of %+v are on %v, want %v", window, got, want)
						}
					}
					if got := c.placed(pageWindow(ref, 0)); !slices.EqualFunc(got, make([][]int, cluster.hosts), slices.Equal) {
						t.Fatalf("page 0, which nobody read, is on %v", got)
					}
					width := uint64(cluster.code.Width())
					fills := c.fills()
					if fills.FromReads != 2 || fills.RightsGranted != 2 || fills.Kept != 2*width ||
						dropped(fills) != 0 || fills.Duplicates != 0 || fills.Refused != 0 {
						t.Fatalf("the cluster's fills came to %+v, want two windows of %d stripes each kept", fills, width)
					}
				})
			})
		}
	}
}

// A cold burst: every host of six reads the same page from the store at once,
// each through its own cache, so each misses. Only the first reader to ask the
// window's rank 1 for the fill right fills it, and every other sends nothing:
// one fill of the page's window and one of its segment's, not six of each.
func TestAColdBurstFillsAWindowOnce(t *testing.T) {
	for _, cluster := range clusterCodes {
		t.Run(cluster.code.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newFillCluster(t, fillConfig{hosts: cluster.hosts, code: cluster.code, share: 100})
				index, m := publish(t, c.publisher, "vm", []uint64{0})
				ref := index.Ref()
				var readers sync.WaitGroup
				for _, h := range c.hosts {
					readers.Go(func() { c.read(t, h, ref, m, 0) })
				}
				readers.Wait()
				c.settle(t)
				for _, h := range c.hosts {
					if gets := h.objects.gets.Load(); gets != 3 {
						t.Fatalf("%s made %d requests of the store, want the index, the segment and the page", h.name, gets)
					}
				}
				hosts := uint64(cluster.hosts)
				width := uint64(cluster.code.Width())
				fills := c.fills()
				if fills.FromReads != 2 || fills.RightsGranted != 2 || fills.WithoutRight != 2*(hosts-1) ||
					fills.Kept != 2*width || fills.Duplicates != 0 || dropped(fills) != 0 {
					t.Fatalf("a burst of %d readers came to %+v, want two windows filled once each", hosts, fills)
				}
				for _, window := range []rank.Window{pageWindow(ref, 0), segmentWindow(ref)} {
					if got, want := c.placed(window), c.ranked(window); !slices.EqualFunc(got, want, slices.Equal) {
						t.Fatalf("the stripes of %+v are on %v, want %v", window, got, want)
					}
				}
			})
		})
	}
}
