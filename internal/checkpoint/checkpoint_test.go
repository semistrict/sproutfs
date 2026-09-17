package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// testPrefix is the object prefix every checkpoint fixture publishes under.
func testPrefix(t testing.TB) platform.ObjectPrefix {
	t.Helper()
	prefix, err := platform.NewObjectPrefix("deployment")
	if err != nil {
		t.Fatal(err)
	}
	return prefix
}

func mustStore(t testing.TB, config checkpoint.Config) *checkpoint.Store {
	t.Helper()
	if config.ObjectPrefix.String() == "" {
		config.ObjectPrefix = testPrefix(t)
	}
	store, err := checkpoint.NewStore(config)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// sectorsPerPage is how many 4 KiB sectors one published page holds.
const sectorsPerPage = checkpoint.PageSize / checkpoint.SectorSize

// objectKey names one checkpoint object the way the store does, so a test can
// assert which objects a publication wrote.
func objectKey(t testing.TB, parts ...string) platform.ObjectKey {
	t.Helper()
	key, err := platform.NewObjectKey("deployment/" + parts[0])
	for _, part := range parts[1:] {
		key, err = platform.NewObjectKey(key.String() + "/" + part)
	}
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// partKey names one part of a checkpoint's data plane by number the way the
// store does.
func partKey(t testing.TB, vm string, sequence uint64, number uint32) platform.ObjectKey {
	t.Helper()
	return objectKey(t, "vm", vm, "ckpt", strconv.FormatUint(sequence, 10), "part", strconv.FormatUint(uint64(number), 10))
}

// indexKey names one checkpoint's index object, which holds the segments it
// changed and its root: while it is there the checkpoint is published.
func indexKey(t testing.TB, vm string, sequence uint64) platform.ObjectKey {
	t.Helper()
	return objectKey(t, "vm", vm, "ckpt", strconv.FormatUint(sequence, 10), "index")
}

func present(t *testing.T, objects platform.ObjectStore, key platform.ObjectKey) bool {
	t.Helper()
	_, err := objects.Head(t.Context(), key)
	if err == nil {
		return true
	}
	if !errors.Is(err, platform.ErrNotFound) {
		t.Fatal(err)
	}
	return false
}

// model is the byte-exact expectation for one VM's volumes, and the Source a
// publication reads whole pages from.
type model struct {
	sizes    map[string]uint64
	contents map[string][]byte
}

func newModel(sizes map[string]uint64) *model {
	m := &model{sizes: sizes, contents: make(map[string][]byte, len(sizes))}
	for name, size := range sizes {
		m.contents[name] = make([]byte, size)
	}
	return m
}

func (m *model) clone() *model {
	next := &model{sizes: m.sizes, contents: make(map[string][]byte, len(m.contents))}
	for name, data := range m.contents {
		next.contents[name] = slices.Clone(data)
	}
	return next
}

func (m *model) ReadPage(_ context.Context, volume string, page uint64, dst []byte) error {
	copy(dst, m.contents[volume][page*checkpoint.PageSize:])
	return nil
}

// dirty writes one sector into the model and marks its page changed.
func (m *model) dirty(p *checkpoint.Publication, volume string, page uint64, sector uint32, data []byte) {
	copy(m.contents[volume][page*checkpoint.PageSize+uint64(sector)*checkpoint.SectorSize:], data)
	p.Dirty(volume, page)
}

// zero clears one sector in the model and marks its page changed.
func (m *model) zero(p *checkpoint.Publication, volume string, page uint64, sector uint32) {
	start := page*checkpoint.PageSize + uint64(sector)*checkpoint.SectorSize
	clear(m.contents[volume][start : start+checkpoint.SectorSize])
	p.Dirty(volume, page)
}

// sectorData is deterministic sector content that no other sector repeats.
func sectorData(tag string, page uint64, sector uint32) []byte {
	data := make([]byte, checkpoint.SectorSize)
	seed := fmt.Sprintf("%s/%d/%d/", tag, page, sector)
	for index := range data {
		data[index] = seed[index%len(seed)] ^ byte(index)
	}
	return data
}

func checkRead(t *testing.T, store *checkpoint.Store, index *checkpoint.Index, m *model) {
	t.Helper()
	for _, name := range index.Volumes() {
		if index.Size(name) != m.sizes[name] {
			t.Fatalf("volume %s: size %d, want %d", name, index.Size(name), m.sizes[name])
		}
		got := make([]byte, index.Size(name))
		if err := store.Read(t.Context(), index, name, 0, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, m.contents[name]) {
			t.Fatalf("volume %s at %s differs from the model", name, index.Ref())
		}
	}
}

// checkPartialRead exercises the unaligned read path against the same model.
func checkPartialRead(t *testing.T, store *checkpoint.Store, index *checkpoint.Index, m *model, volume string, offset, length uint64) {
	t.Helper()
	got := make([]byte, length)
	if err := store.Read(t.Context(), index, volume, offset, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, m.contents[volume][offset:offset+length]) {
		t.Fatalf("volume %s [%d,%d) differs from the model", volume, offset, offset+length)
	}
}

// line is one VM's publication history against its byte model.
type line struct {
	vm       string
	sequence uint64
	index    *checkpoint.Index
	model    *model
}

// publish writes a random dirty set, commits it, and returns the new index.
func (l *line) publish(t *testing.T, store *checkpoint.Store, random *rand.Rand) {
	t.Helper()
	l.sequence++
	p := store.Begin(l.index, control.Ref{VM: l.vm, Sequence: l.sequence})
	names := l.index.Volumes()
	for range 1 + random.IntN(64) {
		volume := names[random.IntN(len(names))]
		sectors := l.model.sizes[volume] / checkpoint.SectorSize
		absolute := uint64(random.IntN(int(sectors)))
		page, sector := absolute/sectorsPerPage, uint32(absolute%sectorsPerPage)
		if random.IntN(8) == 0 {
			l.model.zero(p, volume, page, sector)
			continue
		}
		l.model.dirty(p, volume, page, sector, sectorData(l.vm+strconv.FormatUint(l.sequence, 10), page, sector))
	}
	index, err := p.Commit(t.Context(), l.model)
	if err != nil {
		t.Fatal(err)
	}
	if index.Ref() != (control.Ref{VM: l.vm, Sequence: l.sequence}) {
		t.Fatalf("published %s, want %s/%d", index.Ref(), l.vm, l.sequence)
	}
	l.index = index
}

func TestPublicationTracksByteModelAcrossGenerationsAndForks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		random := rand.New(rand.NewPCG(0x5eed, 0x1b))
		objects := sim.New(sim.Config{Seed: 7}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, Concurrency: 4})
		sizes := map[string]uint64{"root": 2*checkpoint.PageSize + 8*checkpoint.SectorSize, "swap": checkpoint.PageSize}
		root, err := store.Root(t.Context(), control.Ref{VM: "vm-a", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		source := &line{vm: "vm-a", sequence: 1, index: root, model: newModel(sizes)}
		checkRead(t, store, source.index, source.model)

		var fork *line
		var origin control.Ref
		for generation := range 6 {
			source.publish(t, store, random)
			checkRead(t, store, source.index, source.model)
			checkPartialRead(t, store, source.index, source.model, "root", checkpoint.SectorSize+17, 3*checkpoint.SectorSize+5)
			if generation == 2 {
				fork = &line{vm: "vm-b", index: source.index, model: source.model.clone()}
				origin = source.index.Ref()
			}
			if fork != nil {
				fork.publish(t, store, random)
				checkRead(t, store, fork.index, fork.model)
				checkRead(t, store, source.index, source.model)
			}
		}
		// Every generation must also be readable from storage alone.
		for _, published := range []*line{source, fork} {
			reopened, err := store.Open(t.Context(), published.index.Ref())
			if err != nil {
				t.Fatal(err)
			}
			checkRead(t, store, reopened, published.model)
		}
		// The fork goes on reading its source's checkpoints, which is what makes
		// a fork cost nothing, and names its own alongside them.
		named := fork.index.Checkpoints()
		if !slices.Contains(named, origin) || !slices.Contains(named, fork.index.Ref()) {
			t.Fatalf("the fork names %v, neither its origin %v nor itself", named, origin)
		}
	})
}

