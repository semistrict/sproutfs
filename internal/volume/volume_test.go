package volume_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/volume"
)

// harness is one simulated deployment: an object store holding every VM's
// control record and checkpoints. A manager built over it is one host, so two
// managers over the same store are two hosts.
type harness struct {
	runtime *sim.Runtime
	prefix  platform.ObjectPrefix
	objects platform.ObjectStore
	// knobs are the tunables every store and manager this harness builds is
	// given. They are the deployment's defaults unless a campaign drew a seed's
	// own set, which only happens under the opt-in.
	knobs knobs.Knobs
	// clients counts the control clients this harness has built, so each one
	// draws its nonces and its creating epochs from a stream of its own and a
	// scenario reproduces whatever order its writers ran in.
	clients int
}

// randomizeKnobs gives this harness one seed's own tunables, so a campaign
// explores the part sizes and budgets a constant used to fix. It is a no-op
// without SPROUTFS_TEST_KNOBS: the scheduled scenarios compare their recordings
// byte for byte across processes, and a seed that also chose its knobs would be
// comparing a different run.
func (h *harness) randomizeKnobs(t *testing.T) {
	t.Helper()
	if os.Getenv("SPROUTFS_TEST_KNOBS") == "" {
		return
	}
	h.knobs = knobs.Randomize(h.runtime.Random("campaign/knobs"))
	// A scenario that models a takeover holds the superseded handle as well as
	// the one that fenced it, so the open-VM bound cannot go below what the
	// scenario itself opens. The bound is a real one — a host given it refuses
	// the next open — but refusing the scenario's own second handle is the
	// harness running out of room, not the code deciding anything.
	h.knobs.MaxOpenVMs = max(h.knobs.MaxOpenVMs, 8)
	if err := h.knobs.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Logf("knobs=%v", h.knobs.Changed())
}

func newHarness(t *testing.T) *harness { return newSeededHarness(t, 1) }

func newSeededHarness(t *testing.T, seed uint64) *harness {
	return newConfiguredHarness(t, sim.Config{Seed: seed})
}

func newConfiguredHarness(t *testing.T, config sim.Config) *harness {
	t.Helper()
	runtime := sim.New(config)
	prefix, err := platform.NewObjectPrefix("sproutfs/")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{runtime: runtime, prefix: prefix, objects: runtime.ObjectStore(),
		knobs: knobs.Defaults()}
}

// close releases what the harness owns, which is nothing durable: every host's
// state is in the object store.
func (h *harness) close(context.Context) {}

// config is the manager configuration every test starts from. Nothing publishes
// on its own, so a test decides exactly when a checkpoint happens.
func (h *harness) config() volume.Config {
	return volume.Config{MaxWriteBytes: h.knobs.MaxWriteBytes, MaxOpenVMs: h.knobs.MaxOpenVMs}
}

func (h *harness) controlClient(t *testing.T, objects platform.ObjectStore) *control.Client {
	t.Helper()
	h.clients++
	client, err := control.NewClient(control.Config{ObjectStore: objects, ObjectPrefix: h.prefix,
		Entropy: h.runtime.NewEntropy(fmt.Sprintf("control/%d", h.clients))})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (h *harness) imageStore(t *testing.T, objects platform.ObjectStore) *checkpoint.Store {
	t.Helper()
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: objects, ObjectPrefix: h.prefix,
		PartBytes: h.knobs.PartBytes, MaxIndexBytes: h.knobs.MaxIndexBytes,
		Concurrency: h.knobs.UploadConcurrency, MaxBuilders: h.knobs.MaxBuilders})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// manager builds a manager over the harness store, supplying the control client
// and checkpoint store the test did not.
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

// faultStore fails one armed object-store write, optionally after the store has
// already applied it, which is how a lost reply is simulated. It also blocks
// every write matching a predicate, which is how a publication is held up
// indefinitely.
type faultStore struct {
	platform.ObjectStore

	mu       sync.Mutex
	armed    bool
	at       int
	after    bool
	seen     int
	failed   bool
	deny     func(platform.ObjectKey) bool
	holding  func(platform.ObjectKey) bool
	released chan struct{}
}

