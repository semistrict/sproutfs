package checkpoint

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
)

// readCounter records every read a store makes of object storage: how many,
// and how many bytes each returned.
type readCounter struct {
	platform.ObjectStore
	lengths []int64
}

func (c *readCounter) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	result, err := c.ObjectStore.Get(ctx, request)
	if err == nil {
		c.lengths = append(c.lengths, result.ContentLength)
	}
	return result, err
}

// countedStore is a store whose reads are counted and whose index tail is the
// given number of bytes, so a test chooses whether a root fits the first read.
func countedStore(t *testing.T, tail int64) (*Store, *readCounter) {
	t.Helper()
	store, runtime := tailStore(t)
	counter := &readCounter{ObjectStore: runtime.ObjectStore()}
	store.objects = counter
	store.indexTail = tail
	return store, counter
}

// severalSegments publishes a checkpoint that changes one page of each of a
// handful of volumes, so its index object holds that many segments ahead of its
// root and is several times the size of the root alone.
func severalSegments(t *testing.T, store *Store) control.Ref {
	t.Helper()
	sizes := make(map[string]uint64)
	for number := range 24 {
		sizes[spreadVolumeName(number)] = SectorSize
	}
	root, err := store.Root(t.Context(), control.Ref{VM: "tail", Sequence: 1}, volumesAt(at2MiB, sizes))
	if err != nil {
		t.Fatal(err)
	}
	ref := control.Ref{VM: "tail", Sequence: 2}
	publication := store.Begin(root, ref)
	for name := range sizes {
		publication.Dirty(name, 0)
	}
	if _, err := publication.Commit(t.Context(), fillSource{value: 3}); err != nil {
		t.Fatal(err)
	}
	return ref
}

// indexObjectSize is how many bytes one checkpoint's index object holds.
func indexObjectSize(t *testing.T, store *Store, ref control.Ref) int64 {
	t.Helper()
	key, err := store.indexKey(ref)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := platform.ReadObject(t.Context(), store.objects, key, 1, maximumIndexSize, ErrCorrupt)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(data))
}

// Opening a checkpoint reads its root and nothing else: one read of the end of
// the index object, however many segments lie ahead of the root. What an open
// costs must not grow with what the checkpoint changed, because a 4 KiB-page
// volume writes about 5 MiB of segments for every GiB of it a checkpoint
// dirtied, and every fork, restore and migration opens one.
func TestOpeningACheckpointReadsOnlyTheEndOfItsIndexObject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const tail = 1 << 10
		store, counter := countedStore(t, tail)
		ref := severalSegments(t, store)
		size := indexObjectSize(t, store, ref)
		if size <= tail {
			t.Fatalf("the index object is %d bytes, want more than the %d-byte tail", size, tail)
		}
		counter.lengths = nil
		index, err := store.Open(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if len(counter.lengths) != 1 || counter.lengths[0] != tail {
			t.Fatalf("the open read %v bytes, want one read of the %d-byte tail", counter.lengths, tail)
		}
		// The segments are fetched when a read needs them, and the page with them.
		got := make([]byte, SectorSize)
		if err := store.Read(t.Context(), index, spreadVolumeName(7), 0, got); err != nil {
			t.Fatal(err)
		}
		if got[0] != 3 || got[SectorSize-1] != 3 {
			t.Fatalf("the page reads back as %#x...%#x, want what was published", got[0], got[SectorSize-1])
		}
	})
}

// A root longer than the tail costs one more read, of exactly the bytes of it
// the first read did not bring.
func TestARootLongerThanTheTailIsFetchedInOneMoreRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const tail = 64
		store, counter := countedStore(t, tail)
		ref := severalSegments(t, store)
		whole, _ := countedStore(t, 1<<20)
		whole.objects = store.objects
		want, err := whole.Open(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		counter.lengths = nil
		index, err := store.Open(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if len(counter.lengths) != 2 || counter.lengths[0] != tail {
			t.Fatalf("the open read %v bytes, want the %d-byte tail and then the rest of the root", counter.lengths, tail)
		}
		encoded, err := want.encode()
		if err != nil {
			t.Fatal(err)
		}
		again, err := index.encode()
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(encoded) {
			t.Fatal("the root read in two pieces is not the root read in one")
		}
	})
}

// An index object may be far larger than the 64 MiB that bounded it while every
// page was 2 MiB: a checkpoint that dirtied tens of GiB of a 4 KiB-page volume
// writes hundreds of MiB of segments, and refusing it would stop the VM. The
// segments here are padding, which is all an open needs them to be.
func TestAnIndexObjectPastTheOldBoundPublishesAndOpens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, counter := countedStore(t, defaultIndexTail)
		ref := control.Ref{VM: "large", Sequence: 1}
		index := newIndex(store, ref)
		index.checkpoints[ref] = checkpointCost{}
		object := newIndexObject(store)
		object.data = append(object.data, make([]byte, 65<<20)...)
		data, err := object.seal(t.Context(), index)
		if err != nil {
			t.Fatalf("sealing an index object of %d bytes: %v", len(object.data), err)
		}
		if err := store.putIndexObject(t.Context(), ref, data); err != nil {
			t.Fatal(err)
		}
		counter.lengths = nil
		if _, err := store.Open(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		if len(counter.lengths) != 1 || counter.lengths[0] != defaultIndexTail {
			t.Fatalf("opening a %d-byte index object read %v bytes, want one read of %d",
				len(data), counter.lengths, defaultIndexTail)
		}
	})
}