func TestLocateSharesIdentityAcrossForkUntilWritten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		sizes := map[string]uint64{"root": 2 * checkpoint.PageSize}
		root, err := store.Root(t.Context(), control.Ref{VM: "vm-a", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(sizes)
		base := store.Begin(root, control.Ref{VM: "vm-a", Sequence: 2})
		for sector := range uint32(sectorsPerPage) {
			m.dirty(base, "root", 0, sector, sectorData("base", 0, sector))
		}
		parent, err := base.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}

		forkModel := m.clone()
		child := store.Begin(parent, control.Ref{VM: "vm-b", Sequence: 1})
		forkModel.dirty(child, "root", 0, 5, sectorData("fork", 0, 5))
		fork, err := child.Commit(t.Context(), forkModel)
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, fork, forkModel)
		checkRead(t, store, parent, m)

		inherited := control.Identity{Ref: control.Ref{VM: "vm-a", Sequence: 2}, Volume: "root", Page: 0}
		written := control.Identity{Ref: control.Ref{VM: "vm-b", Sequence: 1}, Volume: "root", Page: 0}
		parentExtents, err := parent.Locate(t.Context(), "root", 0, checkpoint.PageSize)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(parentExtents, []control.Extent{{Offset: 0, Length: checkpoint.PageSize, Identity: inherited}}) {
			t.Fatalf("parent extents %+v", parentExtents)
		}
		forkExtents, err := fork.Locate(t.Context(), "root", 0, checkpoint.PageSize)
		if err != nil {
			t.Fatal(err)
		}
		// One sector written republishes the whole 2 MiB page under the
		// fork's own reference: there is no sub-page identity to inherit.
		want := []control.Extent{{Offset: 0, Length: checkpoint.PageSize, Identity: written}}
		if !slices.Equal(forkExtents, want) {
			t.Fatalf("fork extents %+v", forkExtents)
		}

		// A page no checkpoint has written is one zero extent in both indexes.
		zero := []control.Extent{{Offset: checkpoint.PageSize, Length: checkpoint.PageSize, Identity: control.Identity{Zero: true}}}
		for _, index := range []*checkpoint.Index{parent, fork} {
			extents, err := index.Locate(t.Context(), "root", checkpoint.PageSize, checkpoint.PageSize)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(extents, zero) {
				t.Fatalf("%s hole extents %+v", index.Ref(), extents)
			}
		}
		if _, err := fork.Locate(t.Context(), "swap", 0, 1); !errors.Is(err, checkpoint.ErrUnknownVolume) {
			t.Fatalf("locate on unknown volume: %v", err)
		}
		if _, err := fork.Locate(t.Context(), "root", 2*checkpoint.PageSize, 1); !errors.Is(err, checkpoint.ErrInvalidRange) {
			t.Fatalf("locate past end: %v", err)
		}
	})
}

