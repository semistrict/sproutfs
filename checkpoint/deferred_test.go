package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// cadence is how often a store writes an index object. The deferred index's
// tests run a store that defers it beside one that writes one every checkpoint,
// on the same workload.
type cadence struct {
	name  string
	every int
}

var cadences = []cadence{{name: "every checkpoint"}, {name: "every third", every: 3}}

// side is one cadence's store and the index each VM last published in it.
type side struct {
	cadence cadence
	objects platform.ObjectStore
	store   *checkpoint.Store
	index   map[string]*checkpoint.Index
}

func newSides(t *testing.T) []*side {
	t.Helper()
	sides := make([]*side, 0, len(cadences))
	for _, c := range cadences {
		objects := sim.New(sim.Config{}).ObjectStore()
		sides = append(sides, &side{cadence: c, objects: objects,
			store: mustStore(t, checkpoint.Config{ObjectStore: objects, IndexEvery: c.every}),
			index: make(map[string]*checkpoint.Index)})
	}
	return sides
}

// touched is every page a test has ever written into a model, by volume, which
// is what a check reads back rather than whole volumes of mostly zeroes.
type touched map[string]map[uint64]bool

func (t touched) add(volume string, page uint64) {
	if t[volume] == nil {
		t[volume] = make(map[uint64]bool)
	}
	t[volume][page] = true
}

// checkPages reads every touched page inside the volume back and compares it
// with the model, and requires every page Locate reports as holding bytes to
// be a touched one.
func checkPages(t *testing.T, store *checkpoint.Store, index *checkpoint.Index, m *model, pages touched) {
	t.Helper()
	for _, name := range index.Volumes() {
		if index.Size(name) != m.sizes[name] {
			t.Fatalf("%s %s: size %d, want %d", index.Ref(), name, index.Size(name), m.sizes[name])
		}
		size := m.pageSize[name]
		got := make([]byte, size)
		for _, page := range slices.Sorted(maps.Keys(pages[name])) {
			start := page * size
			if start >= m.sizes[name] {
				continue
			}
			buffer := got[:min(size, m.sizes[name]-start)]
			if err := store.Read(t.Context(), index, name, start, buffer); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buffer, m.contents[name][start:start+uint64(len(buffer))]) {
				t.Fatalf("%s %s page %d differs from the model", index.Ref(), name, page)
			}
		}
		extents, err := index.Locate(t.Context(), name, 0, index.Size(name))
		if err != nil {
			t.Fatal(err)
		}
		for _, extent := range extents {
			if !extent.Identity.Zero && !pages[name][extent.Offset/size] {
				t.Fatalf("%s %s locates page %d, which nothing wrote", index.Ref(), name, extent.Offset/size)
			}
		}
	}
}

// located is every volume's Locate of the whole volume, which is the page
// identity of every byte: two indexes that agree on it read from the same
// published pages.
func located(t *testing.T, index *checkpoint.Index) map[string][]control.Extent {
	t.Helper()
	all := make(map[string][]control.Extent)
	for _, name := range index.Volumes() {
		extents, err := index.Locate(t.Context(), name, 0, index.Size(name))
		if err != nil {
			t.Fatal(err)
		}
		all[name] = extents
	}
	return all
}

