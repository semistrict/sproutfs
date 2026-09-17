package checkpoint_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

func TestLocateCrossPageRangesAgainstPageLineage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const size = 3*checkpoint.PageSize + 2*checkpoint.SectorSize
		store := mustStore(t, checkpoint.Config{ObjectStore: sim.New(sim.Config{}).ObjectStore()})
		sizes := map[string]uint64{"root": size}
		root, err := store.Root(t.Context(), control.Ref{VM: "lineage", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		model := newModel(sizes)
		baseRef := control.Ref{VM: "lineage", Sequence: 2}
		base := store.Begin(root, baseRef)
		// One identity per 2 MiB page, including the volume's partial last one.
		lineage := make([]control.Identity, (size+checkpoint.PageSize-1)/checkpoint.PageSize)
		for index := range lineage {
			lineage[index] = control.Identity{Zero: true}
		}
		for sector := range uint32(sectorsPerPage) {
			model.dirty(base, "root", 1, sector, sectorData("base", 1, sector))
		}
		lineage[1] = control.Identity{Ref: baseRef, Volume: "root", Page: 1}
		parent, err := base.Commit(t.Context(), model)
		if err != nil {
			t.Fatal(err)
		}
		forkModel := model.clone()
		forkRef := control.Ref{VM: "lineage-fork", Sequence: 1}
		child := store.Begin(parent, forkRef)
		forkModel.dirty(child, "root", 1, 5, sectorData("fork", 1, 5))
		fork, err := child.Commit(t.Context(), forkModel)
		if err != nil {
			t.Fatal(err)
		}
		// One storage page written republishes the page whole, so the fork owns
		// all of it rather than a run inside it.
		forkLineage := slices.Clone(lineage)
		forkLineage[1] = control.Identity{Ref: forkRef, Volume: "root", Page: 1}
		checkRead(t, store, parent, model)
		checkRead(t, store, fork, forkModel)

		// Exercise byte offsets on either side of page boundaries, including
		// zero-length queries at EOF and the partial final page.
		points := []uint64{0, checkpoint.PageSize - 1, checkpoint.PageSize,
			checkpoint.PageSize + 5*checkpoint.SectorSize - 1, checkpoint.PageSize + 5*checkpoint.SectorSize,
			checkpoint.PageSize + 6*checkpoint.SectorSize, 2*checkpoint.PageSize - 1, 2 * checkpoint.PageSize,
			3 * checkpoint.PageSize, size - 1, size}
		for _, fixture := range []struct {
			index   *checkpoint.Index
			lineage []control.Identity
		}{{parent, lineage}, {fork, forkLineage}} {
			for left, offset := range points {
				for _, end := range points[left:] {
					var want []control.Extent
					// The oracle walks the flat page model, without the index's
					// table or page-relative arithmetic. Only holes merge: two
					// published pages never share an identity.
					for page, identity := range fixture.lineage {
						start := max(offset, uint64(page)*checkpoint.PageSize)
						stop := min(end, min(size, uint64(page+1)*checkpoint.PageSize))
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