func TestZeroingEverySectorDropsThePage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		sizes := map[string]uint64{"root": checkpoint.PageSize}
		root, err := store.Root(t.Context(), control.Ref{VM: "hole", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(sizes)
		filled := store.Begin(root, control.Ref{VM: "hole", Sequence: 2})
		for sector := range uint32(sectorsPerPage) {
			m.dirty(filled, "root", 0, sector, sectorData("hole", 0, sector))
		}
		written, err := filled.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		discard := store.Begin(written, control.Ref{VM: "hole", Sequence: 3})
		for sector := range uint32(sectorsPerPage) {
			m.zero(discard, "root", 0, sector)
		}
		emptied, err := discard.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, emptied, m)
		// The page's bytes are not written and nothing says it left but the
		// segment that no longer names it — and that segment held nothing else,
		// so it is gone too. The checkpoint the page lived in is no longer read,
		// so the only checkpoint this index names is its own.
		if !present(t, objects, indexKey(t, "hole", 3)) {
			t.Fatal("a checkpoint that dropped a page recorded nothing")
		}
		own := []control.Ref{{VM: "hole", Sequence: 3}}
		if !slices.Equal(emptied.Checkpoints(), own) {
			t.Fatalf("a checkpoint that dropped its only page names checkpoints %v, want %v",
				emptied.Checkpoints(), own)
		}
		extents, err := emptied.Locate(t.Context(), "root", 0, checkpoint.PageSize)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(extents, []control.Extent{{Offset: 0, Length: checkpoint.PageSize, Identity: control.Identity{Zero: true}}}) {
			t.Fatalf("zeroed page extents %+v", extents)
		}
	})
}

