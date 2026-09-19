package vmmemory_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// publishedIn is a backing that states the page size its volume is published
// in, which *volume.Volume does. The pager has one page and every number it
// faults, keys and maps is in it.
type publishedIn struct {
	vmmemory.Backing
	pageSize uint64
}

func (b publishedIn) PageSize() uint64 { return b.pageSize }

// A volume published in a page size other than this pager's is refused when it
// is attached: a page number of it means something else, so serving it would
// fault, key and map the wrong bytes. The refusal names both sizes, and it
// happens before any mapping is armed.
func TestAttachRefusesAVolumeOfAnotherPageSize(t *testing.T) {
	f := newFixture(t, 4, 8, 4)
	for _, stated := range []uint64{4 << 10, 64 << 10, 4 << 20} {
		m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
		_, err := f.h.Attach(t.Context(), ram(publishedIn{Backing: f.newBacking(2), pageSize: stated}), m)
		if !errors.Is(err, vmmemory.ErrConfig) {
			t.Fatalf("attaching a volume of %d-byte pages: %v", stated, err)
		}
		if got := err.Error(); !strings.Contains(got, "this pager's page is 2097152") {
			t.Fatalf("the refusal reads %q, want it to name the pager's own page", got)
		}
		if len(m.pages) != 0 {
			t.Fatalf("a refused attach armed %d mappings", len(m.pages))
		}
	}
	// The pager's own page attaches, so what was refused is the geometry and
	// not the backing.
	f.attach(publishedIn{Backing: f.newBacking(2), pageSize: vmmemory.PageSize})
}
