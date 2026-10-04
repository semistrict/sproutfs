package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
)

// stagingRegion is how much of an image a host stages between two checks that
// the disk can keep it.
const stagingRegion = 64 << 20

// The names the disk limiter reports the host's promises by.
const (
	promiseVMMStaging   = "vmm-staging"
	promiseStagedImages = "staged-images"
)

// diskFiles are the files of the host's disk a promise is measured by: each
// pager's spill file.
type diskFiles struct {
	spills map[pagerSlot]platform.File
}

// diskUsers is every user of the host's disk that cannot give space back:
//
//   - each pager's spill file, at the dirty pages it may hold, which is the
//     extent the pager allocates when it starts;
//   - each running VMM's staging, at the largest state a capture may write;
//   - the images staged for an import, at what they hold.
//
// running counts the VMMs this host runs, and staged what its staged images
// hold.
func diskUsers(config SupervisorConfig, files diskFiles, running func() int, staged *atomic.Int64) []resource.DiskUser {
	var users []resource.DiskUser
	spill := func(slot pagerSlot, cfg vmmemory.Config) {
		promise := int64(cfg.DirtyPages) * int64(cfg.PageSize)
		users = append(users, resource.DiskUser{Name: "spill-" + string(slot),
			Promised: func() int64 { return promise }, Allocated: allocation(files.spills[slot])})
	}
	spill(ramPager, pagerConfig(config, vmmemory.Ram))
	spill(pmemPager, pagerConfig(config, vmmemory.Pmem))
	if cfg := ephemeralPagerConfig(config); cfg != nil {
		spill(ephemeralPager, *cfg)
	}
	users = append(users,
		resource.DiskUser{Name: promiseVMMStaging,
			Promised: func() int64 { return int64(running()) * vmmachine.MaxStateBytes }},
		resource.DiskUser{Name: promiseStagedImages, Promised: staged.Load,
			Allocated: func(context.Context) (int64, error) { return staged.Load(), nil }})
	return users
}

// allocation reads what a file holds, nil for a file whose adapter cannot
// tell, which the limiter counts as holding nothing.
func allocation(file platform.File) func(context.Context) (int64, error) {
	allocated, ok := file.(platform.FileAllocation)
	if !ok {
		return nil
	}
	return allocated.Allocated
}

// cacheSlots is how many hosts may keep a page cache's disk in one directory
// at once: one file each, cache-0 to cache-63.
const cacheSlots = 64

// openCacheFile claims the page cache's disk: the first file of the cache
// directory, cache-0, cache-1 and so on, whose lock no other host holds. The
// file is locked while it is open, and the lock ends with the process, so two
// hosts on one node never share a file, and a host started after another
// exited takes the lowest file free, with what that host kept in it. It
// returns the file and its name. The cache directory must be on the
// filesystem the scratch is on, because the one disk limiter measures one
// filesystem.
func openCacheFile(ctx context.Context, config SupervisorConfig) (platform.File, string, error) {
	directory := config.Disk
	if config.CacheDisk != nil {
		if err := sameFilesystem(ctx, config.Disk, config.CacheDisk); err != nil {
			return nil, "", err
		}
		directory = config.CacheDisk
	}
	for slot := range cacheSlots {
		name := fmt.Sprintf("cache-%d", slot)
		options := platform.OpenOptions{Create: true, Lock: true, Permissions: 0o600}
		if sim.Bug(ctx, "host-cache-share-a-file") {
			options.Lock = false
		}
		file, err := directory.Open(ctx, name, options)
		if errors.Is(err, platform.ErrLocked) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("the page cache's disk %s: %w", name, err)
		}
		return file, name, nil
	}
	return nil, "", fmt.Errorf("%w: other hosts hold every one of the %d page cache files in the cache directory",
		ErrInvalidConfig, cacheSlots)
}

// sameFilesystem refuses a cache directory on another filesystem than the
// scratch.
func sameFilesystem(ctx context.Context, scratch, cache platform.Disk) error {
	read := func(disk platform.Disk, what string) (platform.FilesystemSpace, error) {
		space, ok := disk.(platform.DiskSpace)
		if !ok {
			return platform.FilesystemSpace{}, fmt.Errorf("%w: the %s does not report its filesystem", ErrInvalidConfig, what)
		}
		reading, err := space.Space(ctx)
		if err != nil {
			return platform.FilesystemSpace{}, fmt.Errorf("the %s's filesystem: %w", what, err)
		}
		return reading, nil
	}
	scratchSpace, err := read(scratch, "scratch directory")
	if err != nil {
		return err
	}
	cacheSpace, err := read(cache, "cache directory")
	if err != nil {
		return err
	}
	if scratchSpace.ID != cacheSpace.ID && !sim.Bug(ctx, "host-cache-on-another-filesystem") {
		return fmt.Errorf("%w: the cache directory is on filesystem %s and the scratch directory on %s; "+
			"the disk limiter measures one filesystem, so both must be on it", ErrInvalidConfig,
			cacheSpace.ID, scratchSpace.ID)
	}
	return nil
}