func indexBytes(t *testing.T, index *checkpoint.Index) []byte {
	t.Helper()
	encoded, err := checkpoint.IndexBytes(index)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestDeferredIndexMatchesAnIndexEveryCheckpoint publishes one random history
// in two stores at once, one writing an index object at every checkpoint and
// one at every third: overwrites of a hot set, which compaction then rewrites,
// pages zeroed whole, a volume that shrinks across a segment boundary, and a
// fork that goes on beside its parent under a pin. At every checkpoint both
// must read back the model, report the same identity for every page, pass the
// consistency check, and do all of it again after an open from storage alone,
// which for a deferred index rebuilds from the index object before it.
func TestDeferredIndexMatchesAnIndexEveryCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		random := rand.New(rand.NewPCG(0x15fd, 0x22))
		specs := map[string]checkpoint.VolumeSpec{
			// 20,480 pages of 4 KiB and 300 of 2 MiB: two segments each.
			"ram0": {Size: 80 << 20, PageSize: checkpoint.PageSize4KiB},
			"root": {Size: 600 << 20, PageSize: checkpoint.PageSize2MiB},
		}
		sides := newSides(t)
		for _, s := range sides {
			root, err := s.store.Root(t.Context(), control.Ref{VM: "vm-a", Sequence: 1}, specs)
			if err != nil {
				t.Fatal(err)
			}
			s.index["vm-a"] = root
		}
		models := map[string]*model{"vm-a": newModel(specs)}
		pages := map[string]touched{"vm-a": {}}
		sequence := map[string]uint64{"vm-a": 1}
		pinned := map[string][]uint64{}
		compacted := false
		for generation := range 20 {
			for _, vm := range slices.Sorted(maps.Keys(models)) {
				m := models[vm]
				sequence[vm]++
				ref := control.Ref{VM: vm, Sequence: sequence[vm]}
				publications := make([]*checkpoint.Publication, len(sides))
				for at, s := range sides {
					publications[at] = s.store.Begin(s.index[vm], ref)
				}
				if vm == "vm-a" && generation == 0 {
					// A page past the end the volume shrinks to below, at a sequence the
					// deferring store replays rather than indexes.
					copy(m.contents["root"][290*checkpoint.PageSize2MiB:], sectorData(ref.String(), 290, 0))
					pages[vm].add("root", 290)
					for _, p := range publications {
						p.Dirty("root", 290)
					}
				}
				if vm == "vm-a" && generation == 3 {
					// 520 MiB keeps part of the second segment, so the page that
					// straddles nothing but the segment does is cut from it.
					m.sizes = maps.Clone(m.sizes)
					m.sizes["root"] = 520 << 20
					m.contents["root"] = m.contents["root"][:520<<20]
					for _, p := range publications {
						p.SetSize("root", 520<<20)
					}
				}
				for range 1 + random.IntN(24) {
					name := []string{"ram0", "root"}[random.IntN(2)]
					count := m.sizes[name] / m.pageSize[name]
					page := uint64(random.IntN(int(count)))
					if random.IntN(10) < 7 {
						// The hot set: the first eight pages of each segment.
						segment := uint64(random.IntN(2)) * geometryOf(t, m.pageSize[name]).SegmentPages
						page = min(segment+uint64(random.IntN(8)), count-1)
					}
					start := page * m.pageSize[name]
					if random.IntN(6) == 0 {
						clear(m.contents[name][start : start+m.pageSize[name]])
					} else {
						sector := uint32(random.IntN(int(m.pageSize[name] / checkpoint.SectorSize)))
						copy(m.contents[name][start+uint64(sector)*checkpoint.SectorSize:],
							sectorData(ref.String(), page, sector))
					}
					pages[vm].add(name, page)
					for _, p := range publications {
						p.Dirty(name, page)
					}
				}
				for at, s := range sides {
					index, err := publications[at].Commit(t.Context(), m)
					if err != nil {
						t.Fatalf("%s: %s: %v", s.cadence.name, ref, err)
					}
					// A fork's first checkpoint replaces its parent's index, which is
					// the parent's to reclaim.
					if s.index[vm].Ref().VM == vm {
						if err := s.store.Reclaim(t.Context(), s.index[vm], index, pinned[vm]); err != nil {
							t.Fatalf("%s: reclaim at %s: %v", s.cadence.name, ref, err)
						}
					}
					s.index[vm] = index
					checkPages(t, s.store, index, m, pages[vm])
				}
				want := located(t, sides[0].index[vm])
				for _, s := range sides[1:] {
					if got := located(t, s.index[vm]); !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: the store writing an index %s locates other pages than one writing it %s",
							ref, s.cadence.name, sides[0].cadence.name)
					}
				}
				for _, s := range sides {
					// Only the store that defers lacks index objects, and only between
					// the ones it writes.
					wantIndex := s.cadence.every == 0 || !checkpoint.Deferred(s.index[vm])
					if got := present(t, s.objects, indexKey(t, vm, ref.Sequence)); got != wantIndex {
						t.Fatalf("%s: %s has an index object: %v, want %v", s.cadence.name, ref, got, wantIndex)
					}
					reopened, err := s.store.Open(t.Context(), ref)
					if err != nil {
						t.Fatalf("%s: open %s: %v", s.cadence.name, ref, err)
					}
					if got := located(t, reopened); !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: %s reopened locates other pages than it published", s.cadence.name, ref)
					}
					// The whole root, every segment's reads included, is the one it
					// published.
					if !bytes.Equal(indexBytes(t, reopened), indexBytes(t, s.index[vm])) {
						t.Fatalf("%s: %s reopened is another root than the one it published", s.cadence.name, ref)
					}
					if got, want := reopened.Checkpoints(), s.index[vm].Checkpoints(); !slices.Equal(got, want) {
						t.Fatalf("%s: %s reopened names %v, published %v", s.cadence.name, ref, got, want)
					}
					checkPages(t, s.store, reopened, m, pages[vm])
					if _, violations := s.store.CheckIndex(t.Context(), reopened); len(violations) != 0 {
						t.Fatalf("%s: %s: %v", s.cadence.name, ref, violations)
					}
				}
				// Compaction empties a checkpoint the hot set has overwritten, and
				// the index then reads nothing of it.
				if !slices.Contains(sides[0].index[vm].Checkpoints(), control.Ref{VM: vm, Sequence: 2}) {
					compacted = true
				}
			}
			if generation == 5 {
				pinned["vm-a"] = []uint64{sequence["vm-a"]}
				models["vm-b"] = models["vm-a"].clone()
				pages["vm-b"] = touched{}
				for name, held := range pages["vm-a"] {
					for page := range held {
						pages["vm-b"].add(name, page)
					}
				}
				sequence["vm-b"] = 0
				for _, s := range sides {
					s.index["vm-b"] = s.index["vm-a"]
				}
			}
		}
		if !compacted {
			t.Fatal("no checkpoint was compacted away, so the history exercised nothing of compaction")
		}
	})
}