// hold blocks every matching write until the returned function releases it,
// which is how a publication is caught in the middle.
func (s *faultStore) hold(match func(platform.ObjectKey) bool) func() {
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

// gate reports the channel a write must wait on, and nil when it may proceed.
func (s *faultStore) gate(key platform.ObjectKey) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holding == nil || !s.holding(key) {
		return nil
	}
	return s.released
}

func (s *faultStore) arm(at int, after bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed, s.at, s.after, s.seen, s.failed = true, at, after, 0, false
}

func (s *faultStore) disarm() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = false
	return s.failed
}

func (s *faultStore) block(deny func(platform.ObjectKey) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deny = deny
}

// decide reports whether this write fails, and whether the store applies it
// first.
func (s *faultStore) decide(key platform.ObjectKey) (fail, apply bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deny != nil && s.deny(key) {
		return true, false
	}
	if !s.armed {
		return false, false
	}
	s.seen++
	if s.seen != s.at {
		return false, false
	}
	s.failed = true
	return true, s.after
}

func (s *faultStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if gate := s.gate(request.Key); gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return platform.PutResult{}, context.Cause(ctx)
		}
	}
	fail, apply := s.decide(request.Key)
	if !fail {
		return s.ObjectStore.Put(ctx, request)
	}
	if apply {
		if _, err := s.ObjectStore.Put(ctx, request); err != nil {
			return platform.PutResult{}, err
		}
	}
	return platform.PutResult{}, platform.ErrInjectedFault
}

