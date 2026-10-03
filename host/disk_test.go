package host

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory"
)

const mib = 1 << 20

// diskHost is a host's budgets small enough for a simulated disk: 32 MiB of
// dirty RAM and 16 MiB of dirty PMEM in 2 MiB pages, and an ephemeral disk.
func diskHost(ephemeral int64) SupervisorConfig {
	return SupervisorConfig{
		ArenaBytes:   KindBytes{RAM: 32 * mib, PMEM: 16 * mib},
		LogicalPages: KindPages{RAM: 64, PMEM: 64},
		DirtyPages:   KindPages{RAM: 16, PMEM: 8},
		Ephemeral:    EphemeralBudget{ArenaBytes: 8 * mib, DiskBytes: ephemeral},
		DiskGoal:     resource.DiskGoal{FreeBytes: 16 * mib},
	}
}

// openSpills opens each pager's spill file on disk and sizes it as the pager
// does, without writing any of it.
func openSpills(t *testing.T, ctx context.Context, config SupervisorConfig, disk platform.Disk) diskFiles {
	t.Helper()
	files := diskFiles{spills: map[pagerSlot]platform.File{}}
	configs := map[pagerSlot]vmmemory.Config{ramPager: pagerConfig(config, vmmemory.Ram),
		pmemPager: pagerConfig(config, vmmemory.Pmem)}
	if cfg := ephemeralPagerConfig(config); cfg != nil {
		configs[ephemeralPager] = *cfg
	}
	for slot, cfg := range configs {
		file, err := disk.Open(ctx, "spill-"+string(slot), platform.OpenOptions{Create: true, Truncate: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(ctx, int64(cfg.DirtyPages)*int64(cfg.PageSize)); err != nil {
			t.Fatal(err)
		}
		files.spills[slot] = file
	}
	return files
}

// A host promises each spill file the dirty pages its pager may hold, each
// running VMM a state file as large as a capture, and a staged image what it
// holds. The page cache's disk is no promise: it is the cache, which the
// limiter gives what is left.
func TestAHostPromisesItsDiskToWhatCannotGiveItBack(t *testing.T) {
	config := deploymentConfig()
	config.Ephemeral = EphemeralBudget{ArenaBytes: 256 * mib, DiskBytes: 2048 * mib}
	config.CacheDiskBytes = 4096 * mib
	// The cap bounds the cache. It promises nothing.
	var staged atomic.Int64
	staged.Store(5 * mib)
	running := 3
	type promise struct {
		name  string
		bytes int64
	}
	var got []promise
	for _, user := range diskUsers(config, diskFiles{}, func() int { return running }, &staged) {
		got = append(got, promise{user.Name, user.Promised()})
	}
	want := []promise{
		{"spill-ram", 4608 * 2 * mib},
		{"spill-pmem", 1536 * 2 * mib},
		{"spill-ephemeral", 2048 * mib},
		{promiseVMMStaging, 3 * 64 * mib},
		{promiseStagedImages, 5 * mib},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the host promises %v, want %v", got, want)
	}
}

// A host counts its spill files at their promise while they are sparse, and
// refuses to start where the disk cannot keep its promises under its goal.
func TestAHostWhosePromisesDoNotFitIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node", sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: 256 * mib}})
		config := diskHost(32 * mib)
		config.Disk, config.Clock = disk, runtime.NewClock("host")
		var staged atomic.Int64
		none := func() int { return 0 }
		limiter, err := startDiskLimiter(t.Context(), config,
			diskUsers(config, openSpills(t, t.Context(), config, disk), none, &staged))
		if err != nil {
			t.Fatal(err)
		}
		// 256 - 16 - (32 + 16 + 32) = 160, less a band of a fifth.
		status := limiter.Status()
		limiter.Close()
		if status.PromisedBytes != 80*mib || status.CacheShareBytes != 128*mib {
			t.Fatalf("the host promised %d MiB and gave the cache %d, want 80 and 128",
				status.PromisedBytes/mib, status.CacheShareBytes/mib)
		}
		if report := diskReport(status); report.PromisedBytes != 80*mib || report.Binding != "free-bytes" ||
			len(report.Promises) != 5 || report.Promises[0].AllocatedBytes != 0 || len(report.Writes.Refused) != 4 {
			t.Fatalf("the host reports %+v", report)
		}

		config = diskHost(256 * mib)
		config.Disk, config.Clock = disk, runtime.NewClock("larger")
		_, err = startDiskLimiter(t.Context(), config,
			diskUsers(config, openSpills(t, t.Context(), config, disk), none, &staged))
		if !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, resource.ErrDiskPromises) {
			t.Fatalf("a host promising 304 MiB of 240 started with %v, want %v and %v", err, ErrInvalidConfig,
				resource.ErrDiskPromises)
		}
	})
}

// fileWriter writes to a simulated file from its start.
type fileWriter struct {
	ctx  context.Context
	file platform.File
	at   int64
}

func (w *fileWriter) Write(p []byte) (int, error) {
	n, err := w.file.WriteAt(w.ctx, p, w.at)
	w.at += int64(n)
	return n, err
}

// An image is staged a region at a time while the disk can keep one more, and
// counted as a promise until it is removed.
func TestAnImageTheDiskCannotKeepIsNotStaged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node", sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: 256 * mib}})
		// 256 - 16 - (32 + 16 + 128) leaves 64: one region.
		config := diskHost(128 * mib)
		config.Disk, config.Clock = disk, runtime.NewClock("host")
		var staged atomic.Int64
		limiter, err := startDiskLimiter(t.Context(), config,
			diskUsers(config, openSpills(t, t.Context(), config, disk), func() int { return 0 }, &staged))
		if err != nil {
			t.Fatal(err)
		}
		defer limiter.Close()
		image, err := disk.Open(t.Context(), "template-1", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		writer := &stagedWriter{ctx: t.Context(), to: &fileWriter{ctx: t.Context(), file: image},
			limiter: limiter, staged: &staged}
		piece := make([]byte, mib)
		for i := range piece {
			piece[i] = byte(i%251 + 1)
		}
		var copied int64
		for copied < 3*stagingRegion {
			n, err := writer.Write(piece)
			copied += int64(n)
			if err != nil {
				if !errors.Is(err, resource.ErrDiskPromises) {
					t.Fatal(err)
				}
				break
			}
		}
		if copied != stagingRegion || staged.Load() != stagingRegion {
			t.Fatalf("the host staged %d bytes and promised %d, want one region of %d",
				copied, staged.Load(), stagingRegion)
		}
		writer.release()
		if staged.Load() != 0 {
			t.Fatalf("a released image is still promised %d bytes", staged.Load())
		}
	})
}
