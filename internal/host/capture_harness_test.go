package host_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// harness is one simulated deployment: a single object store. Managers built
// over it stand in for hosts, so a second manager over the same object store is
// a second host.
type harness struct {
	runtime  *sim.Runtime
	prefix   platform.ObjectPrefix
	objects  platform.ObjectStore
	sequence int
}

func newHarness(t *testing.T) *harness { return newSeededHarness(t, 1) }

func newSeededHarness(t *testing.T, seed uint64) *harness {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: seed})
	prefix, err := platform.NewObjectPrefix("sproutfs/")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{runtime: runtime, prefix: prefix, objects: runtime.ObjectStore()}
}

// rootSeq is the sequence a VM's first checkpoint is published under, and
// secondSeq the one its creating handle publishes next. A creating handle draws
// its epoch, so both are asked of the handle that publishes them rather than
// assumed to start anywhere in particular.
func rootSeq(vm *volume.VM) uint64   { return control.Sequence(vm.Epoch(), 1) }
func secondSeq(vm *volume.VM) uint64 { return control.Sequence(vm.Epoch(), 2) }

// close releases whatever the harness owns, which is nothing durable: every
// host's state is the object store.
func (h *harness) close(context.Context) {}

// config is the manager configuration every test starts from. Nothing publishes
// on its own, so a test decides exactly when a checkpoint happens.
func (h *harness) config() volume.Config { return volume.Config{} }

func (h *harness) controlClient(t *testing.T, objects platform.ObjectStore) *control.Client {
	t.Helper()
	h.sequence++
	client, err := control.NewClient(control.Config{ObjectStore: objects, ObjectPrefix: h.prefix,
		Entropy: h.runtime.NewEntropy(fmt.Sprintf("control/%d", h.sequence))})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (h *harness) imageStore(t *testing.T, objects platform.ObjectStore) *checkpoint.Store {
	t.Helper()
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: objects, ObjectPrefix: h.prefix})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// manager builds a manager over the harness store, supplying the control client
// and checkpoint store the test did not. Each call is a distinct host: it gets its
// own control client and its own page cache.
func (h *harness) manager(t *testing.T, config volume.Config) *volume.Manager {
	t.Helper()
	if config.Control == nil {
		config.Control = h.controlClient(t, h.objects)
	}
	if config.Store == nil {
		config.Store = h.imageStore(t, h.objects)
	}
	m, err := volume.NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// holdStore blocks every object write matching a predicate until it is
// released, which is how a publication is caught before it completes.
type holdStore struct {
	platform.ObjectStore

	mu       sync.Mutex
	holding  func(platform.ObjectKey) bool
	released chan struct{}
}

func (s *holdStore) hold(match func(platform.ObjectKey) bool) func() {
	released := make(chan struct{})
	s.mu.Lock()
	s.holding, s.released = match, released
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.holding = nil
		s.mu.Unlock()
		close(released)
	}
}

func (s *holdStore) gate(key platform.ObjectKey) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holding == nil || !s.holding(key) {
		return nil
	}
	return s.released
}

func (s *holdStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if gate := s.gate(request.Key); gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return platform.PutResult{}, context.Cause(ctx)
		}
	}
	return s.ObjectStore.Put(ctx, request)
}

// fakeSource stands in for one region's sealed checkpoint: the pager pages the
// seal froze, the bytes of each, and what the publication did with them.
type fakeSource struct {
	pageSize int
	pages    map[uint64][]byte
	readErr  error

	mu        sync.Mutex
	reads     int
	retires   int
	published bool
	held      bool
	shared    []control.Identity
}

func newFakeSource(pageSize int) *fakeSource {
	return &fakeSource{pageSize: pageSize, pages: make(map[uint64][]byte)}
}

// set records one whole sealed pager page.
func (s *fakeSource) set(page uint64, data []byte) {
	full := make([]byte, s.pageSize)
	copy(full, data)
	s.pages[page] = full
}

func (s *fakeSource) PageSize() int { return s.pageSize }

func (s *fakeSource) DirtyPages() []uint64 {
	pages := make([]uint64, 0, len(s.pages))
	for page := range s.pages {
		pages = append(pages, page)
	}
	slices.Sort(pages)
	return pages
}

func (s *fakeSource) ReadDirty(_ context.Context, page uint64, dst []byte) error {
	s.mu.Lock()
	s.reads++
	s.mu.Unlock()
	if s.readErr != nil {
		return s.readErr
	}
	data, found := s.pages[page]
	if !found {
		return fmt.Errorf("read of page %d, which the checkpoint does not hold", page)
	}
	if len(dst) != s.pageSize {
		return fmt.Errorf("read of %d bytes, want one %d-byte pager page", len(dst), s.pageSize)
	}
	copy(dst, data)
	return nil
}

// Hold records that a fork instant took this seal, which on a real host is what
// says it lasts as long as the children of that instant.
func (s *fakeSource) Hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = true
}

// Share records the identity a fork point gave these pages, which on a real
// host is what lets a child map the frames instead of reading them.
func (s *fakeSource) Share(_ context.Context, ref control.Ref, volume string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shared = append(s.shared, control.Identity{Ref: ref, Volume: volume})
	return nil
}

func (s *fakeSource) Retire(_ context.Context, published bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retires++
	s.published = published
	return nil
}

