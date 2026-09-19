package vmmemory_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// A fork on the parent's own host inherits the pages the seal froze without
// publishing anything: the children read the parent's sealed pages by page
// identity, so the second maps the first's page without a load, and the whole
// fork writes one object per child — its control record.
func TestSameHostForkSharesSealedPagesAndPublishesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPagerCluster(t)
		source := c.create(t, "source", 4)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		r, m := f.attach(source.Volume("ram0"))
		// The guest stores into two pages and nothing publishes them, so they
		// exist only in this host's pages.
		for _, page := range []uint64{1, 2} {
			access(t, r, m, page, true)[0] = 44
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		before := c.objectKeys(t)
		point, err := source.ForkPoint(t.Context(),
			volume.Prepared([]byte("vmm"), map[string]volume.DirtySource{"ram0": r.Checkpoint()}))
		if err != nil {
			t.Fatal(err)
		}
		if pages := point.Pages("ram0"); len(pages) != 2 || pages[0] != 1 || pages[1] != 2 {
			t.Fatalf("the fork point holds %v unpublished, want pages 1 and 2", pages)
		}
		// Offering the pages is what the host taking these children in does
		// before their regions attach: it is the whole of the local backing's
		// attach, and it is what the first child below maps rather than reads.
		if err := point.Share(t.Context()); err != nil {
			t.Fatal(err)
		}
		fork := func(id string) (*volume.VM, *vmmemory.Region, *mapping) {
			vm, err := c.manager.Fork(t.Context(), id, point)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = vm.Close(context.Background()) })
			region, mp := f.attach(vm.Volume("ram0"))
			return vm, region, mp
		}
		atFirst, _ := f.h.Stats(t.Context())
		_, a, am := fork("a")
		for _, page := range []uint64{1, 2} {
			if access(t, a, am, page, false)[0] != 44 {
				t.Fatalf("the first fork lost the sealed bytes of page %d", page)
			}
		}
		// The first child maps the parent's own pages: no byte of the fork point
		// is read back out of the seal it was offered from.
		first, _ := f.h.Stats(t.Context())
		if first.Loads != atFirst.Loads || first.IdentityHits-atFirst.IdentityHits != 2 {
			t.Errorf("first fork: identity hits=%d loads=%d; want 2 hits and 0 loads",
				first.IdentityHits-atFirst.IdentityHits, first.Loads-atFirst.Loads)
		}
		// Attaching the sibling's region inherits the pages eagerly, by the
		// identity the fork point gives them: no byte is read for either page.
		atSibling, _ := f.h.Stats(t.Context())
		_, b, bm := fork("b")
		for _, page := range []uint64{1, 2} {
			if access(t, b, bm, page, false)[0] != 44 || am.pages[page].slot != bm.pages[page].slot {
				t.Errorf("sealed page %d did not share its resident page between siblings", page)
			}
		}
		after, _ := f.h.Stats(t.Context())
		if after.Loads != atSibling.Loads || after.IdentityHits-atSibling.IdentityHits != 2 {
			t.Errorf("sibling fork: identity hits=%d loads=%d; want 2 hits and 0 loads",
				after.IdentityHits-atSibling.IdentityHits, after.Loads-atSibling.Loads)
		}
		// Two forks of a running guest and not one checkpoint object: the
		// control record of each child is the whole of what reached the store.
		extra := addedKeys(before, c.objectKeys(t))
		if len(extra) != 2 || extra[0] != "control/a" || extra[1] != "control/b" {
			t.Fatalf("forking wrote %v, want only the children's control records", extra)
		}
		if status := source.Status(); !status.Sealed {
			t.Fatalf("the parent is not sealed while its children read the point: %+v", status)
		}
	})
}

// objectKeys lists every object in the cluster's store, by the key that follows
// the deployment prefix, in ascending order.
func (c *pagerCluster) objectKeys(t *testing.T) []string {
	t.Helper()
	prefix, err := platform.NewObjectPrefix("sproutfs/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	token := ""
	for {
		page, err := c.runtime.ObjectStore().List(t.Context(),
			platform.ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			names = append(names, strings.TrimPrefix(object.Key.String(), prefix.String()))
		}
		if page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	slices.Sort(names)
	return names
}

// addedKeys reports the keys in after that before did not have.
func addedKeys(before, after []string) []string {
	var extra []string
	for _, key := range after {
		if !slices.Contains(before, key) {
			extra = append(extra, key)
		}
	}
	return extra
}