func TestResizingAVolumeKeepsInheritedPagesReadable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		// A tail page of eight sectors, so resizing changes a page's length.
		sizes := map[string]uint64{"root": checkpoint.PageSize + 8*checkpoint.SectorSize}
		root, err := store.Root(t.Context(), control.Ref{VM: "resize", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(sizes)
		p := store.Begin(root, control.Ref{VM: "resize", Sequence: 2})
		for sector := range uint32(8) {
			m.dirty(p, "root", 1, sector, sectorData("resize", 1, sector))
		}
		written, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, written, m)

		// Growing the tail page exposes zeroes past the object it inherited.
		grown := map[string]uint64{"root": 2 * checkpoint.PageSize}
		wider := store.Begin(written, control.Ref{VM: "resize", Sequence: 3})
		wider.SetSize("root", grown["root"])
		grownModel := newModel(grown)
		copy(grownModel.contents["root"], m.contents["root"])
		index, err := wider.Commit(t.Context(), grownModel)
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, index, grownModel)

		// Shrinking below the tail page drops it entirely.
		shrunk := map[string]uint64{"root": checkpoint.PageSize}
		narrower := store.Begin(index, control.Ref{VM: "resize", Sequence: 4})
		narrower.SetSize("root", shrunk["root"])
		shrunkModel := newModel(shrunk)
		copy(shrunkModel.contents["root"], m.contents["root"])
		small, err := narrower.Commit(t.Context(), shrunkModel)
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, small, shrunkModel)
		reopened, err := store.Open(t.Context(), small.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, reopened, shrunkModel)
	})
}

func TestStateIsPublishedPerCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		sizes := map[string]uint64{"root": checkpoint.PageSize}
		root, err := store.Root(t.Context(), control.Ref{VM: "vmm", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadState(t.Context(), root); !errors.Is(err, checkpoint.ErrNoState) {
			t.Fatalf("state of a root index: %v", err)
		}
		m := newModel(sizes)
		p := store.Begin(root, control.Ref{VM: "vmm", Sequence: 2})
		p.SetState([]byte("registers and devices"))
		index, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if !index.HasState() {
			t.Fatal("committed state is not reported")
		}
		reopened, err := store.Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		state, err := store.ReadState(t.Context(), reopened)
		if err != nil {
			t.Fatal(err)
		}
		if string(state) != "registers and devices" {
			t.Fatalf("state %q", state)
		}
		// A later checkpoint publishes its own state over the old one.
		next := store.Begin(index, control.Ref{VM: "vmm", Sequence: 3})
		next.SetState([]byte("later registers"))
		m.dirty(next, "root", 0, 0, sectorData("vmm", 0, 0))
		later, err := next.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		state, err = store.ReadState(t.Context(), later)
		if err != nil {
			t.Fatal(err)
		}
		if string(state) != "later registers" {
			t.Fatalf("state of the later checkpoint %q", state)
		}
	})
}

// faultStore injects one object-store fault at a chosen put ordinal.
type faultStore struct {
	*sim.ObjectStore
	mu         sync.Mutex
	puts       int
	failAt     int
	afterApply bool
}

