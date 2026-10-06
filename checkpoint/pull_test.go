package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// pullRegionBytes is the disk region the pull fixtures use: room for three of
// their 2 MiB pages, so a 64 MiB share holds eight regions.
const pullRegionBytes = 8 << 20

// pullFixture is one published checkpoint of four 2 MiB pages, read through a
// store whose page cache keeps a disk of diskBytes. Its memory holds 4 KiB,
// which no page fits in, so every read of a page the disk does not serve is a
// request of the store. The index is opened afresh, so it has decoded no
// segment yet, and the gets are counted from there.
type pullFixture struct {
	store   *checkpoint.Store
	index   *checkpoint.Index
	model   *model
	cache   *checkpoint.Cache
	objects *cacheStore
	disk    *sim.Disk
	file    platform.File
	runtime *sim.Runtime
	// pages is the pages the checkpoint published.
	pages []uint64
}

// ctx is the test's context carrying the fixture's runtime, so the in-tree bug
// guards a negative test enables reach the pull.
func (f *pullFixture) ctx(t *testing.T) context.Context {
	return sim.WithRuntime(t.Context(), f.runtime)
}

func newPullFixture(t *testing.T, diskBytes int64) *pullFixture {
	t.Helper()
	return newPullFixtureOf(t, diskBytes, cachedPages, []uint64{0, 1, 2, 3})
}

// newPullFixtureOf is a pull fixture whose volume is volumePages 2 MiB pages,
// of which the checkpoint publishes pages.
func newPullFixtureOf(t *testing.T, diskBytes int64, volumePages uint64, pages []uint64) *pullFixture {
	t.Helper()
	return newPullFixtureWith(t, diskBytes, volumePages, pages, nil)
}

// newPullFixtureWith is newPullFixtureOf with a last say over its disk's and
// its cache's configuration, under the fixture's runtime.
func newPullFixtureWith(t *testing.T, diskBytes int64, volumePages uint64, pages []uint64,
	configure func(runtime *sim.Runtime, disk *sim.DiskConfig, config *checkpoint.CacheConfig)) *pullFixture {
	t.Helper()
	runtime := sim.New(sim.Config{})
	diskConfig := sim.DiskConfig{}
	config := checkpoint.CacheConfig{DiskBytes: diskBytes, DiskRegionBytes: pullRegionBytes}
	if configure != nil {
		configure(runtime, &diskConfig, &config)
	}
	disk := runtime.NewDisk("host", diskConfig)
	file, err := disk.Open(t.Context(), "cache", platform.OpenOptions{Create: true, Truncate: true})
	if err != nil {
		t.Fatal(err)
	}
	config.Disk = file
	budget, err := resource.New(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(sim.WithRuntime(t.Context(), runtime), budget, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	objects := &cacheStore{ObjectStore: runtime.ObjectStore(), suspended: "/part/",
		entered: make(chan struct{}, 64), release: make(chan struct{})}
	store := mustStore(t, checkpoint.Config{ObjectStore: objects, Cache: cache})
	sizes := map[string]uint64{"root": volumePages * checkpoint.PageSize2MiB}
	root, err := store.Root(t.Context(), control.Ref{VM: "pulled", Sequence: 1}, volumes2MiB(sizes))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(volumes2MiB(sizes))
	p := store.Begin(root, control.Ref{VM: "pulled", Sequence: 2})
	for _, page := range pages {
		for sector := range uint32(sectorsPerPage) {
			m.dirty(p, "root", page, sector, sectorData("pulled", page, sector))
		}
	}
	published, err := p.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	// The publication left the table of the segment it wrote in the cache's
	// memory. The fixture reads as a host that did not publish it, which
	// holds none of it.
	cache.Clear()
	index, err := store.Open(t.Context(), published.Ref())
	if err != nil {
		t.Fatal(err)
	}
	objects.gets.Store(0)
	return &pullFixture{store: store, index: index, model: m, cache: cache, objects: objects, disk: disk, file: file,
		runtime: runtime, pages: pages}
}

// pull begins a pull of the fixture's checkpoint, closed with the test.
func (f *pullFixture) pull(t *testing.T) *checkpoint.Pull {
	t.Helper()
	pull, err := f.store.Pull(f.ctx(t), f.index)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pull.Close)
	return pull
}

// readAll reads every page of the fixture's checkpoint and checks each against
// the model.
func (f *pullFixture) readAll(t *testing.T) {
	t.Helper()
	for _, page := range f.pages {
		readCachedPage(t, f.store, f.index, f.model, page)
	}
}

// Once a pull is complete, reading its checkpoint makes no request of the store:
// not the first read of a page, and not a read of a page the memory tier has
// since let go of, which is every page here, because none fits in it.
func TestAPulledCheckpointIsReadWithoutTheStore(t *testing.T) {
	f := newPullFixture(t, 64<<20)
	pull := f.pull(t)
	if err := pull.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	stats := pull.Stats()
	if !stats.Done || stats.Err != nil || stats.Pulled != stats.Bytes || stats.Bytes == 0 {
		t.Fatalf("the pull ended at %+v, want every byte of the checkpoint on the disk", stats)
	}
	// The pull fetched the segment and the members, and only those.
	fetched := f.objects.gets.Load()
	f.objects.gets.Store(0)
	f.readAll(t)
	f.readAll(t)
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("reading a pulled checkpoint twice made %d requests of the store, want none", gets)
	}
	// The first pass read the segment and four pages from the disk, and the
	// second the four pages again: the memory tier keeps the segment's table,
	// which fits in it where no page does.
	if disk := f.cache.Stats().Disk; disk.Hits != 9 || disk.Lost != 0 || disk.Entries != cachedPages+1 {
		t.Fatalf("the disk served %+v, want nine hits of five entries", disk)
	}
	// The four members lie next to each other in one part, so the pull fetched
	// them as one extent.
	if fetched != 2 {
		t.Fatalf("the pull made %d requests, want the segment and the one extent", fetched)
	}
}

