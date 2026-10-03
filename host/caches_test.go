package host_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// cachingHost is one host with a page cache disk, in a synctest bubble, on a
// simulated clock: what the list of caches needs of a host and nothing more.
type cachingHost struct {
	ctx     context.Context
	runtime *sim.Runtime
	clock   *sim.Clock
	disk    *sim.Disk
	config  host.Config
}

func newCachingHost(t *testing.T, space sim.SpaceConfig) *cachingHost {
	t.Helper()
	runtime := sim.New(sim.Config{})
	h := &cachingHost{runtime: runtime, clock: runtime.NewClock("host"),
		disk: runtime.NewDisk("node", sim.DiskConfig{Space: space})}
	h.ctx = sim.WithRuntime(t.Context(), runtime)
	file, err := h.disk.Open(h.ctx, "cache", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	h.config = host.Config{Network: runtime.Network(), Resources: testresource.New(),
		ObjectStore: runtime.ObjectStore(), Clock: h.clock, Entropy: runtime.NewEntropy("host"),
		Cache:              checkpoint.CacheConfig{Disk: file, DiskRegionBytes: 8 << 20},
		Migration:          host.MigrationConfig{Address: "host-0-pages"},
		CheckpointInterval: -1, EpochInterval: -1}
	return h
}

func (h *cachingHost) start(t *testing.T) *host.Host {
	t.Helper()
	started, err := host.StartHost(h.ctx, h.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := started.Close(context.WithoutCancel(h.ctx)); err != nil {
			t.Error(err)
		}
	})
	return started
}

// A host with a cache disk reports the cache's identity, the weight of the
// disk it is given and its peer-server address. Until it reads the list of
// caches it holds its own cache alone, under the code of one host, so it
// ranks first for every window and each window's one stripe is its envelope
// whole: what every host does today.
func TestAHostReportsItsCacheAndHoldsItAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 64 << 30})
		h.config.Cache.DiskBytes = 40 << 30
		started := h.start(t)
		status := started.Status()
		self, kept := started.Cache()
		want := rank.Cache{Identity: status.Cache.Disk.Identity, Weight: 3, Address: "host-0-pages"}
		if !kept || self != want || status.Self != want || self.Identity.IsZero() {
			t.Fatalf("the host reports its cache as %+v (%v), want %+v", self, kept, want)
		}
		if got := started.Caches(); !got.Equal(rank.Alone(want)) || !status.Caches.List.Equal(rank.Alone(want)) {
			t.Fatalf("a host that read no list holds %v under %s", got.Caches(), got.Code())
		}
		ref := control.Ref{VM: "vm-alone", Sequence: 1}
		for page := range uint64(2048) {
			window := rank.PageWindow(control.Identity{Ref: ref, Volume: "ram0", Page: page}, 4096)
			if holders := started.Caches().Holders(window); len(holders) != 1 || holders[0] != want {
				t.Fatalf("page %d's window goes on %v, want this host's cache whole", page, holders)
			}
		}
	})
}

// A host whose cache is given no disk is in no list: it has no weight, and
// so no cache to report.
func TestAHostWithNoCacheDiskIsInNoList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 64 << 30})
		started := h.start(t)
		if self, kept := started.Cache(); kept || self != (rank.Cache{}) {
			t.Fatalf("a host with no disk reports cache %+v", self)
		}
		if got := started.Caches(); got.Len() != 0 {
			t.Fatalf("a host with no disk holds %v", got.Caches())
		}
	})
}

// A host reads the list of caches as it starts and then on its own timer, and
// keeps the last list it read while the orchestrator does not answer.
func TestAHostHoldsTheListItLastRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 64 << 30})
		h.config.Cache.DiskBytes = 16 << 30
		var served rank.List
		down := false
		h.config.CacheList = host.CacheListConfig{Read: func(context.Context) (rank.List, error) {
			if down {
				return rank.List{}, errors.New("connection refused")
			}
			return served, nil
		}}
		other := rank.Cache{Identity: rank.Identity{9}, Weight: 1, Address: "host-1-pages"}
		served = mustList(t, rank.Code{K: 1, M: 1}, other)
		started := h.start(t)
		synctest.Wait()
		self, _ := started.Cache()
		// The orchestrator had not heard of this host yet, so its list holds
		// the other host alone.
		if got := started.Caches(); !got.Equal(served) {
			t.Fatalf("a started host holds %v under %s, want the orchestrator's list", got.Caches(), got.Code())
		}
		both := mustList(t, rank.Code{K: 1, M: 1}, self, other)
		served = both
		h.clock.Advance(rank.DefaultInterval)
		synctest.Wait()
		if got := started.Caches(); !got.Equal(both) {
			t.Fatalf("after an interval the host holds %v, want both caches", got.Caches())
		}
		down = true
		h.clock.Advance(rank.DefaultInterval)
		synctest.Wait()
		status := started.Status().Caches
		if !status.List.Equal(both) || status.Reads != 2 || status.Failures != 1 || status.Error != "connection refused" {
			t.Fatalf("with the orchestrator down the host holds %v after %d reads and %d failures (%q)",
				status.List.Caches(), status.Reads, status.Failures, status.Error)
		}
	})
}

// A cache's weight is the disk it is given, as the limiter's goals leave it
// on the filesystem, and never the share the limiter moves as other writers
// fill the disk: every change of a weight moves windows. Here other writers
// hold most of a 200 GiB disk, so the share is a few GiB, while the goal of a
// tenth kept free gives the cache 180 GiB: a weight of eleven.
func TestACachesWeightIsItsDiskNotItsShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 200 << 30, OutsideBytes: 170 << 30})
		limiter, err := resource.NewDiskLimiter(h.ctx, resource.DiskLimiterConfig{Space: h.disk,
			Goal: resource.DiskGoal{FreePercent: 10}, Clock: h.clock})
		if err != nil {
			t.Fatal(err)
		}
		defer limiter.Close()
		h.config.DiskLimiter = limiter
		started := h.start(t)
		// The room is the 30 GiB free, less the floor of 20, less a band of a
		// fifth of the 10 left.
		if share := limiter.CacheShare(); share != 8<<30 {
			t.Fatalf("the share is %d bytes, want 8 GiB", share)
		}
		if self, _ := started.Cache(); self.Weight != 11 {
			t.Fatalf("the cache weighs %d, want 11 for 180 GiB", self.Weight)
		}
	})
}

func mustList(t *testing.T, code rank.Code, caches ...rank.Cache) rank.List {
	t.Helper()
	list, err := rank.NewList(code, caches)
	if err != nil {
		t.Fatal(err)
	}
	return list
}