// objectKeys lists every object in the harness store, by the key that follows
// the deployment prefix, in ascending order. It is how a test asks what an
// operation wrote — and, for a fork, that it wrote nothing.
func (h *harness) objectKeys(t *testing.T) []string {
	t.Helper()
	var names []string
	token := ""
	for {
		page, err := h.objects.List(t.Context(), platform.ListRequest{Prefix: h.prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			names = append(names, strings.TrimPrefix(object.Key.String(), h.prefix.String()))
		}
		if page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	slices.Sort(names)
	return names
}

// added reports the keys in after that before did not have, in ascending order.
func added(before, after []string) []string {
	var extra []string
	for _, key := range after {
		if !slices.Contains(before, key) {
			extra = append(extra, key)
		}
	}
	return extra
}

// model is the byte model every read is checked against.
type model map[string][]byte

func newModel(specs []volume.VolumeSpec) model {
	m := make(model, len(specs))
	for _, spec := range specs {
		m[spec.Name] = make([]byte, spec.Size)
	}
	return m
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
		clear(data) // the VM must own the bytes it accepted
	}
}

// The test VM has one volume of whole pages and one whose last page is a
// three-page tail, so every publication exercises both.
var testSpecs = []volume.VolumeSpec{
	{Name: "root", Size: 3 * checkpoint.PageSize},
	{Name: "state", Size: checkpoint.PageSize + 3*checkpoint.SectorSize},
}

func createVM(t *testing.T, m *volume.Manager, id string) (*volume.VM, model) {
	t.Helper()
	vm, err := m.Create(t.Context(), id, testSpecs)
	if err != nil {
		t.Fatal(err)
	}
	return vm, newModel(testSpecs)
}

// counted is the sequence the counter-th checkpoint of this handle's epoch
// takes. A creating handle draws its epoch rather than starting at a fixed one,
// so a test that names a checkpoint asks the handle that publishes it; a test
// that names one an earlier handle published keeps that handle's epoch before
// the VM is reopened.
func counted(vm *volume.VM, counter uint64) uint64 {
	return control.Sequence(vm.Epoch(), counter)
}

// A VM's volumes must read as the byte model after any interleaving of writes,
// discards, checkpoints and reopens.
func TestVolumeByteModelSurvivesCheckpointsAndReopens(t *testing.T) {
	for _, seed := range []uint64{1, 7, 23} {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newSeededHarness(t, seed)
				defer h.close(t.Context())
				manager := h.manager(t, h.config())
				defer manager.Close(t.Context())
				vm, want := createVM(t, manager, "vm")
				random := rand.New(rand.NewPCG(seed, 91))
				for round := range 10 {
					workload(t, vm, want, random, 15)
					if round%3 == 0 {
						if err := vm.Checkpoint(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					want.check(t, vm, fmt.Sprintf("round %d", round))
					if round%2 != 0 {
						continue
					}
					if err := vm.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					reopened, err := manager.Open(t.Context(), "vm")
					if err != nil {
						t.Fatal(err)
					}
					vm = reopened
					want.check(t, vm, fmt.Sprintf("reopen after round %d", round))
				}
				if err := vm.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// A write contacts nothing, so an object-store outage cannot refuse one. What
// it refuses is the checkpoint, which is the only thing that makes those bytes
// durable, and the handle says so without losing them.
func TestOutageStallsPublicationAndNotWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		root := vm.Volume("root")
		data := bytes.Repeat([]byte{7}, 8192)
		if err := root.Write(t.Context(), 0, data); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], data)
		h.runtime.ObjectStore().Fail()
		if err := root.Write(t.Context(), 13, []byte("new")); err != nil {
			t.Fatalf("a write during an object-store outage: %v", err)
		}
		copy(want["root"][13:], []byte("new"))
		if err := root.Verify(t.Context()); err != nil {
			t.Fatalf("an authority check during an object-store outage: %v", err)
		}
		if err := vm.Checkpoint(t.Context()); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("checkpoint during the outage = %v, want ErrUnavailable", err)
		}
		if status := vm.Status(); !errors.Is(status.CheckpointError, platform.ErrUnavailable) || status.Err != nil {
			t.Fatalf("status during the outage = %+v, want a reported publication failure and a usable handle", status)
		}
		want.check(t, vm, "during the outage")
		h.runtime.ObjectStore().Recover()
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if status := vm.Status(); status.CheckpointError != nil || status.DirtyBytes != 0 {
			t.Fatalf("status after the outage = %+v", status)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		want.check(t, reopened, "reopened after the outage ended")
		if err := reopened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// Opening a VM again advances the control record's epoch, which fences the old
// handle. It keeps its local view and still accepts writes into it, but nothing
// it holds can ever become durable, and its next checkpoint says so.
func TestReopenFencesTheWholeOldHandle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("before")); err != nil {
			t.Fatal(err)
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		next, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer next.Close(t.Context())
		if got := next.Epoch(); got != vm.Epoch()+1 {
			t.Fatalf("epoch after the takeover = %d, want %d", got, vm.Epoch()+1)
		}
		got := make([]byte, 6)
		if err := next.Volume("root").Read(t.Context(), 0, got); err != nil || string(got) != "before" {
			t.Fatalf("takeover read = %q, %v, want \"before\"", got, err)
		}
		if err := vm.Volume("root").Read(t.Context(), 0, got); err != nil || string(got) != "before" {
			t.Fatalf("old handle's local view = %q, %v, want \"before\"", got, err)
		}
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("stale!")); err != nil {
			t.Fatalf("the fenced handle refused a local write: %v", err)
		}
		if err := vm.Checkpoint(t.Context()); !errors.Is(err, control.ErrFenced) || !errors.Is(err, volume.ErrNeedsRecovery) {
			t.Fatalf("checkpoint from the fenced handle = %v, want ErrFenced and ErrNeedsRecovery", err)
		}
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("later")); !errors.Is(err, volume.ErrNeedsRecovery) {
			t.Fatalf("write after the fence was observed = %v, want ErrNeedsRecovery", err)
		}
		if err := vm.Volume("root").Verify(t.Context()); !errors.Is(err, volume.ErrNeedsRecovery) {
			t.Fatalf("authority check from the fenced handle = %v, want ErrNeedsRecovery", err)
		}
		// The fenced handle never reached object storage, so the takeover reads
		// exactly what the last checkpoint published.
		if err := next.Volume("root").Read(t.Context(), 0, got); err != nil || string(got) != "before" {
			t.Fatalf("the fenced handle's writes reached the VM: %q, %v", got, err)
		}
	})
}

// A handle that publishes nothing learns it has been taken over by re-reading
// its control record. It is the only way a VM between checkpoints — or one a
// fork point has sealed, which is not checkpointed at all — finds out, and it
// must leave the handle exactly as a refused publication does.
func TestConfirmFencesAHandleTheRecordHasMovedPast(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if err := vm.Confirm(t.Context()); err != nil {
			t.Fatalf("the only writer's handle = %v, want a confirmed one", err)
		}
		next, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer next.Close(t.Context())
		if err := vm.Confirm(t.Context()); !errors.Is(err, control.ErrFenced) ||
			!errors.Is(err, volume.ErrNeedsRecovery) {
			t.Fatalf("Confirm after the takeover = %v, want ErrFenced and ErrNeedsRecovery", err)
		}
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("stale")); !errors.Is(err, volume.ErrNeedsRecovery) {
			t.Fatalf("a write after Confirm = %v, want a handle that must be reopened", err)
		}
		if status := vm.Status(); !errors.Is(status.Err, volume.ErrNeedsRecovery) {
			t.Fatalf("the confirmed-fenced handle's status = %+v", status)
		}
		if err := next.Confirm(t.Context()); err != nil {
			t.Fatalf("the writer that holds the epoch = %v, want a confirmed one", err)
		}
	})
}