// The page cache's disk outlives its cache. Once a pull is complete, the cache
// is closed and a new one, with a new store, is made over the same file, as a
// host that restarts makes them: reading the checkpoint then makes no request
// of the store, and the disk keeps the identity it was made with.
func TestAPulledCheckpointIsReadWithoutTheStoreAfterARestart(t *testing.T) {
	f := newPullFixture(t, 64<<20)
	pull := f.pull(t)
	if err := pull.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := f.cache.Stats().Disk
	f.cache.Close()
	file, err := f.disk.Open(t.Context(), "cache", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	budget, err := resource.New(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(t.Context(), budget, checkpoint.CacheConfig{Disk: file, DiskBytes: 64 << 20,
		DiskRegionBytes: pullRegionBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	disk := cache.Stats().Disk
	if disk.Identity != before.Identity || disk.FromTables != 1 || disk.Entries != before.Entries ||
		disk.GivenBackOnOpen != 0 {
		t.Fatalf("the restarted disk reports %+v, want the %d entries and identity %v it held", disk,
			before.Entries, before.Identity)
	}
	store := mustStore(t, checkpoint.Config{ObjectStore: f.objects, Cache: cache})
	index, err := store.Open(t.Context(), f.index.Ref())
	if err != nil {
		t.Fatal(err)
	}
	f.objects.gets.Store(0)
	for _, page := range f.pages {
		readCachedPage(t, store, index, f.model, page)
	}
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("reading the pulled checkpoint after a restart made %d requests of the store, want none", gets)
	}
	if hits := cache.Stats().Disk.Hits; hits != 1+cachedPages {
		t.Fatalf("the restarted disk served %d reads, want the segment and every page", hits)
	}
}

// A fault is never queued behind a pull. With the pull's first fetch of pages
// held in the store, a read of a page that fetch carries, and of every other
// page, is served at once from a request of its own.
func TestAReadIsNotQueuedBehindAPull(t *testing.T) {
	f := newPullFixture(t, 64<<20)
	f.objects.block.Store(true)
	pull := f.pull(t)
	<-f.objects.entered
	f.objects.block.Store(false)
	f.readAll(t)
	if stats := pull.Stats(); stats.Done || stats.Pulled == stats.Bytes {
		t.Fatalf("the pull reached %+v while its first fetch was held, want it still waiting", stats)
	}
	f.objects.release <- struct{}{}
	if err := pull.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.objects.gets.Store(0)
	f.readAll(t)
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("after the pull, reading the checkpoint made %d requests, want none", gets)
	}
}

// A pull runs behind the faults: while a read of the store is in flight for
// one, the pull makes no request of its own, and it carries on once that read
// is done.
func TestAPullWaitsWhileAFaultIsReadingTheStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPullFixture(t, 64<<20)
		f.objects.block.Store(true)
		read := make(chan struct{})
		go func() {
			defer close(read)
			readCachedPage(t, f.store, f.index, f.model, 2)
		}()
		<-f.objects.entered
		// The fault holds a request: its segment, then its page, which is held.
		before := f.objects.gets.Load()
		pull := f.pull(t)
		synctest.Wait()
		if gets := f.objects.gets.Load(); gets != before {
			t.Fatalf("the pull made %d requests while a fault was reading the store, want none", gets-before)
		}
		if stats := pull.Stats(); stats.Pulled != 0 {
			t.Fatalf("the pull reached %+v while a fault was reading the store, want nothing yet", stats)
		}
		f.objects.block.Store(false)
		f.objects.release <- struct{}{}
		<-read
		if err := pull.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if stats := pull.Stats(); stats.Pulled != stats.Bytes {
			t.Fatalf("the pull ended at %+v, want the whole checkpoint", stats)
		}
	})
}

