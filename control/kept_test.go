package control_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// keptClient is a client whose kept checkpoints are dated by a simulated
// clock, so a test can name the time a keep records.
func keptClient(t *testing.T) (*control.Client, *sim.Clock, *interposedStore) {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1})
	prefix, err := platform.NewObjectPrefix("deployment/")
	if err != nil {
		t.Fatal(err)
	}
	clock := runtime.NewClock("control")
	store := &interposedStore{ObjectStore: runtime.ObjectStore()}
	client, err := control.NewClient(control.Config{ObjectStore: store, ObjectPrefix: prefix, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return client, clock, store
}

// A selection that keeps its checkpoint records it in the same write, with the
// time it was selected and whether it holds VMM state. Later selections carry
// it on, reclamation is told to spare it, and keeping it again writes nothing.
func TestASelectionThatKeepsRecordsTheCheckpointKept(t *testing.T) {
	client, clock, _ := keptClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	third := control.Sequence(control.MinimumEpoch, 3)
	keptAt := clock.Now().UTC()
	record, err := handle.SelectKept(ctx, second, true)
	if err != nil {
		t.Fatalf("selecting a kept checkpoint: %v", err)
	}
	want := []control.Kept{{Sequence: second, Time: keptAt, State: true}}
	if record.Selected != second || !keptEqual(record.Kept, want) {
		t.Fatalf("the selection reported %+v, want %d selected and kept as %+v", record, second, want)
	}
	clock.Advance(time.Minute)
	if again, err := handle.SelectKept(ctx, second, true); err != nil || !keptEqual(again.Kept, want) {
		t.Fatalf("keeping the selected checkpoint again = %+v, %v, want it kept as it was", again.Kept, err)
	}
	record, err = handle.Select(ctx, third)
	if err != nil {
		t.Fatal(err)
	}
	if !keptEqual(record.Kept, want) || !slices.Equal(record.Protected(), []uint64{second}) {
		t.Fatalf("the next selection keeps %+v and protects %v, want %+v and %d",
			record.Kept, record.Protected(), want, second)
	}
	durable, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !equalRecords(durable, record) {
		t.Fatalf("the durable record = %+v, want %+v", durable, record)
	}
}

// A kept checkpoint can be forked without the writer long after the record
// selects another. The fork pins it, and from then on it cannot be released:
// the pin is what a descendant reads through.
func TestAForkedKeptCheckpointCannotBeReleased(t *testing.T) {
	client, _, _ := keptClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	third := control.Sequence(control.MinimumEpoch, 3)
	if _, err := handle.SelectKept(ctx, second, false); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Select(ctx, third); err != nil {
		t.Fatal(err)
	}
	handle.Close()
	pinned, err := client.Pin(ctx, "vm", second)
	if err != nil {
		t.Fatalf("pinning a kept checkpoint the record no longer selects: %v", err)
	}
	if pinned != (control.Ref{VM: "vm", Sequence: second}) {
		t.Fatalf("the pin reports %v, want vm/%d", pinned, second)
	}
	if _, err := client.Release(ctx, "vm", second); !errors.Is(err, control.ErrForked) {
		t.Fatalf("releasing a forked kept checkpoint = %v, want ErrForked", err)
	}
	if _, err := client.Release(ctx, "vm", third); !errors.Is(err, control.ErrNotKept) {
		t.Fatalf("releasing a checkpoint nothing kept = %v, want ErrNotKept", err)
	}
	record, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !record.IsKept(second) || !slices.Equal(record.Pinned, []uint64{second}) {
		t.Fatalf("the record keeps %+v and pins %v, want %d kept and pinned", record.Kept, record.Pinned, second)
	}
}

// A kept checkpoint no fork was taken from is released without the writer.
// From then on nothing may pin it, because a sweep may be deleting it.
func TestAReleasedCheckpointCanNoLongerBeForked(t *testing.T) {
	client, _, _ := keptClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	third := control.Sequence(control.MinimumEpoch, 3)
	if _, err := handle.SelectKept(ctx, second, true); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Select(ctx, third); err != nil {
		t.Fatal(err)
	}
	released, err := client.Release(ctx, "vm", second)
	if err != nil {
		t.Fatalf("releasing a kept checkpoint: %v", err)
	}
	if len(released.Kept) != 0 || released.Epoch != handle.Epoch() || released.Selected != third {
		t.Fatalf("the release left %+v, want nothing kept at epoch %d selecting %d",
			released, handle.Epoch(), third)
	}
	if _, err := client.Pin(ctx, "vm", second); !errors.Is(err, control.ErrNotPublished) {
		t.Fatalf("pinning a released checkpoint = %v, want ErrNotPublished", err)
	}
	// The writer's next write is refused against the record it tracked, and it
	// adopts the release rather than keeping the checkpoint again.
	fourth := control.Sequence(control.MinimumEpoch, 4)
	record, err := handle.Select(ctx, fourth)
	if err != nil {
		t.Fatalf("selecting over a record a release moved: %v", err)
	}
	if record.Selected != fourth || len(record.Kept) != 0 {
		t.Fatalf("the selection reported %+v, want %d selected and nothing kept", record, fourth)
	}
}

// A pin and a release of one kept checkpoint are ordered by the record. A pin
// that lands between a release's read and its write makes the release read
// again and be refused; a release that lands between a pin's read and its
// write makes the pin read again and be refused.
func TestAPinAndAReleaseOfOneCheckpointAreOrdered(t *testing.T) {
	second := control.Sequence(control.MinimumEpoch, 2)
	third := control.Sequence(control.MinimumEpoch, 3)
	setup := func(t *testing.T) (*control.Client, *interposedStore) {
		client, _, store := keptClient(t)
		handle, err := client.Create(t.Context(), "vm", root(), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.SelectKept(t.Context(), second, true); err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Select(t.Context(), third); err != nil {
			t.Fatal(err)
		}
		handle.Close()
		return client, store
	}
	t.Run("pin first", func(t *testing.T) {
		client, store := setup(t)
		store.before = func() {
			if _, err := client.Pin(t.Context(), "vm", second); err != nil {
				t.Error(err)
			}
		}
		if _, err := client.Release(t.Context(), "vm", second); !errors.Is(err, control.ErrForked) {
			t.Fatalf("a release a pin overtook = %v, want ErrForked", err)
		}
	})
	t.Run("release first", func(t *testing.T) {
		client, store := setup(t)
		store.before = func() {
			if _, err := client.Release(t.Context(), "vm", second); err != nil {
				t.Error(err)
			}
		}
		if _, err := client.Pin(t.Context(), "vm", second); !errors.Is(err, control.ErrNotPublished) {
			t.Fatalf("a pin a release overtook = %v, want ErrNotPublished", err)
		}
		record, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Kept) != 0 || len(record.Pinned) != 0 {
			t.Fatalf("the record keeps %+v and pins %v, want neither", record.Kept, record.Pinned)
		}
	})
}

// A release whose reply is lost has landed if the record read back no longer
// keeps the checkpoint.
func TestALostReleaseReplyIsReconciled(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	if _, err := handle.SelectKept(ctx, second, false); err != nil {
		t.Fatal(err)
	}
	handle.Close()
	store.FailNextAfterApply(sim.ObjectPut, 1)
	released, err := client.Release(ctx, "vm", second)
	if err != nil {
		t.Fatalf("a release whose reply was lost reported %v, want the record it left", err)
	}
	if released.IsKept(second) || released.Selected != second {
		t.Fatalf("the reconciled release reports %+v, want %d selected and not kept", released, second)
	}
}

func keptEqual(a, b []control.Kept) bool {
	return slices.EqualFunc(a, b, func(x, y control.Kept) bool {
		return x.Sequence == y.Sequence && x.Time.Equal(y.Time) && x.State == y.State
	})
}
