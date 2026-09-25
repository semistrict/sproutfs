package checkpoint_test

import (
	"bytes"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
)

func TestCompressedCheckpointObjectsAndPartialReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		sizes := map[string]uint64{"ram0": 2 << 20}
		root, err := store.Root(t.Context(), control.Ref{VM: "compressed", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(volumes2MiB(sizes))
		p := store.Begin(root, control.Ref{VM: "compressed", Sequence: 2})
		for sector := range uint32(512) {
			m.dirty(p, "ram0", 0, sector, bytes.Repeat([]byte{byte(sector/256 + 1)}, 4096))
		}
		state := bytes.Repeat([]byte("vmm state "), 10000)
		p.SetState(state)
		index, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		// One part holds the whole checkpoint's data: a 2 MiB page and
		// 100 KB of state, both repetitive, and the table naming them.
		metadata, err := objects.Head(t.Context(), partKey(t, "compressed", 2, 0))
		if err != nil {
			t.Fatal(err)
		}
		if metadata.Size >= 10000 {
			t.Fatalf("the part was not compressed: %d bytes", metadata.Size)
		}
		if present(t, objects, partKey(t, "compressed", 2, 1)) {
			t.Fatal("one 2 MiB page produced two parts")
		}
		// Read across the old 1 MiB boundary and the last sector of the new page.
		for _, offset := range []uint64{(1 << 20) - 16, (2 << 20) - 32} {
			got := make([]byte, 32)
			if err := store.Read(t.Context(), index, "ram0", offset, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, m.contents["ram0"][offset:offset+32]) {
				t.Fatal("partial read returned the wrong bytes")
			}
		}
		// A store into the last sector republishes the whole 2 MiB page.
		child := store.Begin(index, control.Ref{VM: "compressed", Sequence: 3})
		m.dirty(child, "ram0", 0, 511, bytes.Repeat([]byte{9}, 4096))
		latest, err := child.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		metadata, err = objects.Head(t.Context(), partKey(t, "compressed", 3, 0))
		if err != nil || metadata.Size >= 10000 {
			t.Fatalf("republished page was not compressed: %+v, %v", metadata, err)
		}
		reopened, err := store.Open(t.Context(), latest.Ref())
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 2<<20)
		if err := store.Read(t.Context(), reopened, "ram0", 0, got); err != nil || !bytes.Equal(got, m.contents["ram0"]) {
			t.Fatalf("republished page: %v", err)
		}
		gotState, err := store.ReadState(t.Context(), index)
		if err != nil || !bytes.Equal(gotState, state) {
			t.Fatalf("state round trip: %v", err)
		}
		// An identical retry is accepted, but equal-length different bytes conflict.
		if _, err := child.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		other := m.clone()
		conflict := store.Begin(index, latest.Ref())
		other.dirty(conflict, "ram0", 0, 511, bytes.Repeat([]byte{8}, 4096))
		if _, err := conflict.Commit(t.Context(), other); !errors.Is(err, checkpoint.ErrConflict) {
			t.Fatalf("same-length replacement accepted: %v", err)
		}
	})
}
