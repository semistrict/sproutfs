package checkpoint_test

import (
	"bytes"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

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
}

func newPullFixture(t *testing.T, diskBytes int64) *pullFixture {
	t.Helper()
	runtime := sim.New(sim.Config{})
	disk := runtime.NewDisk("host", sim.DiskConfig{})
	file, err := disk.Open(t.Context(), "cache", platform.OpenOptions{Create: true, Truncate: true})
	if err != nil {
		t.Fatal(err)
	}
	budget, err := resource.New(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(budget, checkpoint.CacheConfig{Disk: file, DiskBytes: diskBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	objects := &cacheStore{ObjectStore: runtime.ObjectStore(), suspended: "/part/",
		entered: make(chan struct{}, 64), release: make(chan struct{})}
	store := mustStore(t, checkpoint.Config{ObjectStore: objects, Cache: cache})
	sizes := map[string]uint64{"root": cachedPages * checkpoint.PageSize2MiB}
	root, err := store.Root(t.Context(), control.Ref{VM: "pulled", Sequence: 1}, volumes2MiB(sizes))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(volumes2MiB(sizes))
	p := store.Begin(root, control.Ref{VM: "pulled", Sequence: 2})
	for page := range uint64(cachedPages) {
		for sector := range uint32(sectorsPerPage) {
			m.dirty(p, "root", page, sector, sectorData("pulled", page, sector))
		}
	}
	published, err := p.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.Open(t.Context(), published.Ref())
	if err != nil {
		t.Fatal(err)
	}
	objects.gets.Store(0)
	return &pullFixture{store: store, index: index, model: m, cache: cache, objects: objects, disk: disk, file: file}
}

// pull begins a pull of the fixture's checkpoint, closed with the test.
func (f *pullFixture) pull(t *testing.T) *checkpoint.Pull {
	t.Helper()
	pull, err := f.store.Pull(t.Context(), f.index)
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
	for page := range uint64(cachedPages) {
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
	// second the four pages again: the index keeps the segment it decoded.
	if disk := f.cache.Stats().Disk; disk.Hits != 9 || disk.Lost != 0 || disk.Entries != cachedPages+1 {
		t.Fatalf("the disk served %+v, want nine hits of five entries", disk)
	}
	// The four members lie next to each other in one part, so the pull fetched
	// them as one extent.
	if fetched != 2 {
		t.Fatalf("the pull made %d requests, want the segment and the one extent", fetched)
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
			if _, err := f.file.WriteAt(t.Context(), garbage, 0); err != nil {
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
	// The newer checkpoint's segment and its page 1 come from the store.
	if gets := f.objects.gets.Load(); gets != 2 {
		t.Fatalf("reading the newer checkpoint's page made %d requests, want its segment and its page", gets)
	}
	f.objects.gets.Store(0)
	for _, page := range []uint64{0, 2, 3} {
		readCachedPage(t, f.store, newer, next, page)
	}
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("reading the pages the newer checkpoint kept made %d requests, want none", gets)
	}
}

// Two pulls of one checkpoint share one copy: the second copies nothing and
// gives back the space it took for it. The copy stays while either holds it and
// goes with the last.
func TestPullsShareOneCopyUntilTheLastLetsGo(t *testing.T) {
	f := newPullFixture(t, 64<<20)
	first, err := f.store.Pull(t.Context(), f.index)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	one := f.cache.Stats().Disk
	f.objects.gets.Store(0)
	second, err := f.store.Pull(t.Context(), f.index)
	if err != nil {
		t.Fatal(err)
	}
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
	f.readAll(t)
	if gets := f.objects.gets.Load(); gets != 0 {
		t.Fatalf("with the second pull still holding the copy, reading made %d requests, want none", gets)
	}
	second.Close()
	if disk := f.cache.Stats().Disk; disk.UsedBytes != 0 || disk.Entries != 0 {
		t.Fatalf("with both pulls closed the disk holds %+v, want nothing", disk)
	}
	f.readAll(t)
	if gets := f.objects.gets.Load(); gets != cachedPages {
		t.Fatalf("with no pull left, reading made %d requests, want one per page", gets)
	}
}