// TestDeferredOpenSkipsAnUnselectedCheckpoint publishes a checkpoint that is
// never selected and then publishes its successor from the one before it, as a
// host does after a publication whose selection failed. An open of the
// successor must replay its own chain and leave the orphan out.
func TestDeferredOpenSkipsAnUnselectedCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, IndexEvery: 8})
		specs := volumes4KiB(map[string]uint64{"ram0": 1 << 20})
		root, err := store.Root(t.Context(), control.Ref{VM: "vm-a", Sequence: 1}, specs)
		if err != nil {
			t.Fatal(err)
		}
		selected := newModel(specs)
		publish := func(parent *checkpoint.Index, sequence uint64, m *model, tag string) *checkpoint.Index {
			t.Helper()
			p := store.Begin(parent, control.Ref{VM: "vm-a", Sequence: sequence})
			m.dirty(p, "ram0", 3, 0, sectorData(tag, 3, 0))
			index, err := p.Commit(t.Context(), m)
			if err != nil {
				t.Fatal(err)
			}
			return index
		}
		second := publish(root, 2, selected, "second")
		orphan := selected.clone()
		publish(second, 3, orphan, "orphan")
		fourth := publish(second, 4, selected, "fourth")
		reopened, err := store.Open(t.Context(), fourth.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, reopened, selected)
		if slices.Contains(reopened.Checkpoints(), control.Ref{VM: "vm-a", Sequence: 3}) {
			t.Fatalf("the open replayed the unselected checkpoint: it names %v", reopened.Checkpoints())
		}
	})
}

// TestDeferredOpenOfAbsentCheckpoint reports a checkpoint with neither an
// index object nor parts as not found.
func TestDeferredOpenOfAbsentCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, IndexEvery: 8})
		_, err := store.Open(t.Context(), control.Ref{VM: "vm-a", Sequence: 4})
		if !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("open of a VM with no objects: %v, want not found", err)
		}
	})
}

