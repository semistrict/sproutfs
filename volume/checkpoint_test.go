package volume_test

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// dirtyEveryPage writes one sector into every page of the test VM's volumes, so
// a checkpoint of it uploads exactly four page objects and one index.
func dirtyEveryPage(t *testing.T, vm *volume.VM, want model, fill byte) {
	t.Helper()
	sector := bytes.Repeat([]byte{fill}, checkpoint.SectorSize)
	for _, offset := range []uint64{0, checkpoint.PageSize2MiB, 2 * checkpoint.PageSize2MiB} {
		if err := vm.Volume("root").Write(t.Context(), offset, sector); err != nil {
			t.Fatal(err)
		}
		copy(want["root"][offset:], sector)
	}
	if err := vm.Volume("state").Write(t.Context(), 0, sector); err != nil {
		t.Fatal(err)
	}
	copy(want["state"], sector)
}

// Closing a VM with dirty data publishes a final checkpoint, so a reopen finds
// everything in the selected index and replays nothing.
func TestCloseCheckpointsDirtyState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		// The checkpoint the close publishes, named from the handle that
		// publishes it: reopening advances the epoch, the sequence does not.
		closed := counted(vm, 2)
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("closing")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], []byte("closing"))
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		status := reopened.Status()
		if status.Checkpoint != (control.Ref{VM: "vm", Sequence: closed}) || status.DirtyBytes != 0 {
			t.Fatalf("Status after reopen = %+v, want the checkpoint the close published", status)
		}
		want.check(t, reopened, "after close and reopen")
		if err := reopened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if status := reopened.Status(); status.Checkpoint != (control.Ref{VM: "vm", Sequence: closed}) {
			t.Fatalf("a clean close published a checkpoint: %+v", status)
		}
	})
}

// A checkpoint interrupted at any upload, at its index, or at the control
// record it selects with leaves the VM readable, lets a retry succeed, and
// survives a reopen. Nothing partially published is ever selected.
func TestCheckpointInterruptedAtEveryStep(t *testing.T) {
	// One part per dirty page, four of them, then the index, then the control
	// record.
	for stage := 1; stage <= 6; stage++ {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("stage%d/after%t", stage, after), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					h := newHarness(t)
					defer h.close(t.Context())
					fault := &faultStore{ObjectStore: h.runtime.ObjectStore()}
					store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: fault, ObjectPrefix: h.prefix, Concurrency: 1, PartBytes: 1})
					if err != nil {
						t.Fatal(err)
					}
					config := h.config()
					config.Control, config.Store = h.controlClient(t, fault), store
					manager := h.manager(t, config)
					defer manager.Close(t.Context())
					vm, want := createVM(t, manager, "vm")
					dirtyEveryPage(t, vm, want, 4)

					fault.arm(stage, after)
					checkpointErr := vm.Checkpoint(t.Context())
					if !fault.disarm() {
						t.Fatalf("stage %d was never reached", stage)
					}
					want.check(t, vm, "after the interrupted checkpoint")
					published := counted(vm, 2)
					if checkpointErr != nil {
						if !errors.Is(checkpointErr, platform.ErrInjectedFault) {
							t.Fatalf("interrupted checkpoint = %v, want the injected fault", checkpointErr)
						}
						if err := vm.Checkpoint(t.Context()); err != nil {
							t.Fatalf("retry after stage %d: %v", stage, err)
						}
						// The interrupted publication burnt its sequence: a
						// retry never republishes under a reference objects may
						// already be sitting under.
						published = counted(vm, 3)
					}
					status := vm.Status()
					if status.Checkpoint != (control.Ref{VM: "vm", Sequence: published}) || status.DirtyBytes != 0 {
						t.Fatalf("Status after the retry = %+v", status)
					}
					dirtyEveryPage(t, vm, want, 5)
					if err := vm.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					reopened, err := manager.Open(t.Context(), "vm")
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close(t.Context())
					want.check(t, reopened, "after reopening")
				})
			})
		}
	}
}

// A page the guest changed is republished whole from the checkpoint, and a page
// all of whose bytes were discarded leaves the index entirely.
func TestPublishingRewritesAndDropsWholePages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		state := vm.Volume("state")
		tail := bytes.Repeat([]byte{5}, int(state.Size()-checkpoint.PageSize2MiB))
		if err := state.Write(t.Context(), checkpoint.PageSize2MiB, tail); err != nil {
			t.Fatal(err)
		}
		copy(want["state"][checkpoint.PageSize2MiB:], tail)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		want.check(t, vm, "after rewriting the tail page")

		if err := state.Discard(t.Context(), checkpoint.PageSize2MiB, state.Size()-checkpoint.PageSize2MiB); err != nil {
			t.Fatal(err)
		}
		clear(want["state"][checkpoint.PageSize2MiB:])
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		want.check(t, reopened, "after the discarded page left the index")
		dropped, err := reopened.Volume("state").Locate(t.Context(), checkpoint.PageSize2MiB, state.Size()-checkpoint.PageSize2MiB)
		if err != nil {
			t.Fatal(err)
		}
		if len(dropped) != 1 || dropped[0].Identity != (control.Identity{Zero: true}) {
			t.Fatalf("Locate of the dropped page = %+v, want one zero extent", dropped)
		}
	})
}