// startDiskLimiter builds the host's disk limiter over Disk, and refuses a
// configuration whose promises the disk cannot keep under its goals. cache is
// the page cache's file: what a restart left in it is the cache's, before the
// cache is made.
func startDiskLimiter(ctx context.Context, config SupervisorConfig, users []resource.DiskUser,
	cache platform.File) (*resource.DiskLimiter, error) {
	space, ok := config.Disk.(platform.DiskSpace)
	if !ok {
		return nil, fmt.Errorf("%w: the host's disk does not report its space", ErrInvalidConfig)
	}
	limiter, err := resource.NewDiskLimiter(ctx, resource.DiskLimiterConfig{Space: space, Goal: config.DiskGoal,
		Users: users, CacheFile: allocation(cache), Region: checkpoint.DefaultDiskRegionBytes, MaxBandBytes: config.DiskBandBytes,
		ReserveBytes: config.DiskReserveBytes, Writes: config.DiskWrites, Device: config.DeviceWrites,
		Clock: config.Clock})
	if err != nil {
		return nil, fmt.Errorf("%w: the disk limiter: %w", ErrInvalidConfig, err)
	}
	// Only promises this filesystem could never keep are a configuration to
	// refuse. Space another writer holds now — the cache of another host on
	// the node, an image being pulled — is given back as that writer's own
	// goals push it, so the host starts and reports itself unready until it
	// is. A host that refused would truncate its spill files as it exited,
	// and the other writer would never see the pressure that makes it give
	// space back.
	if err := limiter.Feasible(); err != nil {
		limiter.Close()
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := limiter.Ready(); err != nil {
		if sim.Bug(ctx, "host-refuse-start-while-the-disk-is-held") {
			limiter.Close()
			return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		slog.WarnContext(ctx, "host: the disk cannot keep the host's promises until other writers give space back",
			"error", err)
	}
	status := limiter.Status()
	promises := make([]any, 0, 2*len(status.Promises))
	for _, promise := range status.Promises {
		promises = append(promises, promise.Name, promise.PromisedBytes)
	}
	slog.InfoContext(ctx, "host: the disk limiter was assembled", "goal", status.Goal.String(),
		"binding", string(status.Binding), "total_bytes", status.TotalBytes, "available_bytes", status.AvailableBytes,
		"floor_bytes", status.FloorBytes, "reserve_bytes", status.ReserveBytes, "promised_bytes", status.PromisedBytes, slog.Group("promises", promises...),
		"cache_share_bytes", status.CacheShareBytes, "write_bytes_per_day", status.Writes.BytesPerDay,
		"write_burst_bytes", status.Writes.BurstBytes)
	return limiter, nil
}

// diskReport is the limiter's status as the host API reports it.
func diskReport(status resource.DiskStatus) hostapi.Disk {
	promises := make([]hostapi.DiskPromise, 0, len(status.Promises))
	for _, promise := range status.Promises {
		promises = append(promises, hostapi.DiskPromise{Name: promise.Name, PromisedBytes: promise.PromisedBytes,
			AllocatedBytes: promise.AllocatedBytes})
	}
	return hostapi.Disk{
		Goal: hostapi.DiskGoal{FreeBytes: status.Goal.FreeBytes, FreePercent: status.Goal.FreePercent,
			UsedBytes: status.Goal.UsedBytes},
		Binding: string(status.Binding), TotalBytes: status.TotalBytes, AvailableBytes: status.AvailableBytes,
		SmoothTotalBytes: status.SmoothTotalBytes, SmoothFreeBytes: status.SmoothFreeBytes,
		FloorBytes: status.FloorBytes, ReserveBytes: status.ReserveBytes, BandBytes: status.BandBytes, Promises: promises,
		PromisedBytes: status.PromisedBytes, CacheHeldBytes: status.CacheHeldBytes,
		CacheShareBytes: status.CacheShareBytes, Unready: status.Unready, ReadError: status.ReadError,
		Writes: hostapi.DiskWrites{BytesPerDay: status.Writes.BytesPerDay, BurstBytes: status.Writes.BurstBytes,
			WrittenBytes: status.Writes.WrittenBytes, AdmittedBytes: status.Writes.AdmittedBytes,
			LeftBytes: status.Writes.LeftBytes, Refused: status.Writes.Refused[:]},
	}
}

// cacheDiskReport is the page cache's disk as /status reports it: the file the
// host claimed and the disk's own counters. A cache that keeps no disk has no
// identity, and is reported as none.
func cacheDiskReport(file string, disk checkpoint.DiskStats) *hostapi.CacheDisk {
	if disk.Identity.IsZero() {
		return nil
	}
	return &hostapi.CacheDisk{File: file, Identity: disk.Identity.String(), Regions: disk.Regions,
		Entries: disk.Entries, IndexBytes: disk.IndexBytes, Hits: disk.Hits, Lost: disk.Lost,
		Evicted: disk.Evicted, Rewritten: disk.Rewritten, Refused: disk.Refused,
		Opened: hostapi.CacheDiskOpened{FromTables: disk.FromTables, Scanned: disk.Scanned,
			GivenBack: disk.GivenBackOnOpen}}
}

// cacheShardsReport is each shard the host serves, as /status reports it:
// what its disk keeps, and what it found there when the host opened it.
func cacheShardsReport(shards []checkpoint.DiskStats) []hostapi.CacheDisk {
	var reported []hostapi.CacheDisk
	for _, shard := range shards {
		reported = append(reported, *cacheDiskReport("", shard))
	}
	return reported
}

// cacheFillReport is what the host's fills of the cluster's disk cache did,
// as /status reports it: nothing for a host that keeps no cache disk and
// serves no shard, which member says.
func cacheFillReport(member bool, fill checkpoint.FillStats) *hostapi.CacheFill {
	if !member {
		return nil
	}
	dropped := make(map[string]uint64, len(fill.Dropped))
	for _, reason := range checkpoint.DropReasons() {
		dropped[reason.String()] = fill.Dropped[reason]
	}
	return &hostapi.CacheFill{FromReads: fill.FromReads, FromPublications: fill.FromPublications,
		WithoutRight: fill.WithoutRight, RightsGranted: fill.RightsGranted, Sent: fill.Sent,
		SentBytes: fill.SentBytes, Kept: fill.Kept, Dropped: dropped, Duplicates: fill.Duplicates,
		Refused: fill.Refused, QueuedBytes: fill.Queued, QueueBytes: fill.QueueBytes,
		QueuedPeakBytes: fill.QueuedPeak, PublicationWaits: fill.Waits,
		PublicationWaitedSeconds: fill.Waited.Seconds(), PublicationsGaveUp: fill.GaveUp}
}

// hotTierReport is what the host's reads through its hot tier and its fills
// of it did, as /status reports them: nothing for a host with no hot tier.
func hotTierReport(stats *checkpoint.HotTierStats) *hostapi.HotTier {
	if stats == nil {
		return nil
	}
	failed := make(map[string]uint64, len(stats.Failed))
	for _, reason := range checkpoint.HotFailures() {
		failed[reason.String()] = stats.Failed[reason]
	}
	dropped := make(map[string]uint64, len(stats.Dropped))
	for _, reason := range checkpoint.HotDrops() {
		dropped[reason.String()] = stats.Dropped[reason]
	}
	return &hostapi.HotTier{Hits: stats.Hits, Misses: stats.Misses, Failed: failed, Skipped: stats.Skipped,
		MarkedDown: stats.MarkedDown, Down: stats.Down, FromReads: stats.FromReads,
		FromPublications: stats.FromPublications, Duplicates: stats.Duplicates, Sent: stats.Sent,
		SentBytes: stats.SentBytes, Present: stats.Present, Dropped: dropped, HeadChecks: stats.HeadChecks,
		HeadMissing: stats.HeadMissing, QueuedBytes: stats.Queued, QueueBytes: stats.QueueBytes}
}

// cacheReadReport is what the host's reads of the cluster's disk cache did,
// and what its peer server served of its cache, as /status reports them:
// nothing for a host that keeps no cache disk and serves no shard, which
// member says.
func cacheReadReport(member bool, read checkpoint.ReadStats, served peer.ServerStats) *hostapi.CacheRead {
	if !member {
		return nil
	}
	return &hostapi.CacheRead{Hits: read.Hits, OwnHits: read.OwnHits, EarlierHits: read.EarlierHits,
		Misses: read.Misses, Requests: read.Requests,
		Replaced: read.Replaced, SecondRequests: read.SecondRequests, RefusedByBudget: read.Refused,
		StoreHedges: read.StoreHedges, StoreHedgesWon: read.StoreHedgesWon, StoreHedgesRefused: read.StoreHedgesRefused,
		WrongStripes: read.WrongStripes, DropsSent: read.DropsSent, Repairs: read.Repairs, Timeouts: read.Timeouts,
		MarkedDown: read.MarkedDown, MarkCapped: read.Capped, MarkCleared: read.Cleared, Down: read.Down,
		HeadChecks: read.HeadChecks, HeadMissing: read.HeadMissing, Classes: readClassReport(read.Classes),
		Served: served.StripeReads, ServedStripes: served.Stripes, ServedBytes: served.StripeBytes,
		ServeBusy: served.StripesBusy}
}

// readClassReport is a reader's delay and bound for each size class of read,
// as /status reports them.
func readClassReport(classes []checkpoint.ReadClass) []hostapi.CacheReadClass {
	report := make([]hostapi.CacheReadClass, len(classes))
	for at, class := range classes {
		report[at] = hostapi.CacheReadClass{UpToBytes: class.Bytes, Reads: class.Reads, Delay: class.Delay,
			Bound: class.Bound}
	}
	return report
}

// stagedWriter stages an image on the host's disk. It counts what the image
// holds as a promise, and before each region it asks the limiter whether the
// disk can keep one more, so an import the disk cannot hold is refused rather
// than left to fill it.
type stagedWriter struct {
	ctx     context.Context
	to      io.Writer
	limiter *resource.DiskLimiter
	staged  *atomic.Int64
	// written is what this image holds, and checked how far the limiter has
	// said the disk can keep it.
	written, checked int64
}

func (w *stagedWriter) Write(p []byte) (int, error) {
	for w.written+int64(len(p)) > w.checked {
		if err := w.limiter.Fits(w.ctx, stagingRegion); err != nil {
			return 0, fmt.Errorf("staging an image past %d bytes: %w", w.written, err)
		}
		w.checked += stagingRegion
	}
	n, err := w.to.Write(p)
	w.written += int64(n)
	w.staged.Add(int64(n))
	return n, err
}

// release gives back the promise once the staged image is removed.
func (w *stagedWriter) release() { w.staged.Add(-w.written) }

// cacheShare is the page cache's disk budget as the host's disk limiter sets
// it: the share is the limiter's alone. A write is admitted by the limiter's
// write budget, at the priority of its kind: the cache's kinds of write are
// numbered lowest first, as the limiter's priorities are.
type cacheShare struct {
	limiter *resource.DiskLimiter
}

func (s cacheShare) Share() int64 { return s.limiter.CacheShare() }

func (s cacheShare) Admit(n int64, kind checkpoint.WriteKind) bool {
	return s.limiter.Admit(n, int(kind))
}

// cacheFitter is the page cache's disk as the limiter shrinks it. The cache
// gives regions back, oldest first and with no second chance, until it holds
// its share less one region, which is the target the limiter asks for, because
// both count in the same regions. Giving a region back only closes and punches
// it, so it is done in the limiter's own call: the limiter calls Shrink
// outside its lock, and the cache reads the share without waiting on it.
type cacheFitter struct {
	ctx        context.Context
	cache      *checkpoint.Cache
	unregister func()
}

// fitCacheDisk registers the page cache's disk with the limiter.
func fitCacheDisk(ctx context.Context, limiter *resource.DiskLimiter, cache *checkpoint.Cache) (*cacheFitter, error) {
	f := &cacheFitter{ctx: ctx, cache: cache}
	unregister, err := limiter.RegisterCache(f)
	if err != nil {
		return nil, err
	}
	f.unregister = unregister
	return f, nil
}

func (f *cacheFitter) Held() int64 { return f.cache.Stats().Disk.UsedBytes }

func (f *cacheFitter) Shrink(int64) {
	if err := f.cache.FitDisk(f.ctx); err != nil && context.Cause(f.ctx) == nil {
		slog.WarnContext(f.ctx, "host: giving the page cache's disk back to the limiter failed", "error", err)
	}
}

// stop unregisters the cache, which must outlive any shrink the limiter has
// already begun: the limiter holds no lock across Shrink, so the cache is
// closed only after the host's context ends every fit in progress.
func (f *cacheFitter) stop() { f.unregister() }