// A checkpoint that does not fit in what the disk has left is refused whole,
// before anything is fetched, and so is a pull on a host that keeps no disk.
// Either way the checkpoint reads from the store as it always did.
func TestAPullThatDoesNotFitIsRefusedAndTheStoreServes(t *testing.T) {
	f := newPullFixture(t, 64<<10)
	if _, err := f.store.Pull(t.Context(), f.index); !errors.Is(err, checkpoint.ErrDiskFull) {
		t.Fatalf("a pull of four 2 MiB pages into a 64 KiB disk returned %v, want %v", err, checkpoint.ErrDiskFull)
	}
	if disk := f.cache.Stats().Disk; disk.UsedBytes != 0 || disk.LimitBytes != 64<<10 {
		t.Fatalf("the refused pull left the disk at %+v, want nothing used of 64 KiB", disk)
	}
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("the refused pull made %d requests, want none", gets)
	}
	f.readAll(t)
	// One request for the segment and one per page: the store serves it all.
	if gets := f.objects.gets.Load(); gets != 1+cachedPages {
		t.Fatalf("reading the checkpoint made %d requests, want %d", gets, 1+cachedPages)
	}

	none := newPullFixture(t, 0)
	if _, err := none.store.Pull(t.Context(), none.index); !errors.Is(err, checkpoint.ErrNoDisk) {
		t.Fatalf("a pull on a host that keeps no disk returned %v, want %v", err, checkpoint.ErrNoDisk)
	}
}

// A read of a pulled page whose caller gives up asks nothing of the store.
// The caller's leaving cancels the shared fetch, the disk's read of the page
// then fails for that, and a failed read of the disk is otherwise read from
// the store. On 2026-10-05 a give-back's compare on GCE made exactly that
// request of a pulled template page, after a prefetch cancelled under
// pressure had left the fetch it started.
func TestAReadGivenUpReadsNothingFromTheStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The disk takes a moment to read, so the caller can leave while it
		// does.
		f := newPullFixtureWith(t, 64<<20, cachedPages, []uint64{0, 1, 2, 3},
			func(_ *sim.Runtime, disk *sim.DiskConfig, _ *checkpoint.CacheConfig) {
				disk.ReadLatency = time.Millisecond
			})
		pull := f.pull(t)
		if err := pull.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.objects.gets.Store(0)
		ctx, cancel := context.WithCancel(f.ctx(t))
		read := make(chan error, 1)
		go func() {
			got := make([]byte, checkpoint.PageSize2MiB)
			read <- f.store.Read(ctx, f.index, "root", 2*checkpoint.PageSize2MiB, got)
		}()
		// The read is under way on the disk; its caller gives up.
		synctest.Wait()
		cancel()
		if err := <-read; !errors.Is(err, context.Canceled) {
			t.Fatalf("a read whose caller gave up returned %v, want %v", err, context.Canceled)
		}
		// The fetch its leaving cancelled runs on, and ends.
		synctest.Wait()
		if gets := f.objects.gets.Load(); gets != 0 {
			t.Fatalf("a read whose caller gave up made %d requests of the store, want none", gets)
		}
	})
}

