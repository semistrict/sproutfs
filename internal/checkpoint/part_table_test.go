package checkpoint

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint/internal/part"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// tailStore is a store over a simulated object store whose runtime the test
// keeps, so that what one read costs can be counted out of the trace.
func tailStore(t *testing.T) (*Store, *sim.Runtime) {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 104})
	prefix, err := platform.NewObjectPrefix(fixturePrefix)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Config{ObjectStore: runtime.ObjectStore(), ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return store, runtime
}

// spreadVolumes is how many volumes the checkpoint below fills one page of. A
// volume name may be maximumName bytes, so one entry costs a little under 300
// of them and this is comfortably more table than a part is allowed; every
// volume is one sector long, so the checkpoint costs kilobytes rather than
// gigabytes.
const spreadVolumes = 1400

// spreadVolumeName is a volume name of the longest kind a checkpoint admits.
func spreadVolumeName(number int) string {
	name := fmt.Sprintf("volume-%04d-", number)
	return name + strings.Repeat("x", maximumName-len(name))
}

// A checkpoint whose members' entries alone exceed the table bound spreads them
// over several parts rather than writing one unbounded table. Their bodies are
// a sector each, so nothing but the table's own bound seals those parts — and
// that bound is what lets every part's table be read as one suffix of
// maximumTableSize + TrailerSize bytes.
func TestMembersPastTheTableBoundSealTheirPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _ := tailStore(t)
		sizes := make(map[string]uint64, spreadVolumes)
		for number := range spreadVolumes {
			sizes[spreadVolumeName(number)] = SectorSize
		}
		root, err := store.Root(t.Context(), control.Ref{VM: "spread", Sequence: 1}, volumesAt(at2MiB, sizes))
		if err != nil {
			t.Fatal(err)
		}
		filledRef := control.Ref{VM: "spread", Sequence: 2}
		filling := store.Begin(root, filledRef)
		for name := range sizes {
			filling.Dirty(name, 0)
		}
		filled, err := filling.Commit(t.Context(), fillSource{value: 1})
		if err != nil {
			t.Fatal(err)
		}
		entry := filled.checkpoints[filledRef]
		if entry.parts < 2 {
			t.Fatalf("a checkpoint filling %d volumes wrote %d part(s), want its members spread over several",
				spreadVolumes, entry.parts)
		}
		members := 0
		for number := range entry.parts {
			table, err := store.readPartTable(t.Context(), filledRef, number)
			if err != nil {
				t.Fatal(err)
			}
			members += len(table.members)
			if bytes := partTableBytes(t, store, filledRef, number); bytes > maximumTableSize {
				t.Fatalf("part %d holds a table of %d bytes, want no more than %d",
					number, bytes, maximumTableSize)
			}
		}
		// One page per volume, and nothing else: the segments locating them are
		// the index object's.
		if members != spreadVolumes {
			t.Fatalf("the checkpoint's parts hold %d members, want %d", members, spreadVolumes)
		}
	})
}

// partTableBytes is the encoded size of one part's table, read out of the
// part's own trailer.
func partTableBytes(t *testing.T, store *Store, ref control.Ref, number uint32) uint64 {
	t.Helper()
	key, err := store.partKey(ref, number)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := platform.ReadObject(t.Context(), store.objects, key, 0, maximumPartSize, ErrCorrupt)
	if err != nil {
		t.Fatal(err)
	}
	trailer, err := part.DecodeTrailer(data[len(data)-part.TrailerSize:], uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return trailer.TableLength
}

// Reading a part's table costs one request. The tail a table can occupy is
// bounded, so a reader fetches it as one suffix range rather than heading the
// object for its size and then reading the trailer and the table it locates.
func TestReadingAPartTableIsOneRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, runtime := tailStore(t)
		ref := control.Ref{VM: "tail", Sequence: 1}
		root, err := store.Root(t.Context(), ref, volumesAt(at2MiB, map[string]uint64{"disk": 2 * PageSize2MiB}))
		if err != nil {
			t.Fatal(err)
		}
		publication := store.Begin(root, control.Ref{VM: "tail", Sequence: 2})
		publication.Dirty("disk", 0)
		publication.Dirty("disk", 1)
		if _, err := publication.Commit(t.Context(), fillSource{value: 7}); err != nil {
			t.Fatal(err)
		}
		runtime.Trace().Reset()
		table, err := store.readPartTable(t.Context(), control.Ref{VM: "tail", Sequence: 2}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(table.members) != 2 {
			t.Fatalf("the part names %d members, want the two pages", len(table.members))
		}
		var operations []string
		for _, event := range runtime.Trace().Events() {
			if event.Kind == "object_store" {
				operations = append(operations, event.Operation)
			}
		}
		if len(operations) != 1 || operations[0] != string(sim.ObjectGet) {
			t.Fatalf("reading a part's table issued %v, want one get", operations)
		}
	})
}
