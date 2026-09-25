package volume_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// observed counts object-store requests by what they name, which is how a test
// says what one operation costs: a checkpoint's index object, which holds its
// root and is what opening one reads, any other checkpoint object, or a VM's
// control record.
type observed struct {
	root    int
	object  int
	control int
	other   int
}

func (o observed) since(base observed) observed {
	return observed{root: o.root - base.root, object: o.object - base.object,
		control: o.control - base.control, other: o.other - base.other}
}

// observingStore records every request one manager's clients issue, so a test
// can assert the exact cost of an open or a handoff rather than that it merely
// succeeded.
type observingStore struct {
	platform.ObjectStore

	mu   sync.Mutex
	gets observed
	puts observed
}

func (s *observingStore) count(counts *observed, key platform.ObjectKey) {
	name := key.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.HasSuffix(name, "/index"):
		counts.root++
	case strings.Contains(name, "/ckpt/"):
		counts.object++
	case strings.Contains(name, control.RecordPrefix):
		counts.control++
	default:
		counts.other++
	}
}

func (s *observingStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	s.count(&s.gets, request.Key)
	return s.ObjectStore.Get(ctx, request)
}

func (s *observingStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	s.count(&s.gets, key)
	return s.ObjectStore.Head(ctx, key)
}

func (s *observingStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	s.count(&s.puts, request.Key)
	return s.ObjectStore.Put(ctx, request)
}

// totals reports the requests seen so far; a test diffs two of them around the
// operation it is measuring.
func (s *observingStore) totals() (gets, puts observed) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.puts
}

// observedManager builds a manager whose control client and checkpoint store share
// one observing object store.
func (h *harness) observedManager(t *testing.T) (*volume.Manager, *observingStore) {
	t.Helper()
	store := &observingStore{ObjectStore: h.objects}
	config := h.config()
	config.Control = h.controlClient(t, store)
	config.Store = h.imageStore(t, store)
	return h.manager(t, config), store
}

// Handoff publishes nothing. It releases the VM to another host, which opens the
// checkpoint the control record already selects; everything written since it
// lives in the source's pager and reaches the destination over the page server,
// so uploading it inside the pause would be the one cost a post-copy migration
// exists to avoid.
func TestHandoffPublishesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, source := h.observedManager(t)
		defer manager.Close(t.Context())

		vm, want := createVM(t, manager, "vm")
		root, state := vm.Volume("root"), vm.Volume("state")
		published := bytes.Repeat([]byte{9}, 8192)
		if err := root.Write(t.Context(), 0, published); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], published)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		selected := vm.Status().Checkpoint
		// Written after the last checkpoint, so the handoff leaves them behind:
		// on a real host they are the pager's pages, which the destination
		// fetches and publishes in its own next checkpoint.
		if err := root.Write(t.Context(), 4096, []byte("tail")); err != nil {
			t.Fatal(err)
		}
		if err := state.Write(t.Context(), 8192, []byte("moved")); err != nil {
			t.Fatal(err)
		}

		getsBefore, putsBefore := source.totals()
		if err := vm.Handoff(t.Context()); err != nil {
			t.Fatal(err)
		}
		gets, puts := source.totals()
		if diff := puts.since(putsBefore); diff != (observed{}) {
			t.Fatalf("Handoff wrote %+v, want nothing at all", diff)
		}
		if diff := gets.since(getsBefore); diff != (observed{}) {
			t.Fatalf("Handoff read %+v, want nothing at all", diff)
		}

		status := vm.Status()
		final := control.Ref{VM: "vm", Sequence: counted(vm, 2)}
		if selected != final {
			t.Fatalf("the last checkpoint was %v, want %v", selected, final)
		}
		if !status.HandedOff || status.Checkpoint != final {
			t.Fatalf("status after handoff is %+v, want handed off at %v", status, final)
		}
		if !errors.Is(status.Err, volume.ErrHandedOff) {
			t.Fatalf("status error after handoff is %v, want ErrHandedOff", status.Err)
		}
		if err := root.Write(t.Context(), 0, []byte("x")); !errors.Is(err, volume.ErrHandedOff) {
			t.Fatalf("writing a handed-off volume returned %v, want ErrHandedOff", err)
		}
		if err := root.Read(t.Context(), 0, make([]byte, 8)); !errors.Is(err, volume.ErrHandedOff) {
			t.Fatalf("reading a handed-off volume returned %v, want ErrHandedOff", err)
		}
		if _, err := root.Locate(t.Context(), 0, 4096); !errors.Is(err, volume.ErrHandedOff) {
			t.Fatalf("locating in a handed-off volume returned %v, want ErrHandedOff", err)
		}
		if err := root.Verify(t.Context()); !errors.Is(err, volume.ErrHandedOff) {
			t.Fatalf("checking authority on a handed-off volume returned %v, want ErrHandedOff", err)
		}
		if err := vm.Checkpoint(t.Context()); !errors.Is(err, volume.ErrHandedOff) {
			t.Fatalf("checkpointing a handed-off VM returned %v, want ErrHandedOff", err)
		}
		if _, err := vm.Snapshot(t.Context(), volume.Prepared([]byte("state"), nil)); !errors.Is(err, volume.ErrHandedOff) {
			t.Fatalf("snapshotting a handed-off VM returned %v, want ErrHandedOff", err)
		}
		if err := vm.Handoff(t.Context()); !errors.Is(err, volume.ErrHandedOff) {
			t.Fatalf("handing off twice returned %v, want ErrHandedOff", err)
		}

		destination, observer := h.observedManager(t)
		defer destination.Close(t.Context())
		moved, err := destination.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		gets, _ = observer.totals()
		if gets.root != 1 || gets.object != 0 {
			t.Fatalf("the destination's open read %+v, want exactly one root and no pages", gets)
		}
		if got := moved.Status().Checkpoint; got != final {
			t.Fatalf("the destination opened at %v, want the handed-off %v", got, final)
		}
		// The destination sees the last checkpoint exactly, and none of the
		// writes the source made after it: those are the source pager's to serve.
		want.check(t, moved, "opened on the destination after a handoff")
		if err := moved.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// Opening a VM costs the control record and one read of the checkpoint's last