// A checkpoint whose control record was taken by a later writer selects
// nothing and turns its handle terminal. Everything the fenced handle held but
// had not published is lost, which is exactly what losing its host would cost.
func TestCheckpointFencedByALaterWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		dirtyEveryPage(t, vm, want, 6)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		published := vm.Status().Checkpoint
		// Dirty again, so the fenced handle holds bytes nothing has published.
		dirtyEveryPage(t, vm, newModel(testSpecs), 7)

		next, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer next.Close(t.Context())
		if err := vm.Checkpoint(t.Context()); !errors.Is(err, control.ErrFenced) {
			t.Fatalf("checkpoint from the fenced handle = %v, want ErrFenced", err)
		}
		if status := vm.Status(); status.Checkpoint != published || !errors.Is(status.Err, volume.ErrNeedsRecovery) {
			t.Fatalf("Status of the fenced handle = %+v", status)
		}
		// The takeover sits on the last selected checkpoint, and the fenced
		// handle's unpublished bytes are gone.
		if status := next.Status(); status.Checkpoint != published {
			t.Fatalf("the takeover opened %+v, want %v", status, published)
		}
		want.check(t, next, "the checkpoint the fenced handle had published")
	})
}

// A guest's writes never reach the overlay, so a pager-backed VM's write
// generation never moves and a failed publication must not hand its sequence
// back: the next checkpoint seals a different dirty set, and republishing it
// under a reference the failed one already uploaded a part for conflicts
// forever. Every checkpoint after a failure lands under its own sequence and
// advances the VM's durability.
func TestFailedPublicationBurnsItsSequence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		fault := &faultStore{ObjectStore: h.runtime.ObjectStore()}
		config := h.config()
		config.Control, config.Store = h.controlClient(t, fault), h.imageStore(t, fault)
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())

		// The index object of the first checkpoint this handle takes, and that
		// one only: its parts land, its index object does not.
		failed := control.Ref{VM: "vm", Sequence: counted(vm, 2)}
		denied := false
		fault.block(func(key platform.ObjectKey) bool {
			if denied || !strings.HasSuffix(key.String(), "/ckpt/"+strconv.FormatUint(failed.Sequence, 10)+"/index") {
				return false
			}
			denied = true
			return true
		})

		ckpt, err := vm.Snapshot(t.Context(), volume.Prepared(nil,
			map[string]volume.DirtySource{"root": sealedPages{size: checkpoint.PageSize2MiB, pages: []uint64{0}, fill: 0xa1}}))
		if err != nil {
			t.Fatal(err)
		}
		if got := ckpt.Ref(); got != failed {
			t.Fatalf("the first checkpoint took %v, want %v", got, failed)
		}
		if err := ckpt.Wait(t.Context()); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("the interrupted publication = %v, want the injected fault", err)
		}
		if status := vm.Status(); status.Checkpoint.Sequence != counted(vm, 1) {
			t.Fatalf("Status after the failure = %+v, want the root checkpoint still selected", status)
		}

		// Three more checkpoints of the running guest, each sealing a different
		// dirty set, exactly as the interval loop takes them.
		for offset, sealed := range []sealedPages{
			{size: checkpoint.PageSize2MiB, pages: []uint64{1}, fill: 0xa2},
			{size: checkpoint.PageSize2MiB, pages: []uint64{2}, fill: 0xa3},
			{size: checkpoint.PageSize2MiB, pages: []uint64{0, 1}, fill: 0xa4},
		} {
			want := control.Ref{VM: "vm", Sequence: counted(vm, uint64(3+offset))}
			next, err := vm.Snapshot(t.Context(), volume.Prepared(nil, map[string]volume.DirtySource{"root": sealed}))
			if err != nil {
				t.Fatalf("checkpoint %d after the failure: %v", offset, err)
			}
			if got := next.Ref(); got != want {
				t.Fatalf("checkpoint %d after the failure took %v, want %v", offset, got, want)
			}
			if err := next.Wait(t.Context()); err != nil {
				t.Fatalf("publishing checkpoint %d after the failure: %v", offset, err)
			}
			if status := vm.Status(); status.Checkpoint != want || status.CheckpointError != nil {
				t.Fatalf("Status after checkpoint %d = %+v, want %v", offset, status, want)
			}
		}

		// The bytes the later checkpoints published are what the VM reads, and
		// the page the failed one held is still unwritten.
		got := make([]byte, 3*checkpoint.PageSize2MiB)
		if err := vm.Volume("root").Read(t.Context(), 0, got); err != nil {
			t.Fatal(err)
		}
		want := append(bytes.Repeat([]byte{0xa4}, 2*checkpoint.PageSize2MiB), bytes.Repeat([]byte{0xa3}, checkpoint.PageSize2MiB)...)
		if !bytes.Equal(got, want) {
			t.Fatalf("root after the checkpoints that followed the failure differs from what they published")
		}
	})
}

// A checkpoint of the disks alone names no VMM state, not even its parent's: the
// registers of the capture before it, over the disks of this one, are a guest
// that never existed.
func TestSnapshotDisksNamesNoState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		store := h.imageStore(t, h.objects)

		full, err := vm.Snapshot(t.Context(), volume.Prepared([]byte("registers"), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := full.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("disk only")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], []byte("disk only"))
		disks, err := vm.SnapshotDisks(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := disks.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		index, err := store.Open(t.Context(), vm.Status().Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if state, err := store.ReadState(t.Context(), index); !errors.Is(err, checkpoint.ErrNoState) {
			t.Fatalf("a checkpoint of the disks names state %q (%v), want none", state, err)
		}
		want.checkCheckpoint(t, disks, "the checkpoint of the disks")

		if _, err := vm.SnapshotDisks(t.Context(), volume.Prepared([]byte("registers"), nil)); !errors.Is(err, volume.ErrInvalidConfig) {
			t.Fatalf("a checkpoint of the disks that captured state gave %v, want ErrInvalidConfig", err)
		}
	})
}