// TestDeferredOpenReadsATableLongerThanTheTail replays a checkpoint whose part
// table is longer than the tail an open reads first, which costs the open a
// second read of the rest of it.
func TestDeferredOpenReadsATableLongerThanTheTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, IndexEvery: 8})
		specs := volumes4KiB(map[string]uint64{"ram0": 64 << 20})
		root, err := store.Root(t.Context(), control.Ref{VM: "vm-a", Sequence: 1}, specs)
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(specs)
		p := store.Begin(root, control.Ref{VM: "vm-a", Sequence: 2})
		// About 30 bytes of table a page: 4,000 pages is well past 64 KiB.
		for page := range uint64(4000) {
			m.dirty(p, "ram0", page*3, 0, sectorData("long", page*3, 0))
		}
		published, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := store.Open(t.Context(), published.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, reopened, m)
		if !bytes.Equal(indexBytes(t, reopened), indexBytes(t, published)) {
			t.Fatal("the reopened root is not the one published")
		}
	})
}

// partFault fails every put of a first part while failing is set, after a
// delay, so the parts after it are uploaded first.
type partFault struct {
	*sim.ObjectStore
	failing atomic.Bool
}

func (s *partFault) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if s.failing.Load() && strings.HasSuffix(request.Key.String(), "/part/0") {
		time.Sleep(time.Second)
		return platform.PutResult{}, errors.New("injected: the first part does not land")
	}
	return s.ObjectStore.Put(ctx, request)
}

// TestDeferredCommitWaitsForEveryEarlierPart fails the upload of one part of a
// checkpoint that defers its index object. Its last part is its commit, so it
// must never be written while an earlier one is not durable: the checkpoint is
// absent, its parent still opens, and a retry publishes it whole.
func TestDeferredCommitWaitsForEveryEarlierPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := &partFault{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
		// One page to a part, so the checkpoint below has three.
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, IndexEvery: 4, PartBytes: 1})
		sizes := map[string]uint64{"root": 3 * checkpoint.PageSize2MiB}
		root, err := store.Root(t.Context(), control.Ref{VM: "torn", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(volumes2MiB(sizes))
		first := store.Begin(root, control.Ref{VM: "torn", Sequence: 2})
		m.dirty(first, "root", 0, 0, sectorData("first", 0, 0))
		parent, err := first.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if !checkpoint.Deferred(parent) {
			t.Fatal("the checkpoint after the root wrote an index object")
		}
		child := m.clone()
		publish := func() (*checkpoint.Index, error) {
			p := store.Begin(parent, control.Ref{VM: "torn", Sequence: 3})
			for page := range uint64(3) {
				child.dirty(p, "root", page, 1, sectorData("child", page, 1))
			}
			return p.Commit(t.Context(), child)
		}
		objects.failing.Store(true)
		if _, err := publish(); err == nil {
			t.Fatal("a commit whose first part failed succeeded")
		}
		if present(t, objects, partKey(t, "torn", 3, 2)) {
			t.Fatal("the last part was written while an earlier one had failed")
		}
		if _, err := store.Open(t.Context(), control.Ref{VM: "torn", Sequence: 3}); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("open of the failed checkpoint: %v, want not found", err)
		}
		reopened, err := store.Open(t.Context(), parent.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, reopened, m)
		objects.failing.Store(false)
		published, err := publish()
		if err != nil {
			t.Fatal(err)
		}
		reopened, err = store.Open(t.Context(), published.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, reopened, child)
	})
}