func (s *faultStore) arm(failAt int, afterApply bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts, s.failAt, s.afterApply = 0, failAt, afterApply
}

func (s *faultStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func (s *faultStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	s.mu.Lock()
	s.puts++
	if s.puts == s.failAt {
		if s.afterApply {
			s.ObjectStore.FailNextAfterApply(sim.ObjectPut, 1)
		} else {
			s.ObjectStore.FailNext(sim.ObjectPut, 1)
		}
	}
	s.mu.Unlock()
	return s.ObjectStore.Put(ctx, request)
}

// interruptedCommit publishes a parent, then commits a child whose put number
// failAt fails, retries it, and returns how many objects the child uploaded.
func interruptedCommit(t *testing.T, failAt int, afterApply bool) int {
	t.Helper()
	objects := &faultStore{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
	store := mustStore(t, checkpoint.Config{ObjectStore: objects, Concurrency: 1})
	sizes := map[string]uint64{"root": 3 * checkpoint.PageSize}
	root, err := store.Root(t.Context(), control.Ref{VM: "torn", Sequence: 1}, sizes)
	if err != nil {
		t.Fatal(err)
	}
	parentModel := newModel(sizes)
	first := store.Begin(root, control.Ref{VM: "torn", Sequence: 2})
	for page := range uint64(3) {
		for sector := range uint32(sectorsPerPage) {
			parentModel.dirty(first, "root", page, sector, sectorData("parent", page, sector))
		}
	}
	parent, err := first.Commit(t.Context(), parentModel)
	if err != nil {
		t.Fatal(err)
	}

	childModel := parentModel.clone()
	child := store.Begin(parent, control.Ref{VM: "torn", Sequence: 3})
	child.SetState([]byte("state"))
	childModel.dirty(child, "root", 0, 1, sectorData("child", 0, 1))
	childModel.dirty(child, "root", 1, 2, sectorData("child", 1, 2))
	for sector := range uint32(sectorsPerPage) {
		childModel.dirty(child, "root", 2, sector, sectorData("child", 2, sector))
	}

	objects.arm(failAt, afterApply)
	index, err := child.Commit(t.Context(), childModel)
	if failAt == 0 {
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, index, childModel)
		return objects.count()
	}
	if !errors.Is(err, platform.ErrInjectedFault) {
		t.Fatalf("interrupted at put %d: %v", failAt, err)
	}
	// The parent is untouched by a failed publication, whatever it wrote.
	reopenedParent, err := store.Open(t.Context(), parent.Ref())
	if err != nil {
		t.Fatal(err)
	}
	checkRead(t, store, reopenedParent, parentModel)

	objects.arm(0, false)
	retried, err := child.Commit(t.Context(), childModel)
	if err != nil {
		t.Fatalf("retry after put %d: %v", failAt, err)
	}
	checkRead(t, store, retried, childModel)
	reopenedChild, err := store.Open(t.Context(), retried.Ref())
	if err != nil {
		t.Fatal(err)
	}
	checkRead(t, store, reopenedChild, childModel)
	return objects.count()
}

func TestInterruptedCommitLeavesParentReadableAndRetrySucceeds(t *testing.T) {
	uploads := 0
	synctest.Test(t, func(t *testing.T) { uploads = interruptedCommit(t, 0, false) })
	// One part, holding the state and the three republished pages, and the
	// index object holding the segment locating them and the root.
	if uploads != 2 {
		t.Fatalf("publication wrote %d objects, want 2", uploads)
	}
	for put := 1; put <= uploads; put++ {
		for _, afterApply := range []bool{false, true} {
			t.Run(fmt.Sprintf("put=%d/applied=%t", put, afterApply), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) { interruptedCommit(t, put, afterApply) })
			})
		}
	}
}

// concurrencyStore records the greatest number of puts in flight at once, the
// greatest volume of object bodies they held together, and how many parts were
// written: a publication's memory is the parts it has not finished uploading,
// so both maxima are what a bounded publication is asserted on.
type concurrencyStore struct {
	platform.ObjectStore
	inFlight  atomic.Int64
	peak      atomic.Int64
	bytes     atomic.Int64
	peakBytes atomic.Int64
	parts     atomic.Int64
}

