package host

import (
	"context"
	"slices"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// memberOf is this host as the membership names it: its identity, which is
// the identity in its cache file's header, so a pod replaced over the same
// file keeps it; the address its peer server answers at; and its disk, with
// the weight of the disk it is given. A host that keeps no cache disk, or
// gives it no space, is no member, and the zero host says so.
func memberOf(ctx context.Context, config Config, identity rank.Identity) membership.Host {
	weight := rank.Weight(cacheDisk(ctx, config))
	if identity.IsZero() || weight == 0 || config.Migration.Address == "" {
		return membership.Host{}
	}
	volume := config.CacheVolume
	if volume == "" {
		volume = identity.String()
	}
	return membership.Host{ID: identity, Address: config.Migration.Address,
		Disks: []membership.Disk{{ID: identity, Volume: volume, Weight: weight}}}
}

// alone is what a host holds before it has read the membership: itself, its
// disk serving, under the code of one host.
func alone(self membership.Host) membership.Membership {
	var disk membership.Disk
	if len(self.Disks) > 0 {
		disk = self.Disks[0]
	}
	return membership.Alone(membership.Member{ID: self.ID, Address: self.Address}, disk)
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

// Member is this host as the membership names it, and whether it is one: a
// host that keeps no cache disk and serves no shard is not. A host that serves
// shards reports the shards it holds open.
func (h *Host) Member() (membership.Host, bool) {
	self := h.self
	if h.shards != nil {
		self.Disks = h.shards.held()
	}
	if h.journalDisks != nil {
		self.Disks = append(slices.Clone(self.Disks), h.journalDisks.held()...)
	}
	return self, !self.ID.IsZero()
}

// Membership is the membership this host holds now, which it ranks windows
// and routes its cache's requests by.
func (h *Host) Membership() membership.Membership { return h.view.Current() }

// RefreshMembership reads the membership now rather than at the next turn of
// its timer or the next request that names a newer generation: what a test,
// or a simulated deployment that turned the timer off, does once the
// membership changed.
func (h *Host) RefreshMembership(ctx context.Context) error {
	_, err := h.view.Refresh(ctx)
	return err
}

// SettleDisks opens and closes the shards and the journal disks the
// membership this host holds calls for now, and returns once it has: what a
// test, or a simulated deployment that steps its controller by hand, does
// after the host read a new generation or the cloud attached a disk. A host
// that serves neither does nothing.
func (h *Host) SettleDisks(ctx context.Context) {
	if h.shards != nil {
		h.shards.pass(ctx)
	}
	if h.journalDisks != nil {
		h.journalDisks.pass(ctx)
	}
}

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

// memberReport is this host as /status reports it to the membership, and the
// membership it holds.
func memberReport(self membership.Host, held membership.ViewStatus) (*hostapi.Member, hostapi.Membership) {
	var member *hostapi.Member
	if !self.ID.IsZero() {
		reported := hostapi.MemberOf(self, held.Membership)
		member = &reported
	}
	return member, hostapi.MembershipOf(held)
}
