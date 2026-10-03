package host

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform"
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

// startDiskLimiter builds the host's disk limiter over Disk, and refuses a
// configuration whose promises the disk cannot keep under its goals.
func startDiskLimiter(ctx context.Context, config SupervisorConfig, users []resource.DiskUser) (*resource.DiskLimiter, error) {
	space, ok := config.Disk.(platform.DiskSpace)
	if !ok {
		return nil, fmt.Errorf("%w: the host's disk does not report its space", ErrInvalidConfig)
	}
	limiter, err := resource.NewDiskLimiter(ctx, resource.DiskLimiterConfig{Space: space, Goal: config.DiskGoal,
		Users: users, Region: checkpoint.DefaultDiskRegionBytes, MaxBandBytes: config.DiskBandBytes, Writes: config.DiskWrites, Device: config.DeviceWrites,
		Clock: config.Clock})
	if err != nil {
		return nil, fmt.Errorf("%w: the disk limiter: %w", ErrInvalidConfig, err)
	}
	if err := limiter.Ready(); err != nil {
		limiter.Close()
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	status := limiter.Status()
	promises := make([]any, 0, 2*len(status.Promises))
	for _, promise := range status.Promises {
		promises = append(promises, promise.Name, promise.PromisedBytes)
	}
	slog.InfoContext(ctx, "host: the disk limiter was assembled", "goal", status.Goal.String(),
		"binding", string(status.Binding), "total_bytes", status.TotalBytes, "available_bytes", status.AvailableBytes,
		"floor_bytes", status.FloorBytes, "promised_bytes", status.PromisedBytes, slog.Group("promises", promises...),
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
		FloorBytes: status.FloorBytes, BandBytes: status.BandBytes, Promises: promises,
		PromisedBytes: status.PromisedBytes, CacheHeldBytes: status.CacheHeldBytes,
		CacheShareBytes: status.CacheShareBytes, Unready: status.Unready, ReadError: status.ReadError,
		Writes: hostapi.DiskWrites{BytesPerDay: status.Writes.BytesPerDay, BurstBytes: status.Writes.BurstBytes,
			WrittenBytes: status.Writes.WrittenBytes, AdmittedBytes: status.Writes.AdmittedBytes,
			LeftBytes: status.Writes.LeftBytes, Refused: status.Writes.Refused[:]},
	}
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
// it. The share is the limiter's, under the configured cap where there is one.
// A write is admitted by the limiter's write budget, at the priority of its
// kind: the cache's kinds of write are numbered lowest first, as the
// limiter's priorities are.
type cacheShare struct {
	limiter *resource.DiskLimiter
	cap     int64
}

func (s cacheShare) Share() int64 {
	share := s.limiter.CacheShare()
	if s.cap > 0 {
		share = min(share, s.cap)
	}
	return share
}

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