func (s *concurrencyStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	raise(&s.peak, s.inFlight.Add(1))
	raise(&s.peakBytes, s.bytes.Add(request.Size))
	if strings.Contains(request.Key.String(), "/part/") {
		s.parts.Add(1)
	}
	defer s.inFlight.Add(-1)
	defer s.bytes.Add(-request.Size)
	return s.ObjectStore.Put(ctx, request)
}

// raise records current as the new maximum when it exceeds the one seen.
func raise(peak *atomic.Int64, current int64) {
	for {
		seen := peak.Load()
		if current <= seen || peak.CompareAndSwap(seen, current) {
			return
		}
	}
}

func TestCommitBoundsUploadConcurrency(t *testing.T) {
	for _, concurrency := range []int{1, 3} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				objects := &concurrencyStore{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
				// A part per page, so a wide checkpoint has parts to upload at
				// once rather than one that holds everything.
				store := mustStore(t, checkpoint.Config{ObjectStore: objects, Concurrency: concurrency, PartBytes: 1})
				const pages = 12
				sizes := map[string]uint64{"root": pages * checkpoint.PageSize}
				root, err := store.Root(t.Context(), control.Ref{VM: "wide", Sequence: 1}, sizes)
				if err != nil {
					t.Fatal(err)
				}
				m := newModel(sizes)
				p := store.Begin(root, control.Ref{VM: "wide", Sequence: 2})
				for page := range uint64(pages) {
					for sector := range uint32(sectorsPerPage) {
						m.dirty(p, "root", page, sector, sectorData("wide", page, sector))
					}
				}
				objects.peak.Store(0)
				index, err := p.Commit(t.Context(), m)
				if err != nil {
					t.Fatal(err)
				}
				checkRead(t, store, index, m)
				if got := objects.peak.Load(); got != int64(concurrency) {
					t.Fatalf("peak uploads in flight %d, want %d", got, concurrency)
				}
			})
		})
	}
}

// Concurrency is a budget of the store, not of one publication. A host whose
// VMs all become dirty at once — after a mass restore, or when the idle ceiling
// lines them up — must not multiply it by the number of publications it starts.
func TestConcurrencyIsSharedAcrossConcurrentPublications(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const concurrency, publications, pages = 4, 8, 6
		objects := &concurrencyStore{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, Concurrency: concurrency})
		sizes := map[string]uint64{"root": pages * checkpoint.PageSize}
		models := make([]*model, publications)
		pending := make([]*checkpoint.Publication, publications)
		for vm := range publications {
			name := fmt.Sprintf("vm-%d", vm)
			root, err := store.Root(t.Context(), control.Ref{VM: name, Sequence: 1}, sizes)
			if err != nil {
				t.Fatal(err)
			}
			models[vm] = newModel(sizes)
			pending[vm] = store.Begin(root, control.Ref{VM: name, Sequence: 2})
			for page := range uint64(pages) {
				for sector := range uint32(sectorsPerPage) {
					models[vm].dirty(pending[vm], "root", page, sector, sectorData(name, page, sector))
				}
			}
		}

		objects.peak.Store(0)
		published := make([]*checkpoint.Index, publications)
		failures := make([]error, publications)
		var wait sync.WaitGroup
		for vm := range publications {
			wait.Add(1)
			go func() {
				defer wait.Done()
				published[vm], failures[vm] = pending[vm].Commit(t.Context(), models[vm])
			}()
		}
		wait.Wait()
		for vm := range publications {
			if failures[vm] != nil {
				t.Fatal(failures[vm])
			}
			checkRead(t, store, published[vm], models[vm])
		}
		if got := objects.peak.Load(); got != concurrency {
			t.Fatalf("peak uploads in flight across %d publications = %d, want %d", publications, got, concurrency)
		}
	})
}

