package volume_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// eventLog orders the things one publication does, which is how a test asks
// what came before what without depending on any clock.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func (l *eventLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = nil
}

// sweepStore is an object store whose deletes are slow: it records each one and
// can hold every one of them until the test lets them through, which is what a
// reclamation sweep against a real object store costs.
type sweepStore struct {
	platform.ObjectStore
	log *eventLog
	// deleted carries one value per delete this store is asked for, recorded
	// before the delete itself runs, so a test can wait for a sweep to reach
	// the store rather than guessing when it has.
	deleted chan struct{}

	mu   sync.Mutex
	gate chan struct{}
}

// forget drops what an earlier sweep left behind — the events it recorded and
// the signals it queued — so the window a test is about to watch starts empty.
func (s *sweepStore) forget() {
	s.log.reset()
	for {
		select {
		case <-s.deleted:
		default:
			return
		}
	}
}

// hold blocks every delete from now on and returns the function that lets them
// all through.
func (s *sweepStore) hold() func() {
	gate := make(chan struct{})
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.gate = nil
		s.mu.Unlock()
		close(gate)
	}
}

func (s *sweepStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	s.log.add("delete")
	select {
	case s.deleted <- struct{}{}:
	default:
	}
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	return s.ObjectStore.Delete(ctx, request)
}

// retiringPages is a pager seal that records when it ends. A guest whose seal
// is still standing copies every store it makes into a private page, so the
// interval between the checkpoint becoming durable and this is what the seal
// costs the guest.
type retiringPages struct {
	sealedPages
	log *eventLog
}

func (r *retiringPages) Retire(context.Context, bool) error {
	r.log.add("retire")
	return nil
}

// sweepingManager builds a manager whose control records and checkpoint objects
// go through a store that records its deletes.
func sweepingManager(t *testing.T, h *harness) (*volume.Manager, *sweepStore) {
	t.Helper()
	objects := &sweepStore{ObjectStore: h.objects, log: &eventLog{}, deleted: make(chan struct{}, 64)}
	return h.manager(t, volume.Config{Control: h.controlClient(t, objects),
		Store: h.imageStore(t, objects)}), objects
}

// seal is one volume's sealed pager state over both of the reclamation specs'
// pages, which republishes the whole volume and so leaves the checkpoint it
// replaced holding nothing anyone reads.
func seal(log *eventLog, fill byte) map[string]volume.DirtySource {
	return map[string]volume.DirtySource{"root": &retiringPages{
		sealedPages: sealedPages{size: checkpoint.PageSize, pages: []uint64{0, 1}, fill: fill}, log: log}}
}

// A checkpoint's sealed pages go back to the guest as soon as the checkpoint
// is durable. The reclamation sweep that follows is a run of object-store
// deletes, and holding the seal across it makes the guest copy every store it
// makes for as long as the deletes take.
func TestSealedPagesAreRetiredBeforeTheReclamationSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, objects := sweepingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		defer vm.Close(t.Context())

		first, err := vm.Snapshot(t.Context(), volume.Prepared(nil, seal(objects.log, 0x11)))
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		// The second checkpoint republishes every page, so the first holds
		// nothing anyone reads and the sweep deletes its parts and its index object.
		// The first checkpoint swept too — it reclaimed the root index the
		// create published — so this waits for that sweep to reach the store
		// and then drops everything it left in the window.
		<-objects.deleted
		objects.forget()
		second, err := vm.Snapshot(t.Context(), volume.Prepared(nil, seal(objects.log, 0x22)))
		if err != nil {
			t.Fatal(err)
		}
		if err := second.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The sweep reaching the store is what orders this: the seal must
		// already be over by the time the first delete is asked for.
		<-objects.deleted
		events := objects.log.all()
		if !slices.Contains(events, "delete") {
			t.Fatalf("the second publication reclaimed nothing: %v", events)
		}
		if events[0] != "retire" {
			t.Fatalf("the second publication did %v, want the seal retired before the sweep's deletes", events)
		}
		synctest.Wait()
	})
}

// The sweep is not the publication: it deletes objects nothing reads, and it
// holds neither the publication lock nor the guest. A capture taken while a
// sweep is running gets the lock straight away.
func TestTheReclamationSweepRunsOffThePublicationLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, objects := sweepingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		defer vm.Close(t.Context())
		first, err := vm.Snapshot(t.Context(), volume.Prepared(nil, seal(objects.log, 0x11)))
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// The first checkpoint's own sweep reclaims the root index the create
		// published; this window watches the second's, so wait for that one to
		// reach the store and drop what it left.
		<-objects.deleted
		objects.forget()

		// The second checkpoint's sweep is caught in the middle: every delete it
		// makes is held until this test lets it go.
		release := objects.hold()
		second, err := vm.Snapshot(t.Context(), volume.Prepared(nil, seal(objects.log, 0x22)))
		if err != nil {
			release()
			t.Fatal(err)
		}
		// A capture reports as soon as it holds the publication lock and has
		// frozen its overlays, which is everything that lock is for.
		captured := make(chan *volume.Checkpoint, 1)
		go func() {
			third, err := vm.Snapshot(t.Context(), volume.Prepared(nil, seal(objects.log, 0x33)))
			if err != nil {
				t.Error(err)
			}
			captured <- third
		}()
		// The sweep has reached the store and is held there.
		<-objects.deleted
		synctest.Wait()
		var third *volume.Checkpoint
		select {
		case third = <-captured:
		default:
			t.Errorf("a capture waited for the sweep's deletes to finish")
		}
		release()
		if third == nil {
			third = <-captured
		}
		if third != nil {
			if err := third.Wait(t.Context()); err != nil {
				t.Errorf("the capture taken while the sweep ran: %v", err)
			}
		}
		if err := second.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
	})
}
