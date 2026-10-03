package host

import (
	"context"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// CacheListConfig is where a host reads the list of caches, and how often.
type CacheListConfig struct {
	// Read reads the list, which the orchestrator serves. Nil never reads, and
	// the host holds its own cache alone: it ranks first for every window.
	Read func(ctx context.Context) (rank.List, error)
	// Interval is how often the list is read. Zero is rank.DefaultInterval.
	Interval time.Duration
}

// cacheOf is this host's cache as the list of caches names it: the identity
// in its file's header, the weight of the disk it is given, and the address
// the host serves pages at. A host that keeps no cache disk, or gives it no
// space, has none, and the zero cache says so.
func cacheOf(ctx context.Context, config Config, identity rank.Identity) rank.Cache {
	weight := rank.Weight(cacheDisk(ctx, config))
	if identity.IsZero() || weight == 0 {
		return rank.Cache{}
	}
	return rank.Cache{Identity: identity, Weight: weight, Address: config.Migration.Address}
}

// cacheDisk is the size of the disk this host's cache is given: what the disk
// limiter's goals leave it on this filesystem, or Cache.DiskBytes for a host
// with no limiter. It is read once, when the host starts, and never from the
// share the limiter moves as the disk fills, because every change of a weight
// moves windows between hosts.
func cacheDisk(ctx context.Context, config Config) int64 {
	if config.DiskLimiter == nil {
		return config.Cache.DiskBytes
	}
	if sim.Bug(ctx, "host-weight-from-share") {
		return config.DiskLimiter.CacheShare()
	}
	return config.DiskLimiter.Capacity()
}

// Cache is this host's cache as the list of caches names it, and whether the
// host keeps one.
func (h *Host) Cache() (rank.Cache, bool) { return h.self, !h.self.Identity.IsZero() }

// Caches is the list of caches this host holds now, which it ranks windows by.
func (h *Host) Caches() rank.List { return h.caches.List() }

// RefreshCaches reads the list of caches now rather than at the next turn of
// its timer: what a host that has just learned of a cache, or a test that
// starts from the list a host read, does.
func (h *Host) RefreshCaches(ctx context.Context) error { return h.caches.Refresh(ctx) }

// SettleFills returns once every fill of the cluster's cache this host's
// cache was handed has been written or dropped, and every keep and fill right
// it asked a peer for has been answered, and every fill of its hot tier has
// been done or dropped. Nothing waits on a fill; a test that says what the
// cluster's disks or the hot tier hold waits on this.
func (h *Host) SettleFills(ctx context.Context) error {
	if err := h.cache.SettleFills(ctx); err != nil {
		return err
	}
	if h.hot != nil {
		return h.hot.Settle(ctx)
	}
	return nil
}

// cacheReport is this host's cache and the list of caches it holds, as
// /status reports them.
func cacheReport(self rank.Cache, caches rank.FollowerStatus) (*hostapi.Cache, hostapi.CacheList) {
	var cache *hostapi.Cache
	if !self.Identity.IsZero() {
		reported := hostapi.CacheOf(self)
		cache = &reported
	}
	list := hostapi.CacheList{Caches: hostapi.CachesOf(caches.List), Reads: caches.Reads,
		Failures: caches.Failures, Error: caches.Error}
	if !caches.Read.IsZero() {
		read := caches.Read
		list.Read = &read
	}
	return cache, list
}
