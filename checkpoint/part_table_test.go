package checkpoint

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint/internal/part"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
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

// spreadVolumeName is a volume name of the longest kind a checkpoint admits,
// which makes every member of that volume the widest entry a part's table can
// hold: a little under 300 bytes.
func spreadVolumeName(number int) string {
	name := fmt.Sprintf("volume-%04d-", number)
	return name + strings.Repeat("x", maximumName-len(name))
}

const (
	// spreadMembers is more members of that width than maximumTableSize admits,
	// so it is the table and not the body that seals their parts: their pages
	// are 4 KiB, so the whole checkpoint is megabytes against the 64 MiB a part
	// fills to.
	spreadMembers = 4000
	// packedMembers is fewer than the table admits and far fewer bytes than a
	// part fills to, so a checkpoint of them is one part — and it is more than
	// the 256 KiB bound of the build before this one admitted, which wrote four.
	packedMembers = 3000
	// supersededTableSize is that earlier bound, which a part of 4 KiB pages
	// reached at about 8,700 members, a third of the 64 MiB it fills to.
	supersededTableSize = 256 << 10
)

// spreadStore publishes one checkpoint of count consecutive 4 KiB pages of a
// volume whose members are the widest a table holds, and reports the reference
// and the index it published.
func spreadStore(t *testing.T, store *Store, count uint64) (control.Ref, *Index) {
	t.Helper()
	name := spreadVolumeName(0)
	root, err := store.Root(t.Context(), control.Ref{VM: "spread", Sequence: 1},
		volumesAt(at4KiB, map[string]uint64{name: count * PageSize4KiB}))
	if err != nil {
		t.Fatal(err)
	}
	ref := control.Ref{VM: "spread", Sequence: 2}
	filling := store.Begin(root, ref)
	for page := range count {
		filling.Dirty(name, page)
	}
	filled, err := filling.Commit(t.Context(), randomPages{tag: 0xc0})
	if err != nil {
		t.Fatal(err)
	}
	return ref, filled
}

// A checkpoint whose members' entries alone exceed the table bound spreads them
// over several parts rather than writing one unbounded table. Their bodies are
// far short of what a part fills to, so nothing but the table's own bound seals
// those parts — and that bound is what lets every part's table be read as one
// suffix of maximumTableSize + TrailerSize bytes.
func TestMembersPastTheTableBoundSealTheirPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _ := tailStore(t)
		ref, filled := spreadStore(t, store, spreadMembers)
		entry := filled.checkpoints[ref]
		if entry.parts < 2 {
			t.Fatalf("a checkpoint of %d wide members wrote %d part(s), want them spread over several",
				spreadMembers, entry.parts)
		}
		if entry.bytes >= partTargetBytes {
			t.Fatalf("the checkpoint holds %d bytes, want less than the %d a part fills to",
				entry.bytes, partTargetBytes)
		}
		members := 0
		for number := range entry.parts {
			table, err := store.readPartTable(t.Context(), ref, number)
			if err != nil {
				t.Fatal(err)
			}
			members += len(table.members)
			if bytes := partTableBytes(t, store, ref, number); bytes > maximumTableSize {
				t.Fatalf("part %d holds a table of %d bytes, want no more than %d",
					number, bytes, maximumTableSize)
			}
		}
		if members != spreadMembers {
			t.Fatalf("the checkpoint's parts hold %d members, want %d", members, spreadMembers)
		}
	})
}

// A checkpoint of many small pages writes the parts its bytes need. The table
// bound is what used to stop a part short of what it fills to, and a checkpoint
// of 4 KiB pages then cost about twice the PUTs its bytes needed.
func TestACheckpointOfSmallPagesWritesThePartsItsBytesNeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _ := tailStore(t)
		ref, filled := spreadStore(t, store, packedMembers)
		entry := filled.checkpoints[ref]
		if entry.parts != 1 {
			t.Fatalf("a checkpoint of %d members holding %d bytes wrote %d parts, want the one its bytes need",
				packedMembers, entry.bytes, entry.parts)
		}
		// One suffix read still holds the whole table, and that table is past
		// what the build before this one would have admitted into one part.
		table := partTableBytes(t, store, ref, 0)
		if table > maximumTableSize || table <= supersededTableSize {
			t.Fatalf("the part's table is %d bytes, want more than %d and no more than %d",
				table, supersededTableSize, maximumTableSize)
		}
	})
}

// The table admits the members a part that filled to its target on 4 KiB pages
// holds. The entry cost is measured through the encoder rather than estimated,
// because it is what the bound was chosen from.
func TestTheTableAdmitsAFullPartOfSmallPages(t *testing.T) {
	const members = partTargetBytes / PageSize4KiB
	widest := part.Member{Volume: "ram0", Page: members - 1,
		Offset: partTargetBytes, Length: PageSize4KiB + blob.HeaderSize}
	entry := part.EntryBytes(widest)
	if entry > 32 {
		t.Fatalf("one entry of a 4 KiB member costs %d bytes, want the about 29 the bound was chosen from", entry)
	}
	if table := members * entry; table > maximumTableSize {
		t.Fatalf("the %d members of a full part of 4 KiB pages cost %d bytes of table, want no more than %d",
			members, table, maximumTableSize)
	}
	if table := members * entry; table <= supersededTableSize {
		t.Fatalf("the %d members cost %d bytes of table, which the %d-byte bound before this one already admitted",
			members, table, supersededTableSize)
	}
	// The envelope is what a member costs beyond the page it holds, and the
	// entry naming it is what that member costs in table: together about 1.9 %
	// of a checkpoint of whole 4 KiB pages.
	overhead := blob.HeaderSize + entry
	if overhead*100/PageSize4KiB > 2 {
		t.Fatalf("a 4 KiB member costs %d bytes beyond its page, want under two percent of it", overhead)
	}
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
