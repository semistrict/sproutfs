package control_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// interposedStore runs a step of another participant just before the first
// write that replaces a record. It is how a test puts that participant's write
// between a pin's read and its conditional write, which is the one window the
// pin's condition exists for.
type interposedStore struct {
	platform.ObjectStore
	before func()
}

func (s *interposedStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if step := s.before; step != nil && request.Conditions.IfMatch != nil {
		s.before = nil
		step()
	}
	return s.ObjectStore.Put(ctx, request)
}

// A VM nobody runs has no writer, so a pin on it is written without one. It
// keeps the record's epoch and nonce: it takes nothing from the writer that
// published the checkpoint, and the next open advances the epoch as it would
// have.
func TestAPinWithoutTheWriterKeepsTheEpoch(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	if _, err := handle.Select(ctx, second); err != nil {
		t.Fatal(err)
	}
	stopped := handle.Record()
	handle.Close()

	pinned, err := client.Pin(ctx, "vm", 0)
	if err != nil {
		t.Fatalf("pinning the checkpoint a stopped VM selects: %v", err)
	}
	if pinned != (control.Ref{VM: "vm", Sequence: second}) {
		t.Fatalf("the pin reports %v, want vm/%d", pinned, second)
	}
	want := control.Record{VM: "vm", Epoch: stopped.Epoch, Nonce: stopped.Nonce,
		Selected: second, Created: true, Pinned: []uint64{second}}
	current, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !equalRecords(current, want) {
		t.Fatalf("the durable record = %+v, want %+v", current, want)
	}
	// Naming the sequence is the same pin, and a pin already there is kept.
	again, err := client.Pin(ctx, "vm", second)
	if err != nil {
		t.Fatal(err)
	}
	if again != pinned {
		t.Fatalf("pinning again reports %v, want %v", again, pinned)
	}
	if current, err = client.Read(ctx, "vm"); err != nil {
		t.Fatal(err)
	}
	if !equalRecords(current, want) {
		t.Fatalf("the durable record after pinning again = %+v, want %+v", current, want)
	}
	// The next open takes the next epoch and carries the pin on.
	reopened, err := client.Open(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Epoch() != stopped.Epoch+1 || !slices.Equal(reopened.Record().Pinned, []uint64{second}) {
		t.Fatalf("the reopened handle is at epoch %d pinning %v", reopened.Epoch(), reopened.Record().Pinned)
	}
}

// A pin without the writer names only what no writer can be reclaiming: the
// published checkpoint the record selects, or one a pin already keeps. Any
// other checkpoint may be in the middle of a sweep that read the pins before
// this one landed, so it is refused rather than pinned too late.
func TestAPinWithoutTheWriterRefusesWhatItCannotKeep(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	if _, err := client.Pin(ctx, "absent", 0); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("pinning a VM with no record = %v, want ErrNotFound", err)
	}
	// A fork whose root has not landed selects a checkpoint that does not exist.
	if _, err := client.Create(ctx, "fork", root(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pin(ctx, "fork", 0); !errors.Is(err, control.ErrNotPublished) {
		t.Fatalf("pinning a fork's unpublished root = %v, want ErrNotPublished", err)
	}
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	if _, err := handle.Select(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pin(ctx, "vm", root()); !errors.Is(err, control.ErrNotPublished) {
		t.Fatalf("pinning a checkpoint the record no longer selects = %v, want ErrNotPublished", err)
	}
	// A checkpoint a fork already pinned stays kept, so it may be named.
	if _, err := handle.Pin(ctx, second); err != nil {
		t.Fatal(err)
	}
	third := control.Sequence(control.MinimumEpoch, 3)
	if _, err := handle.Select(ctx, third); err != nil {
		t.Fatal(err)
	}
	pinned, err := client.Pin(ctx, "vm", second)
	if err != nil {
		t.Fatalf("naming a pinned checkpoint the record no longer selects: %v", err)
	}
	if pinned != (control.Ref{VM: "vm", Sequence: second}) {
		t.Fatalf("the pin reports %v, want vm/%d", pinned, second)
	}
	record, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if record.Selected != third || !slices.Equal(record.Pinned, []uint64{second}) {
		t.Fatalf("the record = %+v, want %d selected and %d pinned", record, third, second)
	}
}

// The writer of a VM that turns out to be running has its record change
// underneath it. Its next write is refused, it adopts the record with the pin,
// and it makes its change again over that: the selection lands, and the record
// it reports — which is what its reclamation spares — carries the pin.
func TestAWriterWhoseRecordGainedAPinCarriesItOn(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pin(ctx, "vm", 0); err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	record, err := handle.Select(ctx, second)
	if err != nil {
		t.Fatalf("selecting over a record another pinned: %v", err)
	}
	if record.Selected != second || !slices.Equal(record.Pinned, []uint64{root()}) {
		t.Fatalf("the selection reported %+v, want %d selected and %d pinned", record, second, root())
	}
	current, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !equalRecords(current, record) {
		t.Fatalf("the durable record = %+v, want %+v", current, record)
	}
}

// A writer that selects another checkpoint between a pin's read and its write
// moves the record, so the pin is refused and reads again. Asked for the
// selected checkpoint, it pins the one the writer just selected, which that
// writer's sweep never deletes. Asked for the one it read, it refuses: the
// writer's sweep may already be deleting it.
func TestAPinRacingASelectionPinsWhatIsSelected(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence uint64
		want     error
	}{
		{name: "selected", sequence: 0},
		{name: "named", sequence: root(), want: control.ErrNotPublished},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: 1})
			prefix, err := platform.NewObjectPrefix("deployment/")
			if err != nil {
				t.Fatal(err)
			}
			store := &interposedStore{ObjectStore: runtime.ObjectStore()}
			client, err := control.NewClient(control.Config{ObjectStore: store, ObjectPrefix: prefix})
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			handle, err := client.Create(ctx, "vm", root(), true)
			if err != nil {
				t.Fatal(err)
			}
			second := control.Sequence(control.MinimumEpoch, 2)
			store.before = func() {
				if _, err := handle.Select(ctx, second); err != nil {
					t.Error(err)
				}
			}
			pinned, err := client.Pin(ctx, "vm", test.sequence)
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("pinning %d across a selection = %v, want %v", test.sequence, err, test.want)
				}
				current, err := client.Read(ctx, "vm")
				if err != nil {
					t.Fatal(err)
				}
				if current.Selected != second || len(current.Pinned) != 0 {
					t.Fatalf("the durable record = %+v, want %d selected and nothing pinned", current, second)
				}
				return
			}
			if err != nil {
				t.Fatalf("pinning across a selection: %v", err)
			}
			if pinned != (control.Ref{VM: "vm", Sequence: second}) {
				t.Fatalf("the pin reports %v, want vm/%d", pinned, second)
			}
			record, err := client.Read(ctx, "vm")
			if err != nil {
				t.Fatal(err)
			}
			if record.Selected != second || !slices.Equal(record.Pinned, []uint64{second}) {
				t.Fatalf("the durable record = %+v, want %d selected and pinned", record, second)
			}
		})
	}
}