// part, which holds its root, and nothing else:
// pages are read lazily by the reads that need them, and nothing verifies them.
// The two control-record reads are the one that says whether the selected
// checkpoint was ever published and the one the epoch's own write reads to
// write against; the one write is that epoch.
func TestOpenReadsOnlyTheCheckpointRoot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := h.observedManager(t)
		vm, want := createVM(t, manager, "vm")
		body := bytes.Repeat([]byte{4}, 1<<20)
		if err := vm.Volume("root").Write(t.Context(), 0, body); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], body)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("tail")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], []byte("tail"))
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Handoff(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}

		destination, observer := h.observedManager(t)
		defer destination.Close(t.Context())
		opened, err := destination.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		gets, puts := observer.totals()
		if gets != (observed{root: 1, control: 2}) {
			t.Fatalf("Open read %+v, want one checkpoint root and two control records", gets)
		}
		if puts != (observed{control: 1}) {
			t.Fatalf("Open wrote %+v, want the one control record an open writes", puts)
		}
		want.check(t, opened, "opened with a recent checkpoint")
		if err := opened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// Open serves the destination of a migration, so it must not wait for whatever
// the source is still uploading. A publication held in the middle of its page
// writes does not delay another host's open, which sees the checkpoint the
// control record still selects.
func TestOpenDoesNotWaitForAPublicationInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		faults := &faultStore{ObjectStore: h.objects}
		config := h.config()
		config.Control = h.controlClient(t, faults)
		config.Store = h.imageStore(t, faults)
		manager := h.manager(t, config)
		defer manager.Close(t.Context())

		vm, want := createVM(t, manager, "vm")
		body := bytes.Repeat([]byte{6}, 128<<10)
		if err := vm.Volume("root").Write(t.Context(), 0, body); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], body)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		selected := vm.Status().Checkpoint

		// The next publication is held in the middle of its page writes.
		held := fmt.Sprintf("/ckpt/%d/", counted(vm, 3))
		release := faults.hold(func(key platform.ObjectKey) bool {
			return strings.Contains(key.String(), held)
		})
		if err := vm.Volume("state").Write(t.Context(), 0, []byte("unpublished")); err != nil {
			t.Fatal(err)
		}
		if _, err := vm.Snapshot(t.Context(), volume.Prepared([]byte("vmm"), nil)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		destination := h.manager(t, h.config())
		defer destination.Close(t.Context())
		opened, err := destination.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if got := opened.Status().Checkpoint; got != selected {
			t.Fatalf("the open took the in-flight checkpoint %v, want the selected %v", got, selected)
		}
		want.check(t, opened, "opened while the source was still publishing")
		if err := opened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		release()
	})
}

// A migration abandoned after the handoff leaves the VM openable by the host
// that handed it off, which is the same path any other host takes.
func TestHandedOffVMReopensOnItsOwnHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())

		vm, want := createVM(t, manager, "vm")
		body := bytes.Repeat([]byte{3}, 4096)
		if err := vm.Volume("root").Write(t.Context(), 0, body); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], body)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Handoff(t.Context()); err != nil {
			t.Fatal(err)
		}

		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if status := reopened.Status(); status.HandedOff || status.Err != nil {
			t.Fatalf("the reopened handle reports %+v, want a usable handle", status)
		}
		want.check(t, reopened, "reopened after an abandoned handoff")
		if err := reopened.Volume("state").Write(t.Context(), 0, []byte("again")); err != nil {
			t.Fatal(err)
		}
		copy(want["state"], []byte("again"))
		if err := reopened.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		want.check(t, reopened, "checkpointed after an abandoned handoff")
		if err := reopened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
