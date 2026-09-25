package volume_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

func TestCanceledHandoffRetainsWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		cancel()
		if err := vm.Handoff(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled handoff: %v", err)
		}
		if vm.Status().HandedOff {
			t.Fatal("cancellation gave the writer up")
		}
		body := []byte("still owned after cancellation")
		if err := vm.Volume("root").Write(t.Context(), 0, body); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], body)
		// A handoff publishes nothing, so what the destination sees is what the
		// last checkpoint published.
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Handoff(t.Context()); err != nil {
			t.Fatal(err)
		}
		destination := h.manager(t, h.config())
		defer destination.Close(t.Context())
		opened, err := destination.Open(t.Context(), vm.ID())
		if err != nil {
			t.Fatal(err)
		}
		want.check(t, opened, "retry after canceled handoff")
	})
}

// A handoff waits for a publication another call already had in flight, so a
// checkpoint still uploading when the guest stopped is not left half-selected.
// One canceled while it waits has given nothing up, and a later handoff still
// releases the VM at the checkpoint that publication selected.
func TestHandoffWaitsForAPublicationInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		faults := &faultStore{ObjectStore: h.objects}
		config := h.config()
		config.Control, config.Store = h.controlClient(t, faults), h.imageStore(t, faults)
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		body := []byte("checkpoint before handoff")
		if err := vm.Volume("root").Write(t.Context(), 0, body); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], body)
		held := fmt.Sprintf("/ckpt/%d/", counted(vm, 2))
		release := faults.hold(func(key platform.ObjectKey) bool { return strings.Contains(key.String(), held) })
		defer func() { release() }()
		published := make(chan error, 1)
		go func() { published <- vm.Checkpoint(t.Context()) }()
		synctest.Wait()
		// This write is newer than the held checkpoint, so the handoff leaves it
		// behind: on a real host it is a page the destination fetches.
		tail := []byte("tail after the held checkpoint")
		if err := vm.Volume("state").Write(t.Context(), 0, tail); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		handed := make(chan error, 1)
		go func() { handed <- vm.Handoff(ctx) }()
		synctest.Wait()
		select {
		case err := <-handed:
			t.Fatalf("handoff did not wait for the publication in flight: %v", err)
		default:
		}
		if vm.Status().HandedOff {
			t.Fatal("handoff gave the VM up before the publication in flight landed")
		}
		cancel()
		if err := <-handed; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled handoff: %v", err)
		}
		if vm.Status().HandedOff {
			t.Fatal("a handoff canceled while waiting still gave the VM up")
		}
		release()
		// Avoid releasing the test gate twice when unwinding.
		release = func() {}
		if err := <-published; err != nil {
			t.Fatal(err)
		}
		if err := vm.Handoff(t.Context()); err != nil {
			t.Fatalf("the retried handoff: %v", err)
		}
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("stale")); !errors.Is(err, volume.ErrHandedOff) {
			t.Fatalf("a handed-off handle is still writable: %v", err)
		}
		destination := h.manager(t, h.config())
		defer destination.Close(t.Context())
		opened, err := destination.Open(t.Context(), vm.ID())
		if err != nil {
			t.Fatal(err)
		}
		want.check(t, opened, "the checkpoint the retried handoff released at")
	})
}