// A batched write is one overlay generation, applied in order.
func TestWriteBatchIsOneGenerationAppliedInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		// The in-tree bug guard on the payload budget reads the runtime out
		// of the context, so this test is also what kills
		// volume-unbounded-write.
		ctx := sim.WithRuntime(t.Context(), h.runtime)
		defer h.close(ctx)
		manager := h.manager(t, h.config())
		defer manager.Close(ctx)
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(ctx)
		root := vm.Volume("root")
		if err := root.WriteBatch(ctx, []volume.WriteExtent{
			{Offset: 0, Data: bytes.Repeat([]byte{1}, 4096)},
			{Offset: 8192, Data: bytes.Repeat([]byte{2}, 4096)},
			{Offset: 2048, Data: bytes.Repeat([]byte{3}, 2048)},
		}); err != nil {
			t.Fatal(err)
		}
		copy(want["root"][0:], bytes.Repeat([]byte{1}, 4096))
		copy(want["root"][8192:], bytes.Repeat([]byte{2}, 4096))
		copy(want["root"][2048:], bytes.Repeat([]byte{3}, 2048))
		want.check(t, vm, "after the batch")
		oversized := []volume.WriteExtent{{Offset: 0, Data: make([]byte, root.MaxWriteBytes())}, {Offset: uint64(root.MaxWriteBytes()), Data: make([]byte, 1)}}
		if err := root.WriteBatch(ctx, oversized); !errors.Is(err, volume.ErrWriteTooLarge) {
			t.Fatalf("oversized batch = %v, want ErrWriteTooLarge", err)
		}
		if err := root.Write(ctx, root.Size(), []byte("x")); !errors.Is(err, volume.ErrInvalidRange) {
			t.Fatalf("write past the end = %v, want ErrInvalidRange", err)
		}
	})
}

// A manager admits a bounded number of live handles.
func TestOpenVMBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		config := h.config()
		config.MaxOpenVMs = 1
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if _, err := manager.Create(t.Context(), "second", testSpecs); !errors.Is(err, volume.ErrCapacity) {
			t.Fatalf("Create past MaxOpenVMs = %v, want ErrCapacity", err)
		}
		if stats := manager.Stats(); stats.OpenVMs != 1 || stats.MaxOpenVMs != 1 {
			t.Fatalf("Stats = %+v, want one open VM against a bound of one", stats)
		}
	})
}

// Deleting a VM removes its control record, so nothing can open it again.
func TestDeleteRemovesTheVM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Open(t.Context(), "vm"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("Open after Delete = %v, want ErrNotFound", err)
		}
	})
}

// An identity is usable again once its VM is deleted and the objects under it
// are gone: a create is refused while anything is stored under an identity no
// record accounts for. Deleting a VM therefore deletes what it published, which
// is also the only thing that ever frees the space a deleted VM occupies. The
// VM created afterwards shares nothing with the dead one — it draws its own
// creating epoch, so not one sequence, lineage identity or object key of the
// two is the same.
func TestARecreatedIdentityIsNotPoisonedByTheDeletedVM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		random := rand.New(rand.NewPCG(31, 37))
		workload(t, vm, want, random, 12)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}
		// Deleting twice is how a caller finishes a delete that was interrupted.
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatalf("repeating a delete: %v", err)
		}

		again, reborn := createVM(t, manager, "vm")
		defer again.Close(t.Context())
		workload(t, again, reborn, random, 12)
		if err := again.Checkpoint(t.Context()); err != nil {
			t.Fatalf("checkpointing a recreated identity: %v", err)
		}
		reborn.check(t, again, "the recreated VM")
		// What a later host reads is the new VM's own state, not the dead one's.
		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		reborn.check(t, reopened, "the recreated VM reopened")
	})
}
