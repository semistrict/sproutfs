package checkpoint_test

import (
	"bytes"
	"errors"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// layout is one of the two ways a checkpoint's page table can be stored. The
// log layout's tests run it beside the index layout on the same workload.
type layout struct {
	name string
	log  *checkpoint.LogLayout
}

var layouts = []layout{{name: "index"}, {name: "log", log: &checkpoint.LogLayout{MapEvery: 3}}}

// side is one layout's store and the index each VM last published in it.
type side struct {
	layout  layout
	objects platform.ObjectStore
	store   *checkpoint.Store
	index   map[string]*checkpoint.Index
}

func newSides(t *testing.T) []*side {
	t.Helper()
	sides := make([]*side, 0, len(layouts))
	for _, l := range layouts {
		objects := sim.New(sim.Config{}).ObjectStore()
		sides = append(sides, &side{layout: l, objects: objects,
			store: mustStore(t, checkpoint.Config{ObjectStore: objects, Log: l.log}),
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

// TestLogLayoutMatchesIndexLayout publishes one random history in both layouts
// at once: overwrites of a hot set, which compaction then rewrites, pages zeroed
// whole, a volume that shrinks across a segment boundary, and a fork that goes
// on beside its parent under a pin. At every checkpoint both layouts must read
// back the model, report the same identity for every page, and do both again
// after an open from storage alone, which in the log layout replays from a map
// object through up to MapEvery log records.
func TestLogLayoutMatchesIndexLayout(t *testing.T) {
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
					// log layout replays rather than maps.
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
						t.Fatalf("%s: %s: %v", s.layout.name, ref, err)
					}
					// A fork's first checkpoint replaces its parent's index, which is
					// the parent's to reclaim.
					if s.index[vm].Ref().VM == vm {
						if err := s.store.Reclaim(t.Context(), s.index[vm], index, pinned[vm]); err != nil {
							t.Fatalf("%s: reclaim at %s: %v", s.layout.name, ref, err)
						}
					}
					s.index[vm] = index
					checkPages(t, s.store, index, m, pages[vm])
				}
				want := located(t, sides[0].index[vm])
				for _, s := range sides[1:] {
					if got := located(t, s.index[vm]); !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: the %s layout locates other pages than the index layout", ref, s.layout.name)
					}
					if got, want := s.index[vm].Checkpoints(), sides[0].index[vm].Checkpoints(); !slices.Equal(got, want) {
						t.Fatalf("%s: the %s layout names %v, the index layout %v", ref, s.layout.name, got, want)
					}
				}
				for _, s := range sides {
					reopened, err := s.store.Open(t.Context(), ref)
					if err != nil {
						t.Fatalf("%s: open %s: %v", s.layout.name, ref, err)
					}
					if got := located(t, reopened); !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: %s reopened locates other pages than it published", s.layout.name, ref)
					}
					// The whole root, every segment's reads included, is the one it
					// published.
					if !bytes.Equal(indexBytes(t, reopened), indexBytes(t, s.index[vm])) {
						t.Fatalf("%s: %s reopened is another root than the one it published", s.layout.name, ref)
					}
					checkPages(t, s.store, reopened, m, pages[vm])
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
		// The log layout never writes an index object.
		log := sides[1]
		if present(t, log.objects, indexKey(t, "vm-a", sequence["vm-a"])) {
			t.Fatal("the log layout wrote an index object")
		}
	})
}

// TestLogOpenSkipsAnUnselectedCheckpoint publishes a checkpoint that is never
// selected and then publishes its successor from the one before it, as a host
// does after a publication whose selection failed. An open of the successor
// must replay along its parent chain and leave the orphan out.
func TestLogOpenSkipsAnUnselectedCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, Log: &checkpoint.LogLayout{MapEvery: 8}})
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

// TestLogOpenOfAbsentCheckpoint reports a checkpoint no map object precedes as
// not found, which is what an open of an index layout checkpoint whose index
// object is missing reports.
func TestLogOpenOfAbsentCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, Log: &checkpoint.LogLayout{}})
		_, err := store.Open(t.Context(), control.Ref{VM: "vm-a", Sequence: 4})
		if !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("open of a VM with no objects: %v, want not found", err)
		}
	})
}

// TestLogOpenReadsATableLongerThanTheTail replays a checkpoint whose part
// table is longer than the tail an open reads first, which costs the open a
// second read of the rest of it.
func TestLogOpenReadsATableLongerThanTheTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, Log: &checkpoint.LogLayout{MapEvery: 8}})
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
