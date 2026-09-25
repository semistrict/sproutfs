package checkpoint_test

import (
	"bytes"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint/internal/part"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A publication uploads its parts straight to the object store: there is
// no local staging in this package today, and the part bytes never touch a
// disk. This test stages them itself, because the question it answers is about
// the format and not about where the bytes waited. A part that comes back from
// a power loss holding a torn tail, a lost sector or bytes nobody wrote must be
// refused by its own trailer, table and member envelopes; it must never read
// back as a member whose contents are not the ones that were written.
//
// If this package ever does stage parts locally, this is the test that says
// what such staging is allowed to hand back.
func TestATornPartIsRefusedRatherThanReadAsMembers(t *testing.T) {
	const members = 6
	const memberBytes = 64 << 10
	damaged, refusedTrailer, refusedTable, refusedMember, intact := 0, 0, 0, 0, 0
	synctest.Test(t, func(t *testing.T) {
		for seed := uint64(1); seed <= 48; seed++ {
			want := make([][]byte, members)
			builder := part.NewBuilder(nil)
			for i := range want {
				page := make([]byte, memberBytes)
				for j := range page {
					// Incompressible enough that the envelope stores it raw,
					// so a lost sector is a lost sector and not a codec artefact.
					page[j] = byte((i*7 + j*31 + j/97) % 251)
				}
				want[i] = page
				if _, _, err := builder.Add(t.Context(), part.Member{Volume: "root", Page: uint64(i)}, page); err != nil {
					t.Fatal(err)
				}
			}
			sealed, err := builder.Seal(1)
			if err != nil {
				t.Fatal(err)
			}

			runtime := sim.New(sim.Config{Seed: seed})
			disk := runtime.NewDisk("staging", sim.DiskConfig{PowerLossFaults: true})
			file, err := disk.Open(t.Context(), "part", platform.OpenOptions{Create: true})
			if err != nil {
				t.Fatal(err)
			}
			// The part's extent exists before the power loss; only its
			// contents are still on their way to the device.
			if err := file.Truncate(t.Context(), int64(len(sealed))); err != nil {
				t.Fatal(err)
			}
			if err := file.Sync(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt(t.Context(), sealed, 0); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := disk.PowerLoss(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := readPart(t, disk, "part", len(sealed))
			if bytes.Equal(got, sealed) {
				intact++
				continue
			}
			damaged++

			size := uint64(len(got))
			trailer, err := part.DecodeTrailer(got[size-part.TrailerSize:], size)
			if err != nil {
				refusedTrailer++
				continue
			}
			if trailer.Parts != 1 {
				t.Fatalf("seed %d: a trailer the reader accepted names %d parts", seed, trailer.Parts)
			}
			offset := trailer.TableOffset
			table, err := part.DecodeTable(got[offset:offset+trailer.TableLength], offset)
			if err != nil {
				refusedTable++
				continue
			}
			refused := false
			for _, member := range table {
				decoded, err := blob.Decode(t.Context(), got[member.Offset:member.Offset+member.Length], memberBytes)
				if err != nil {
					refused = true
					continue
				}
				if int(member.Page) >= len(want) || !bytes.Equal(decoded, want[member.Page]) {
					t.Fatalf("seed %d: page %d decoded to bytes that were never written",
						seed, member.Page)
				}
			}
			if refused {
				refusedMember++
			}
		}
	})
	if damaged == 0 {
		t.Fatal("no seed damaged the part; the power loss resolved nothing")
	}
	if refusedTrailer+refusedTable+refusedMember == 0 {
		t.Fatal("every damaged part parsed cleanly; nothing in the format noticed")
	}
	t.Logf("%d damaged of 48 (%d intact): trailer refused %d, table refused %d, member refused %d",
		damaged, intact, refusedTrailer, refusedTable, refusedMember)
}

func readPart(t *testing.T, disk *sim.Disk, name string, size int) []byte {
	t.Helper()
	file, err := disk.Open(t.Context(), name, platform.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	actual, err := file.Size(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if actual != int64(size) {
		// The extent was synced before the power loss, so a resolution that
		// moved it would be resolving something that was already durable.
		t.Fatalf("the staged part came back %d bytes, want %d", actual, size)
	}
	got := make([]byte, size)
	if _, err := file.ReadAt(t.Context(), got, 0); err != nil {
		t.Fatalf("reading the staged part: %v", err)
	}
	return got
}