// Losing the disk loses nothing: the store still holds everything a pull copied,
// so a copy the disk cannot give back, or gives back damaged, is read from the
// store instead, and the disk is not asked for it again.
func TestALostOrDamagedDiskFallsBackToTheStore(t *testing.T) {
	for _, loss := range []struct {
		name string
		lose func(t *testing.T, f *pullFixture)
	}{
		{"damaged", func(t *testing.T, f *pullFixture) {
			garbage := bytes.Repeat([]byte{0xa5}, int(f.cache.Stats().Disk.UsedBytes))
			if _, err := f.file.WriteAt(t.Context(), garbage, pullRegionBytes); err != nil {
				t.Fatal(err)
			}
		}},
		{"gone", func(t *testing.T, f *pullFixture) { f.disk.Fail() }},
	} {
		t.Run(loss.name, func(t *testing.T) {
			f := newPullFixture(t, 64<<20)
			pull := f.pull(t)
			if err := pull.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			loss.lose(t, f)
			f.objects.gets.Store(0)
			f.readAll(t)
			if gets := f.objects.gets.Load(); gets != 1+cachedPages {
				t.Fatalf("reading made %d requests, want the segment and every page from the store", gets)
			}
			if disk := f.cache.Stats().Disk; disk.Lost != 1+cachedPages || disk.Entries != 0 || disk.Hits != 0 {
				t.Fatalf("the disk reports %+v, want every copy lost and none served", disk)
			}
			// A copy the disk lost is not asked for again.
			f.objects.gets.Store(0)
			f.readAll(t)
			if gets := f.objects.gets.Load(); gets != cachedPages {
				t.Fatalf("reading again made %d requests, want one per page", gets)
			}
			if lost := f.cache.Stats().Disk.Lost; lost != 1+cachedPages {
				t.Fatalf("reading again lost %d copies in all, want no more than before", lost)
			}
		})
	}
}

// A newer checkpoint's page is a new identity, so the copy a pull made of the
// page it replaced is never read for it; the pages it did not change are still
// read from the disk.
func TestANewerCheckpointSupersedesThePulledCopy(t *testing.T) {
	f := newPullFixture(t, 64<<20)
	pull := f.pull(t)
	if err := pull.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := f.model.clone()
	p := f.store.Begin(f.index, control.Ref{VM: "pulled", Sequence: 3})
	for sector := range uint32(sectorsPerPage) {
		next.dirty(p, "root", 1, sector, sectorData("newer", 1, sector))
	}
	published, err := p.Commit(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := f.store.Open(t.Context(), published.Ref())
	if err != nil {
		t.Fatal(err)
	}
	f.objects.gets.Store(0)
	readCachedPage(t, f.store, newer, next, 1)
	// The newer checkpoint's page 1 comes from the store. Its segment does
	// not: the publication left the table it wrote in the cache's memory.
	if gets := f.objects.gets.Load(); gets != 1 {
		t.Fatalf("reading the newer checkpoint's page made %d requests, want its page alone", gets)
	}
	f.objects.gets.Store(0)
	for _, page := range []uint64{0, 2, 3} {
		readCachedPage(t, f.store, newer, next, page)
	}
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("reading the pages the newer checkpoint kept made %d requests, want none", gets)
	}
}

// A pull keeps what its VM publishes later. A newer checkpoint published with
// the pull's keep is read from the disk as the pulled one is: its segment and
// the page it wrote as well as the pages it kept, with no request of the store.
// Closing the pull frees nothing of either.
func TestAPullKeepsWhatItsVMPublishesLater(t *testing.T) {
	f := newPullFixture(t, 64<<20)
	pull := f.pull(t)
	if err := pull.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := f.model.clone()
	p := f.store.Begin(f.index, control.Ref{VM: "pulled", Sequence: 3})
	p.Keep(pull)
	for sector := range uint32(sectorsPerPage) {
		next.dirty(p, "root", 1, sector, sectorData("newer", 1, sector))
	}
	published, err := p.Commit(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if kept := pull.Stats().Kept; kept == 0 {
		t.Fatal("the pull kept nothing of the newer checkpoint")
	}
	newer, err := f.store.Open(t.Context(), published.Ref())
	if err != nil {
		t.Fatal(err)
	}
	f.objects.gets.Store(0)
	for range 2 {
		for page := range uint64(cachedPages) {
			readCachedPage(t, f.store, newer, next, page)
		}
	}
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("reading a checkpoint the pull kept made %d requests of the store, want none", gets)
	}
	pull.Close()
	// The pulled segment and four pages, and the newer segment and page 1.
	if entries := f.cache.Stats().Disk.Entries; entries != cachedPages+3 {
		t.Fatalf("the disk holds %d entries once the pull is closed, want %d", entries, cachedPages+3)
	}
	for page := range uint64(cachedPages) {
		readCachedPage(t, f.store, newer, next, page)
	}
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("reading after the pull closed made %d requests of the store, want none", gets)
	}
}

