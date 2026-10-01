package simtest

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// watchedStore is a store whose every change an ownership check sees, and a
// control client over it.
func watchedStore(t *testing.T) (*sim.ObjectStore, *ownership, *control.Client, string) {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1})
	prefix, err := platform.NewObjectPrefix("deployment/")
	if err != nil {
		t.Fatal(err)
	}
	store := runtime.ObjectStore()
	checked := newOwnership(prefix.String())
	store.Observe(checked.observe)
	client, err := control.NewClient(control.Config{ObjectStore: store, ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return store, checked, client, prefix.String()
}

func indexKey(t *testing.T, prefix, vm string, sequence uint64) platform.ObjectKey {
	t.Helper()
	key, err := platform.NewObjectKey(prefix + "vm/" + vm + "/ckpt/" + strconv.FormatUint(sequence, 10) + "/index")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func put(t *testing.T, store *sim.ObjectStore, key platform.ObjectKey, body []byte) {
	t.Helper()
	if _, err := store.Put(t.Context(), platform.PutRequest{Key: key, Body: bytes.NewReader(body),
		Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}

// read is an object's bytes as the store holds them now.
func read(t *testing.T, store *sim.ObjectStore, key platform.ObjectKey) []byte {
	t.Helper()
	result, err := store.Get(t.Context(), platform.GetRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	data, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The check passes what the code does, and catches each change the spec
// forbids once it is made behind the code's back: a record put back to one
// that selected an older checkpoint, and the index of a pinned or a selected
// checkpoint deleted.
func TestOwnershipCatchesWhatTheSpecForbids(t *testing.T) {
	store, checked, client, prefix := watchedStore(t)
	ctx := t.Context()
	first := control.Sequence(control.MinimumEpoch, 1)
	put(t, store, indexKey(t, prefix, "vm", first), []byte("root"))
	handle, err := client.Create(ctx, "vm", first, true)
	if err != nil {
		t.Fatal(err)
	}
	recordKey, err := platform.NewObjectKey(prefix + control.RecordName("vm"))
	if err != nil {
		t.Fatal(err)
	}
	older := read(t, store, recordKey)
	second := control.Sequence(control.MinimumEpoch, 2)
	put(t, store, indexKey(t, prefix, "vm", second), []byte("second"))
	if _, err := handle.Select(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Pin(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := checked.failures(true); err != nil {
		t.Fatalf("the code's own changes broke the spec: %v", err)
	}
	put(t, store, recordKey, older)
	if err := store.Delete(ctx, platform.DeleteRequest{Key: indexKey(t, prefix, "vm", first)}); err != nil {
		t.Fatal(err)
	}
	err = checked.failures(true)
	for _, want := range []string{"SelectionMoves: a pin was taken away",
		"PinnedReadable: the index of pinned checkpoint", "SelectedReadable: the index of selected checkpoint"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the check reported %v, want %q among it", err, want)
		}
	}
}