func TestStoreRejectsUnusableConfigurationAndArguments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		if _, err := checkpoint.NewStore(checkpoint.Config{}); !errors.Is(err, checkpoint.ErrInvalidConfig) {
			t.Fatalf("store without an object store: %v", err)
		}
		if _, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: objects, Concurrency: -1}); !errors.Is(err, checkpoint.ErrInvalidConfig) {
			t.Fatalf("negative concurrency: %v", err)
		}
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		if _, err := store.Root(t.Context(), control.Ref{Sequence: 1}, nil); !errors.Is(err, checkpoint.ErrInvalidConfig) {
			t.Fatalf("root without a VM identity: %v", err)
		}
		if _, err := store.Root(t.Context(), control.Ref{VM: "a/b", Sequence: 1}, nil); !errors.Is(err, checkpoint.ErrInvalidConfig) {
			t.Fatalf("root with a structured VM identity: %v", err)
		}
		sizes := map[string]uint64{"root": checkpoint.SectorSize + 1}
		if _, err := store.Root(t.Context(), control.Ref{VM: "vm", Sequence: 1}, sizes); !errors.Is(err, checkpoint.ErrInvalidConfig) {
			t.Fatalf("root with a partial sector: %v", err)
		}
		root, err := store.Root(t.Context(), control.Ref{VM: "vm", Sequence: 1}, map[string]uint64{"root": checkpoint.PageSize})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Open(t.Context(), control.Ref{VM: "vm", Sequence: 9}); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("open of an unpublished checkpoint: %v", err)
		}
		if err := store.Read(t.Context(), root, "swap", 0, make([]byte, 1)); !errors.Is(err, checkpoint.ErrUnknownVolume) {
			t.Fatalf("read of an unknown volume: %v", err)
		}
		if err := store.Read(t.Context(), root, "root", checkpoint.PageSize, make([]byte, 1)); !errors.Is(err, checkpoint.ErrInvalidRange) {
			t.Fatalf("read past the end: %v", err)
		}
		named := store.Begin(root, control.Ref{VM: "vm", Sequence: 2})
		named.Dirty("a/b", 0)
		if _, err := named.Commit(t.Context(), newModel(map[string]uint64{"root": checkpoint.PageSize})); !errors.Is(err, checkpoint.ErrInvalidConfig) {
			t.Fatalf("dirty page of a structured volume name: %v", err)
		}
		unknown := store.Begin(root, control.Ref{VM: "vm", Sequence: 3})
		unknown.Dirty("swap", 0)
		if _, err := unknown.Commit(t.Context(), newModel(map[string]uint64{"root": checkpoint.PageSize})); !errors.Is(err, checkpoint.ErrUnknownVolume) {
			t.Fatalf("dirty page of an unknown volume: %v", err)
		}
		beyond := store.Begin(root, control.Ref{VM: "vm", Sequence: 4})
		beyond.Dirty("root", 9)
		if _, err := beyond.Commit(t.Context(), newModel(map[string]uint64{"root": checkpoint.PageSize})); !errors.Is(err, checkpoint.ErrInvalidRange) {
			t.Fatalf("dirty page past the end: %v", err)
		}
	})
}

func TestCommitRefusesToReuseAReferenceForOtherContents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		sizes := map[string]uint64{"root": checkpoint.PageSize}
		root, err := store.Root(t.Context(), control.Ref{VM: "reuse", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(sizes)
		first := store.Begin(root, control.Ref{VM: "reuse", Sequence: 2})
		m.dirty(first, "root", 0, 0, sectorData("reuse", 0, 0))
		if _, err := first.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		second := store.Begin(root, control.Ref{VM: "reuse", Sequence: 2})
		other := newModel(sizes)
		other.dirty(second, "root", 0, 0, sectorData("other", 0, 0))
		other.dirty(second, "root", 0, 1, sectorData("other", 0, 1))
		if _, err := second.Commit(t.Context(), other); !errors.Is(err, checkpoint.ErrConflict) {
			t.Fatalf("reused reference with other contents: %v", err)
		}
	})
}