// A pin whose reply is lost has landed if the record read back pins the
// checkpoint, whoever's write that was: pins are only ever added.
func TestALostPinReplyIsReconciledByThePin(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	handle.Close()
	store.FailNextAfterApply(sim.ObjectPut, 1)
	pinned, err := client.Pin(ctx, "vm", 0)
	if err != nil {
		t.Fatalf("a pin whose reply was lost reported %v, want the pin it made", err)
	}
	if pinned != (control.Ref{VM: "vm", Sequence: root()}) {
		t.Fatalf("the reconciled pin reports %v, want vm/%d", pinned, root())
	}
	record, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(record.Pinned, []uint64{root()}) {
		t.Fatalf("the durable record pins %v, want %d", record.Pinned, root())
	}
}

func equalRecords(a, b control.Record) bool {
	return a.VM == b.VM && a.Epoch == b.Epoch && bytes.Equal(a.Nonce, b.Nonce) &&
		a.Selected == b.Selected && a.Created == b.Created && slices.Equal(a.Pinned, b.Pinned)
}

// A pin added without the epoch refuses the writer's next write just as a
// takeover does. So a refusal whose record then cannot be read is no evidence
// of a takeover, and fencing on it would give up a running guest for a pin.
// The writer keeps its epoch, and its next write reads what happened.
func TestARefusalThatCannotBeReadDoesNotFenceTheWriter(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pin(ctx, "vm", 0); err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	store.FailNext(sim.ObjectGet, 1)
	if _, err := handle.Select(ctx, second); !errors.Is(err, platform.ErrPrecondition) ||
		errors.Is(err, control.ErrFenced) {
		t.Fatalf("a refused selection whose record could not be read = %v, want the refusal and no fence", err)
	}
	record, err := handle.Select(ctx, second)
	if err != nil {
		t.Fatalf("repeating the selection: %v", err)
	}
	if record.Selected != second || !slices.Equal(record.Pinned, []uint64{root()}) {
		t.Fatalf("the selection reported %+v, want %d selected and %d pinned", record, second, root())
	}
}

func (s *interposedStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	if step := s.before; step != nil && request.IfMatch != nil {
		s.before = nil
		step()
	}
	return s.ObjectStore.Delete(ctx, request)
}

// A delete reads the record for the pins its sweep spares, and then removes
// it. A pin added without the writer between the two would be lost to the
// sweep, which would delete the checkpoint a new VM had just been created
// from. So the removal is conditional on the record the delete read, and a
// record that moved is read again: the pins it reports include the new one.
func TestARemovalReportsAPinAddedAfterItsRead(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1})
	prefix, err := platform.NewObjectPrefix("deployment/")
	if err != nil {
		t.Fatal(err)
	}
	store := &interposedStore{ObjectStore: runtime.ObjectStore()}
	client, err := control.NewClient(control.Config{ObjectStore: store, ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	handle.Close()
	store.before = func() {
		if _, err := client.Pin(ctx, "vm", 0); err != nil {
			t.Error(err)
		}
	}
	removed, err := client.Remove(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed.Pinned, []uint64{root()}) {
		t.Fatalf("the removed record pins %v, want %d", removed.Pinned, root())
	}
	if _, err := client.Read(ctx, "vm"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("reading a removed record = %v, want ErrNotFound", err)
	}
	if _, err := client.Remove(ctx, "vm"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("removing a record that is gone = %v, want ErrNotFound", err)
	}
}