// Two pulls of one checkpoint share one copy: the second copies nothing. A
// pull holds nothing, so closing both frees nothing: the copy is ordinary disk
// entries, which leave only under pressure.
func TestPullsShareOneCopyAndClosingFreesNothing(t *testing.T) {
	f := newPullFixture(t, 64<<20)
	first := f.pull(t)
	if err := first.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	one := f.cache.Stats().Disk
	f.objects.gets.Store(0)
	second := f.pull(t)
	if err := second.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("a second pull of a copied checkpoint made %d requests, want none", gets)
	}
	if stats := second.Stats(); stats.Pulled != stats.Bytes {
		t.Fatalf("the second pull reached %+v, want the whole checkpoint", stats)
	}
	if two := f.cache.Stats().Disk; two.UsedBytes != one.UsedBytes || two.Entries != one.Entries {
		t.Fatalf("two pulls hold %+v and one held %+v, want the same copy", two, one)
	}
	first.Close()
	second.Close()
	if closed := f.cache.Stats().Disk; closed.UsedBytes != one.UsedBytes || closed.Entries != one.Entries {
		t.Fatalf("with both pulls closed the disk holds %+v, want what one pull left, %+v", closed, one)
	}
	f.readAll(t)
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("with both pulls closed, reading made %d requests, want none", gets)
	}
}

// A pull copies every segment of its checkpoint, not only the first. Pages 0
// and 256 of a volume of 2 MiB pages lie in two segments, and once the pull is
// complete both read without a request of the store.
func TestAPullCopiesEverySegment(t *testing.T) {
	f := newPullFixtureOf(t, 64<<20, 257, []uint64{0, 256})
	pull := f.pull(t)
	if err := pull.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := pull.Stats(); stats.Pulled != stats.Bytes || stats.Bytes == 0 {
		t.Fatalf("the pull ended at %+v, want every byte of both segments", stats)
	}
	// Two segments and two pages.
	if entries := f.cache.Stats().Disk.Entries; entries != 4 {
		t.Fatalf("the disk holds %d entries, want two segments and two pages", entries)
	}
	f.objects.gets.Store(0)
	f.readAll(t)
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("reading both segments' pages made %d requests of the store, want none", gets)
	}
}

// A pull whose read of the store fails stops short with that failure, not
// with pressure. Here every part of the checkpoint is gone: the pull reads the
// segment, and its read of the pages finds no part.
func TestAPullThatCannotReadTheStoreStopsShortWithTheFailure(t *testing.T) {
	f := newPullFixture(t, 64<<20)
	err := platform.ListAll(t.Context(), f.runtime.ObjectStore(), platform.ObjectPrefix{},
		func(object platform.ObjectMetadata) error {
			if !strings.Contains(object.Key.String(), "/part/") {
				return nil
			}
			return f.runtime.ObjectStore().Delete(t.Context(), platform.DeleteRequest{Key: object.Key})
		})
	if err != nil {
		t.Fatal(err)
	}
	pull, err := f.store.Pull(f.ctx(t), f.index)
	if err != nil {
		t.Fatal(err)
	}
	defer pull.Close()
	err = pull.Wait(t.Context())
	stats := pull.Stats()
	if !errors.Is(err, platform.ErrNotFound) || errors.Is(err, checkpoint.ErrPressure) || !stats.Done ||
		!errors.Is(stats.Err, platform.ErrNotFound) || stats.Fetched != stats.Pulled || stats.Pulled == stats.Bytes {
		t.Fatalf("a pull of a checkpoint whose parts are gone ended with %v at %+v, want the missing part after the segment alone", err,
			stats)
	}
}
