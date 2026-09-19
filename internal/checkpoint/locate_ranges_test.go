package checkpoint_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform/sim"
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
