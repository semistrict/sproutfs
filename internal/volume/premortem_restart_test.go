package volume_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/resource"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The pre-mortem of the GCE soak's stops and starts. A round stops a VM and
// starts it on the other host; a later round stops it there and starts it back
// on the first, which still holds everything of it that was warm before the
// stop. The page cache is one host's, keyed by page identity and never
// cleared between the incarnations of a VM, so a start on a host that ran that
// VM before reads through entries an earlier writer's epoch put there.

// premortemHost is one host of the pre-mortem: a checkpoint store with a page
// cache of its own, which every incarnation of every VM on that host shares and
// which nothing clears between them.
type premortemHost struct {
	store *checkpoint.Store
	cache *checkpoint.Cache
}

func newPremortemHost(t *testing.T, h *harness) *premortemHost {
	t.Helper()
	budget, err := resource.New(64 * checkpoint.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(budget, checkpoint.CacheConfig{MaxConcurrentLoads: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: h.objects,
		ObjectPrefix: h.prefix, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	return &premortemHost{store: store, cache: cache}
}

// open is one incarnation of a VM on this host: a manager of its own over the
// host's one checkpoint store, which is what a start after a stop is.
func (p *premortemHost) open(t *testing.T, h *harness, id string) (*volume.Manager, *volume.VM) {
	t.Helper()
	config := h.config()
	config.Store = p.store
	manager := h.manager(t, config)
	vm, err := manager.Open(t.Context(), id)
	if err != nil {
		t.Fatalf("starting %s: %v", id, err)
	}
	return manager, vm
}

// TestPremortemAVMStartedBackOnAHostThatRanItReadsTheOtherHostsWrites walks the
// soak's stop and start: a VM runs on one host, is stopped and started on the
// other, mutates there, is stopped again and started back on the first — which
// still holds the page cache entries of its first incarnation. Every page has
// to read as what the writer before it published, whether that writer was this
// host or the other one, and a page nothing rewrote has to read as the bytes of
// the incarnation that did write it.
func TestPremortemAVMStartedBackOnAHostThatRanItReadsTheOtherHostsWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		first, second := newPremortemHost(t, h), newPremortemHost(t, h)

		// The VM is created and filled on the first host, then stopped: a stop
		// publishes everything the guest holds and closes the handle.
		config := h.config()
		config.Store = first.store
		creating := h.manager(t, config)
		vm, err := creating.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		writePage(t, vm, 0, 1)
		writePage(t, vm, 1, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Reading both pages here is what puts this host's cache entries under
		// the first incarnation's identities.
		checkPages(t, vm, "the first incarnation", [2]byte{1, 2})
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := creating.Close(t.Context()); err != nil {
			t.Fatal(err)
		}

		// Started on the other host, where it rewrites page zero and leaves page
		// one alone, and stopped again.
		away, moved := second.open(t, h, "vm")
		checkPages(t, moved, "started on the other host", [2]byte{1, 2})
		writePage(t, moved, 0, 3)
		if err := moved.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := moved.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := away.Close(t.Context()); err != nil {
			t.Fatal(err)
		}

		// And started back on the host that ran it first, whose cache still
		// holds the page zero of the incarnation before last under its own
		// identity. Page zero must read as what the other host published and
		// page one as what this host published two incarnations ago.
		back, again := first.open(t, h, "vm")
		checkPages(t, again, "started back on the first host", [2]byte{3, 2})
		// One more round of the same, the other way about, so the host that
		// wrote a page last is the one whose cache is warm for it.
		writePage(t, again, 1, 4)
		if err := again.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := again.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := back.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		last, finally := second.open(t, h, "vm")
		checkPages(t, finally, "started on the other host again", [2]byte{3, 4})
		if err := finally.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := last.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		premortemCheck(t, h, "after four incarnations")
	})
}

// TestPremortemAStopSerialisesBehindAPublicationInFlight is the soak's stop
// landing on a VM whose interval checkpoint is already publishing: the two
// serialize on the VM's publication lock rather than sealing the same guest
// twice, and what the stop publishes holds everything the guest wrote, both
// what the publication in flight had captured and what was written after it.
func TestPremortemAStopSerialisesBehindAPublicationInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		faults := &faultStore{ObjectStore: h.objects}
		store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: faults, ObjectPrefix: h.prefix})
		if err != nil {
			t.Fatal(err)
		}
		config := h.config()
		config.Store = store
		config.Control = h.controlClient(t, faults)
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		writePage(t, vm, 0, 1)
		writePage(t, vm, 1, 2)

		// The interval checkpoint: captured, and held on the way to its index
		// object, which is the publication in flight a stop arrives during.
		release := faults.hold(func(key platform.ObjectKey) bool {
			return bytes.HasSuffix([]byte(key.String()), []byte("/index"))
		})
		interval, err := vm.Snapshot(t.Context(), volume.Prepared([]byte("interval"), nil))
		if err != nil {
			t.Fatal(err)
		}
		// The guest goes on writing behind the publication, which is what the
		// stop's own capture has to publish.
		writePage(t, vm, 1, 3)

		// The stop's final checkpoint, which cannot even be captured until the
		// publication in flight has given the lock up.
		stopped := make(chan error, 1)
		var final *volume.Checkpoint
		var once sync.Once
		go func() {
			ckpt, err := vm.Snapshot(context.WithoutCancel(t.Context()),
				volume.Prepared([]byte("stop"), nil))
			once.Do(func() { final = ckpt })
			stopped <- err
		}()
		synctest.Wait()
		select {
		case err := <-stopped:
			t.Fatalf("the stop's checkpoint did not wait for the publication in flight: %v", err)
		default:
		}
		release()
		if err := interval.Wait(t.Context()); err != nil {
			t.Fatalf("the interval checkpoint: %v", err)
		}
		if err := <-stopped; err != nil {
			t.Fatalf("the stop's checkpoint: %v", err)
		}
		if err := final.Wait(t.Context()); err != nil {
			t.Fatalf("publishing the stop's checkpoint: %v", err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		// What the stop published is what a start reads, and it holds the write
		// that happened while the interval checkpoint was in flight.
		readsPages(t, h, "vm", [2]byte{1, 3})
		premortemCheck(t, h, "after a stop behind a publication")
	})
}
