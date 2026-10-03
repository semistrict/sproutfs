package host

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/testpager"
	"github.com/semistrict/sproutfs/internal/testresource"
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

// slotConfigs is the configuration of each pager a host runs, in the order
// its supervisor starts them.
func slotConfigs(config SupervisorConfig) []slotConfig {
	configs := []slotConfig{{ramPager, pagerConfig(config, vmmemory.Ram)},
		{pmemPager, pagerConfig(config, vmmemory.Pmem)}}
	if cfg := ephemeralPagerConfig(config); cfg != nil {
		configs = append(configs, slotConfig{ephemeralPager, *cfg})
	}
	return configs
}

type slotConfig struct {
	slot pagerSlot
	cfg  vmmemory.Config
}

// startPagers starts each of a host's pagers as its supervisor does, each over
// an arena of the test's and its own spill file on disk, and returns the spill
// files the limiter measures. The first pager the disk cannot hold ends it with
// its error. ctx carries the disk's runtime, so the pagers consult the guards
// SPROUTFS_SIM_BUG names.
func startPagers(t *testing.T, ctx context.Context, config SupervisorConfig, disk platform.Disk) (diskFiles, error) {
	t.Helper()
	files := diskFiles{spills: map[pagerSlot]platform.File{}}
	resources := testresource.New()
	for _, pager := range slotConfigs(config) {
		built, spill, err := newPager(ctx, disk, resources, pager.slot, pager.cfg, testpager.NewArena(pager.cfg.Arena))
		if err != nil {
			return files, err
		}
		t.Cleanup(func() {
			if err := built.Close(context.Background()); err != nil {
				t.Errorf("closing the %s pager: %v", pager.slot, err)
			}
			if err := spill.Close(); err != nil {
				t.Errorf("closing the %s spill file: %v", pager.slot, err)
			}
		})
		files.spills[pager.slot] = spill
	}
	return files, nil
}

// mustStartPagers is startPagers on a disk that holds every pager.
func mustStartPagers(t *testing.T, ctx context.Context, config SupervisorConfig, disk platform.Disk) diskFiles {
	t.Helper()
	files, err := startPagers(t, ctx, config, disk)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// simDisk is a node's disk of total bytes, and a context that carries its
// runtime.
func simDisk(t *testing.T, total int64) (context.Context, *sim.Runtime, *sim.Disk) {
	t.Helper()
	runtime := sim.New(sim.Config{})
	disk := runtime.NewDisk("node", sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: total}})
	return sim.WithRuntime(t.Context(), runtime), runtime, disk
}

// A host promises each spill file the dirty pages its pager may hold, each
// running VMM a state file as large as a capture, and a staged image what it
// holds. The page cache's disk is no promise: it is the cache, which the
// limiter gives what is left.
func TestAHostPromisesItsDiskToWhatCannotGiveItBack(t *testing.T) {
	config := deploymentConfig()
	config.Ephemeral = EphemeralBudget{ArenaBytes: 256 * mib, DiskBytes: 2048 * mib}
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

// Once a host's pagers start, each spill file holds its whole extent, which is
// its promise: the space is the host's before anything is spilled, and no other
// writer on the node can take it.
func TestAHostsSpillFilesHoldTheirWholeExtentOnceItsPagersStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, _, disk := simDisk(t, 256*mib)
		files := mustStartPagers(t, ctx, diskHost(32*mib), disk)
		want := map[pagerSlot]int64{ramPager: 32 * mib, pmemPager: 16 * mib, ephemeralPager: 32 * mib}
		for slot, spill := range files.spills {
			size, err := spill.Size(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			holds, err := spill.(platform.FileAllocation).Allocated(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if size != want[slot] || holds != want[slot] {
				t.Fatalf("the %s spill file is %d MiB and holds %d, want %d and %d",
					slot, size/mib, holds/mib, want[slot]/mib, want[slot]/mib)
			}
		}
		if got := disk.Usage().HostBytes; got != 80*mib {
			t.Fatalf("the host holds %d MiB of its disk, want 80", got/mib)
		}
	})
}

