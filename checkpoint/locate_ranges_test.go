package checkpoint_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
)

func TestLocateCrossPageRangesAgainstPageIdentities(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const size = 3*checkpoint.PageSize2MiB + 2*checkpoint.SectorSize
		store := mustStore(t, checkpoint.Config{ObjectStore: sim.New(sim.Config{}).ObjectStore()})
		sizes := map[string]uint64{"root": size}
		root, err := store.Root(t.Context(), control.Ref{VM: "located", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		model := newModel(volumes2MiB(sizes))
		baseRef := control.Ref{VM: "located", Sequence: 2}
		base := store.Begin(root, baseRef)
		// One identity per 2 MiB page, including the volume's partial last one.
		identities := make([]control.Identity, (size+checkpoint.PageSize2MiB-1)/checkpoint.PageSize2MiB)
		for index := range identities {
			identities[index] = control.Identity{Zero: true}
		}
		for sector := range uint32(sectorsPerPage) {
			model.dirty(base, "root", 1, sector, sectorData("base", 1, sector))
		}
		identities[1] = control.Identity{Ref: baseRef, Volume: "root", Page: 1}
		parent, err := base.Commit(t.Context(), model)
		if err != nil {
			t.Fatal(err)
		}
		forkModel := model.clone()
		forkRef := control.Ref{VM: "located-fork", Sequence: 1}
		child := store.Begin(parent, forkRef)
		forkModel.dirty(child, "root", 1, 5, sectorData("fork", 1, 5))
		fork, err := child.Commit(t.Context(), forkModel)
		if err != nil {
			t.Fatal(err)
		}
		// One storage page written republishes the page whole, so the fork owns
		// all of it rather than a run inside it.
		forkIdentities := slices.Clone(identities)
		forkIdentities[1] = control.Identity{Ref: forkRef, Volume: "root", Page: 1}
		checkRead(t, store, parent, model)
		checkRead(t, store, fork, forkModel)

		// Exercise byte offsets on either side of page boundaries, including
		// zero-length queries at EOF and the partial final page.
		points := []uint64{0, checkpoint.PageSize2MiB - 1, checkpoint.PageSize2MiB,
			checkpoint.PageSize2MiB + 5*checkpoint.SectorSize - 1, checkpoint.PageSize2MiB + 5*checkpoint.SectorSize,
			checkpoint.PageSize2MiB + 6*checkpoint.SectorSize, 2*checkpoint.PageSize2MiB - 1, 2 * checkpoint.PageSize2MiB,
			3 * checkpoint.PageSize2MiB, size - 1, size}
		for _, fixture := range []struct {
			index      *checkpoint.Index
			identities []control.Identity
		}{{parent, identities}, {fork, forkIdentities}} {
			for left, offset := range points {
				for _, end := range points[left:] {
					var want []control.Extent
					// The oracle walks the flat page model, without the index's
					// table or page-relative arithmetic. Only holes merge: two
					// published pages never share an identity.
					for page, identity := range fixture.identities {
						start := max(offset, uint64(page)*checkpoint.PageSize2MiB)
						stop := min(end, min(size, uint64(page+1)*checkpoint.PageSize2MiB))
						if start >= stop {
							continue
						}
						if n := len(want); n > 0 && identity.Zero && want[n-1].Identity == identity {
							want[n-1].Length += stop - start
						} else {
							want = append(want, control.Extent{Offset: start, Length: stop - start, Identity: identity})
						}
					}
					got, err := fixture.index.Locate(t.Context(), "root", offset, end-offset)
					if err != nil || !slices.Equal(got, want) {
						t.Fatalf("%s locate [%d,%d): got %+v, %v; want %+v", fixture.index.Ref(), offset, end, got, err, want)
					}
				}
			}
		}
	})
}

// A 4 KiB-page volume of three segments and a short page: the first segment's
// table ends in a hole and the pages past its last entry are holes, the
// second's pages are published in runs between holes, and the third is
// addressed by no table at all. Locating any range of it, one segment's table
// lookup and a scan of its entries at a time, reports what the flat model of
// pages says, wherever the range begins and ends.
func TestLocateRangesAcrossSegmentsOf4KiBPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			page     = checkpoint.PageSize4KiB
			segment  = 16 << 10
			pages    = 3*segment + 1
			size     = pages*page - checkpoint.SectorSize
			sequence = 2
		)
		store := mustStore(t, checkpoint.Config{ObjectStore: sim.New(sim.Config{}).ObjectStore()})
		ref := control.Ref{VM: "located-4k", Sequence: 1}
		root, err := store.Root(t.Context(), ref, map[string]checkpoint.VolumeSpec{
			"ram": {Size: size, PageSize: page}})
		if err != nil {
			t.Fatal(err)
		}
		ref.Sequence = sequence
		publication := store.Begin(root, ref)
		published := map[uint64]bool{}
		for _, run := range [][2]uint64{{0, 1}, {5, 10}, {segment - 40, segment - 3},
			{segment + 100, segment + 200}, {segment + 201, segment + 202}, {2*segment - 1, 2 * segment}} {
			for number := run[0]; number < run[1]; number++ {
				publication.Dirty("ram", number)
				published[number] = true
			}
		}
		index, err := publication.Commit(t.Context(), offsetSource{})
		if err != nil {
			t.Fatal(err)
		}
		points := []uint64{0, page - 1, page, 5*page + 7, 9 * page, segment*page - 4*page - 1, segment * page,
			(segment + 150) * page, (segment+201)*page + 3, 2*segment*page - page, 2 * segment * page,
			(2*segment + 77) * page, size - 1, size}
		for left, offset := range points {
			for _, end := range points[left:] {
				var want []control.Extent
				for number := uint64(0); number < pages; number++ {
					start, stop := max(offset, number*page), min(end, (number+1)*page)
					if start >= stop {
						continue
					}
					identity := control.ZeroIdentity
					if published[number] {
						identity = control.Identity{Ref: ref, Volume: "ram", Page: number}
					}
					if n := len(want); n > 0 && identity.Zero && want[n-1].Identity.Zero {
						want[n-1].Length += stop - start
						continue
					}
					want = append(want, control.Extent{Offset: start, Length: stop - start, Identity: identity})
				}
				got, err := index.Locate(t.Context(), "ram", offset, end-offset)
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("locate [%d,%d): got %+v, %v; want %+v", offset, end, got, err, want)
				}
			}
		}
	})
}