// outcome reports how often the checkpoint was retired and whether the
// checkpoint that read it was selected.
func (s *fakeSource) outcome() (retires int, published bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retires, s.published
}

// fakeRuntime stands in for the VMM process: it records the order of the pause
// and the resume a capture drives, and hands out the VMM state and the sealed
// checkpoint of every region.
type fakeRuntime struct {
	state      []byte
	sources    map[string]volume.DirtySource
	prepareErr error
	resumeErr  error
	releaseErr error

	mu    sync.Mutex
	calls []string
}

func (r *fakeRuntime) Prepare(context.Context) ([]byte, map[string]volume.DirtySource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "prepare")
	if r.prepareErr != nil {
		return nil, nil, r.prepareErr
	}
	return bytes.Clone(r.state), r.sources, nil
}

func (r *fakeRuntime) Resume(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "resume")
	return r.resumeErr
}

func (r *fakeRuntime) Release(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "release")
	return r.releaseErr
}

// The rest of host.Machine is the migration's and the watcher's half, which a
// capture never reaches: this fake is only ever paused, resumed and released.
func (r *fakeRuntime) Regions() map[string]*vmmemory.Region { return nil }

func (r *fakeRuntime) Stop(context.Context) ([]byte, error) { return bytes.Clone(r.state), nil }

func (r *fakeRuntime) Wait(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }

func (r *fakeRuntime) Close() error { return nil }

func (r *fakeRuntime) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *fakeRuntime) check(t *testing.T, want ...string) {
	t.Helper()
	got := r.log()
	if len(got) != len(want) {
		t.Fatalf("runtime calls = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("runtime calls = %v, want %v", got, want)
		}
	}
}

// model is the byte model every read is checked against.
type model map[string][]byte

func newModel() model {
	m := make(model, len(testSpecs))
	for _, spec := range testSpecs {
		m[spec.Name] = make([]byte, spec.Size)
	}
	return m
}

func (m model) clone() model {
	copied := make(model, len(m))
	for name, data := range m {
		copied[name] = bytes.Clone(data)
	}
	return copied
}

func (m model) check(t *testing.T, vm *volume.VM, what string) {
	t.Helper()
	for _, v := range vm.Volumes() {
		got := make([]byte, v.Size())
		if err := v.Read(t.Context(), 0, got); err != nil {
			t.Fatalf("%s: reading %s: %v", what, v.Name(), err)
		}
		if !bytes.Equal(got, m[v.Name()]) {
			t.Fatalf("%s: volume %s differs from the byte model", what, v.Name())
		}
	}
}

func (m model) checkCheckpoint(t *testing.T, ckpt *volume.Checkpoint, what string) {
	t.Helper()
	for name, want := range m {
		got := make([]byte, len(want))
		if err := ckpt.Read(t.Context(), name, 0, got); err != nil {
			t.Fatalf("%s: reading %s: %v", what, name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: checkpoint of %s differs from the byte model", what, name)
		}
	}
}

// workload applies count random writes and discards to a VM and its model.
func workload(t *testing.T, vm *volume.VM, m model, random *rand.Rand, count int) {
	t.Helper()
	volumes := vm.Volumes()
	for range count {
		v := volumes[random.IntN(len(volumes))]
		want := m[v.Name()]
		offset := random.IntN(len(want))
		length := random.IntN(min(8192, len(want)-offset) + 1)
		if length == 0 {
			continue
		}
		if random.IntN(3) == 0 {
			if err := v.Discard(t.Context(), uint64(offset), uint64(length)); err != nil {
				t.Fatal(err)
			}
			clear(want[offset : offset+length])
			continue
		}
		data := bytes.Repeat([]byte{byte(random.IntN(255) + 1)}, length)
		if err := v.Write(t.Context(), uint64(offset), data); err != nil {
			t.Fatal(err)
		}
		copy(want[offset:], data)
	}
}

// The captured VM has one volume of whole pages and one whose last page is a
// three-sector tail, so every publication exercises both.
var testSpecs = []volume.VolumeSpec{
	{Name: "ram", Size: 3 * checkpoint.PageSize},
	{Name: "pmem", Size: checkpoint.PageSize + 3*checkpoint.SectorSize},
}

func createVM(t *testing.T, m *volume.Manager, id string) (*volume.VM, model) {
	t.Helper()
	vm, err := m.Create(t.Context(), id, testSpecs)
	if err != nil {
		t.Fatal(err)
	}
	return vm, newModel()
}

// indexKey matches the index object of one checkpoint, the last object a
// publication writes before it selects the checkpoint.
func indexKey(ref control.Ref) func(platform.ObjectKey) bool {
	want := fmt.Sprintf("vm/%s/ckpt/%d/index", ref.VM, ref.Sequence)
	return func(key platform.ObjectKey) bool { return strings.HasSuffix(key.String(), want) }
}

// vmmState is a recognizable, sector-crossing VMM state blob.
func vmmState(seed byte) []byte {
	state := make([]byte, 3*checkpoint.SectorSize+17)
	for index := range state {
		state[index] = seed + byte(index%251)
	}
	return state
}

// closeVM releases a handle and fails the test on anything but an already
// closed one.
func closeVM(t *testing.T, vm *volume.VM) {
	t.Helper()
	if err := vm.Close(t.Context()); err != nil && !errors.Is(err, volume.ErrClosed) {
		t.Fatalf("closing %s: %v", vm.ID(), err)
	}
}