// A host counts its spill files at their promise, which is what they hold, and
// refuses to start where the disk holds them but cannot keep its promises
// under its goal.
func TestAHostWhosePromisesDoNotFitIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, runtime, disk := simDisk(t, 256*mib)
		config := diskHost(32 * mib)
		config.Disk, config.Clock = disk, runtime.NewClock("host")
		var staged atomic.Int64
		none := func() int { return 0 }
		limiter, err := startDiskLimiter(ctx, config,
			diskUsers(config, mustStartPagers(t, ctx, config, disk), none, &staged), nil)
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
			len(report.Promises) != 5 || report.Promises[0].AllocatedBytes != 32*mib ||
			len(report.Writes.Refused) != 4 {
			t.Fatalf("the host reports %+v", report)
		}

		// 32 + 16 + 200 fits the filesystem, but leaves 8 MiB free of a floor
		// of 16 with nothing else on it, which no other writer can change.
		ctx, runtime, disk = simDisk(t, 256*mib)
		config = diskHost(200 * mib)
		config.Disk, config.Clock = disk, runtime.NewClock("larger")
		_, err = startDiskLimiter(ctx, config,
			diskUsers(config, mustStartPagers(t, ctx, config, disk), none, &staged), nil)
		want := "host: invalid configuration: the disk cannot keep the host's promises: the host promises " +
			"260046848 bytes, and the goals (free 16777216 bytes) leave it 251658240 of a filesystem of 268435456"
		if !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, resource.ErrDiskPromises) || err.Error() != want {
			t.Fatalf("a host promising 248 MiB of 240 started with %v, want %q", err, want)
		}
	})
}

// A host whose promises fit the filesystem, but not the space another writer
// leaves it now, starts and reports itself unready: the other writer may be
// another host's cache on the same node, which gives space back as its own
// goals push it. Once the space is back the host is ready. A host that refused
// to start would truncate its spill files on the way out, and the other
// writer would never see the pressure.
func TestAHostStartsUnreadyWhileAnotherWriterHoldsItsRoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, runtime, disk := simDisk(t, 256*mib)
		config := diskHost(32 * mib)
		clock := runtime.NewClock("host")
		config.Disk, config.Clock = disk, clock
		var staged atomic.Int64
		users := diskUsers(config, mustStartPagers(t, ctx, config, disk), func() int { return 0 }, &staged)
		// The spill files hold 80 MiB. 170 more leave 6 free: under the
		// floor of 16, though the filesystem holds the promises with 160 over.
		disk.SetOutsideBytes(170 * mib)
		limiter, err := startDiskLimiter(ctx, config, users, nil)
		if err != nil {
			t.Fatalf("a host whose promises fit the filesystem was refused while another writer held it: %v", err)
		}
		defer limiter.Close()
		want := "the disk cannot keep the host's promises: the host promises 83886080 bytes to spill-ram, " +
			"spill-pmem, spill-ephemeral, vmm-staging, staged-images, and the free-bytes goal (free 16777216 bytes) " +
			"leaves it 73400320"
		if err := limiter.Ready(); err == nil || err.Error() != want {
			t.Fatalf("the host started ready with %v, want %q", err, want)
		}
		disk.SetOutsideBytes(100 * mib)
		ticks := 0
		for limiter.Ready() != nil {
			clock.Advance(resource.DefaultDiskInterval)
			time.Sleep(time.Second)
			synctest.Wait()
			ticks++
		}
		// The first reading sees the space back, and the smoothed room moves
		// towards it only as time passes after it: the second reading finds
		// the 10 MiB the host needs inside the 70 MiB that came back.
		if ticks != 2 {
			t.Fatalf("the host was ready %d readings after the other writer gave space back, want 2", ticks)
		}
	})
}

