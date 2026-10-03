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
// limiter's goals leave it on this filesystem, under Cache.DiskBytes where that
// is set. It is read once, when the host starts, and never from the share the
// limiter moves as the disk fills, because every change of a weight moves
// windows between hosts.
func cacheDisk(ctx context.Context, config Config) int64 {
	disk := config.Cache.DiskBytes
	if config.DiskLimiter == nil {
		return disk
	}
	capacity := config.DiskLimiter.Capacity()
	if sim.Bug(ctx, "host-weight-from-share") {
		capacity = config.DiskLimiter.CacheShare()
	}
	if disk > 0 {
		return min(disk, capacity)
	}
	return capacity
}

// Cache is this host's cache as the list of caches names it, and whether the
// host keeps one.
func (h *Host) Cache() (rank.Cache, bool) { return h.self, !h.self.Identity.IsZero() }

// Caches is the list of caches this host holds now, which it ranks windows by.
func (h *Host) Caches() rank.List { return h.caches.List() }

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