// TestIndexObjectHoldsEveryPendingSegment defers two checkpoints, each changing
// a different segment, and then writes an index object. That object alone
// must locate everything: its root addresses every segment the deferred ones
// left pending, and an open of it reads no part table.
func TestIndexObjectHoldsEveryPendingSegment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, IndexEvery: 3})
		// Three segments of 4 KiB pages.
		specs := volumes4KiB(map[string]uint64{"ram0": 192 << 20})
		segment := geometryOf(t, checkpoint.PageSize4KiB).SegmentPages
		index, err := store.Root(t.Context(), control.Ref{VM: "vm-a", Sequence: 1}, specs)
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(specs)
		for sequence := uint64(2); sequence <= 4; sequence++ {
			p := store.Begin(index, control.Ref{VM: "vm-a", Sequence: sequence})
			m.dirty(p, "ram0", (sequence-2)*segment+5, 0, sectorData("pending", sequence, 0))
			if index, err = p.Commit(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			if deferred := checkpoint.Deferred(index); deferred != (sequence < 4) {
				t.Fatalf("checkpoint %d deferred its index object: %v", sequence, deferred)
			}
		}
		reopened, err := store.Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, reopened, m)
		if got := reopened.Checkpoints(); slices.Contains(got, control.Ref{VM: "vm-a", Sequence: 1}) {
			t.Fatalf("the index object still names the base the deferred ones replayed: %v", got)
		}
	})
}

// TestPullOfADeferredIndex pulls a checkpoint that deferred its index object:
// its pending segments are in no object, so the pull copies their pages from
// what the index holds and stops complete.
func TestPullOfADeferredIndex(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		cache, err := checkpoint.NewCache(budget, checkpoint.CacheConfig{Disk: file, DiskBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cache.Close)
		store := mustStore(t, checkpoint.Config{ObjectStore: runtime.ObjectStore(), Cache: cache, IndexEvery: 4})
		sizes := map[string]uint64{"root": 4 * checkpoint.PageSize2MiB}
		root, err := store.Root(t.Context(), control.Ref{VM: "pulled", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(volumes2MiB(sizes))
		p := store.Begin(root, control.Ref{VM: "pulled", Sequence: 2})
		for page := range uint64(4) {
			m.dirty(p, "root", page, 0, sectorData("pulled", page, 0))
		}
		published, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		index, err := store.Open(t.Context(), published.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if !checkpoint.Deferred(index) {
			t.Fatal("the checkpoint wrote an index object")
		}
		pull, err := store.Pull(t.Context(), index)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pull.Close)
		if err := pull.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if stats := pull.Stats(); stats.Pulled != stats.Bytes || stats.Bytes == 0 {
			t.Fatalf("pulled %d of %d bytes", stats.Pulled, stats.Bytes)
		}
		checkRead(t, store, index, m)
	})
}

// TestDeferredIndexKeepsTheSegmentsItsBaseAddresses overwrites, in a
// checkpoint that defers its index object, the only page of a segment its base
// addresses in an older checkpoint's index object, and reclaims. An open
// rebuilds that segment from the base's copy of it, so the older index object
// must survive the sweep although nothing reads its pages any more.
func TestDeferredIndexKeepsTheSegmentsItsBaseAddresses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, IndexEvery: 2})
		specs := volumes4KiB(map[string]uint64{"ram0": 128 << 20})
		segment := geometryOf(t, checkpoint.PageSize4KiB).SegmentPages
		index, err := store.Root(t.Context(), control.Ref{VM: "vm-a", Sequence: 1}, specs)
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(specs)
		// 2 defers, 3 writes the index object holding the first segment, 4
		// defers, 5 writes one holding only the second, and 6 defers and
		// overwrites the first segment's only page.
		pages := map[uint64]uint64{2: segment + 1, 3: 7, 4: segment + 2, 5: segment + 3, 6: 7}
		for sequence := uint64(2); sequence <= 6; sequence++ {
			p := store.Begin(index, control.Ref{VM: "vm-a", Sequence: sequence})
			m.dirty(p, "ram0", pages[sequence], 0, sectorData("base", sequence, 0))
			next, err := p.Commit(t.Context(), m)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Reclaim(t.Context(), index, next, nil); err != nil {
				t.Fatal(err)
			}
			index = next
		}
		if !checkpoint.Deferred(index) || present(t, objects, indexKey(t, "vm-a", 6)) {
			t.Fatal("checkpoint 6 wrote an index object")
		}
		reopened, err := mustStore(t, checkpoint.Config{ObjectStore: objects}).Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, reopened, m)
	})
}