// A host takes the first page cache file in the cache directory no other host
// holds, so two hosts on one node never share one, and a host started after
// one exited takes that host's file back.
func TestAHostClaimsACacheFileNoOtherHostHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, _, disk := simDisk(t, 256*mib)
		config := SupervisorConfig{Disk: disk, CacheDisk: disk}
		claim := func() (platform.File, string) {
			t.Helper()
			file, name, err := openCacheFile(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			return file, name
		}
		first, firstName := claim()
		second, secondName := claim()
		defer second.Close()
		if firstName != "cache-0" || secondName != "cache-1" {
			t.Fatalf("two hosts on one node claimed %s and %s, want cache-0 and cache-1", firstName, secondName)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		third, thirdName := claim()
		defer third.Close()
		if thirdName != "cache-0" {
			t.Fatalf("a host started after cache-0's host exited claimed %s, want cache-0", thirdName)
		}
		// A host given no cache directory keeps its cache in the scratch.
		scratch, name, err := openCacheFile(ctx, SupervisorConfig{Disk: disk})
		if err != nil {
			t.Fatal(err)
		}
		defer scratch.Close()
		if name != "cache-2" {
			t.Fatalf("a host with no cache directory claimed %s, want cache-2 beside the others", name)
		}
	})
}

// The one disk limiter measures one filesystem, so a cache directory on
// another filesystem than the scratch is refused.
func TestACacheDirectoryOnAnotherFilesystemIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, runtime, disk := simDisk(t, 256*mib)
		other := runtime.NewDisk("ssd", sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: 256 * mib}})
		_, _, err := openCacheFile(ctx, SupervisorConfig{Disk: disk, CacheDisk: other})
		want := "host: invalid configuration: the cache directory is on filesystem ssd and the scratch directory " +
			"on node; the disk limiter measures one filesystem, so both must be on it"
		if !errors.Is(err, ErrInvalidConfig) || err.Error() != want {
			t.Fatalf("a cache directory on another filesystem was claimed with %v, want %q", err, want)
		}
		if files, err := other.List(ctx, ""); err != nil || len(files) != 0 {
			t.Fatalf("the refused cache directory holds %v (%v), want nothing", files, err)
		}
	})
}

// A host whose disk cannot hold its spill files refuses to start, as a
// configuration the disk cannot keep. The pagers it did start keep what they
// hold until they close, and the one it could not start holds nothing.
func TestAHostWhoseDiskCannotHoldItsSpillFilesIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, _, disk := simDisk(t, 256*mib)
		// 32 + 16 + 256 is more than the filesystem.
		_, err := startPagers(t, ctx, diskHost(256*mib), disk)
		if !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, platform.ErrNoSpace) {
			t.Fatalf("a host whose spill files need 304 MiB of 256 started with %v, want %v and %v",
				err, ErrInvalidConfig, platform.ErrNoSpace)
		}
		want := "host: invalid configuration: the disk cannot allocate the ephemeral spill file's 268435456 bytes: " +
			"allocating the spill file's 268435456 bytes: filesystem space exhausted"
		if err.Error() != want {
			t.Fatalf("the host was refused with %q, want %q", err, want)
		}
		if got := disk.Usage().HostBytes; got != 48*mib {
			t.Fatalf("the refused host holds %d MiB of its disk, want the 48 of the pagers it started", got/mib)
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
		ctx, runtime, disk := simDisk(t, 256*mib)
		// 256 - 16 - (32 + 16 + 128) leaves 64: one region.
		config := diskHost(128 * mib)
		config.Disk, config.Clock = disk, runtime.NewClock("host")
		var staged atomic.Int64
		limiter, err := startDiskLimiter(ctx, config,
			diskUsers(config, mustStartPagers(t, ctx, config, disk), func() int { return 0 }, &staged), nil)
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
