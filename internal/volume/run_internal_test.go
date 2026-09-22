package volume

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
)

// sealedPage is the page size these use, small enough to write the expectation
// out page by page.
const sealedPage = checkpoint.PageSize4KiB

// recordingSource is the inherited checkpoint under a seal: it fills the pages
// a mask marks with a byte of their own and records every mask it was asked
// for, so a test says how many reads the seal cost and which pages each asked
// about.
type recordingSource struct{ asked [][]bool }

func (s *recordingSource) read(ctx context.Context, volume string, offset uint64, dst []byte) error {
	return s.readPages(ctx, volume, offset, dst, nil)
}

func (s *recordingSource) readPages(_ context.Context, _ string, offset uint64, dst []byte, wanted []bool) error {
	mask := wanted
	if mask == nil {
		mask = make([]bool, (offset+uint64(len(dst))-1)/sealedPage-offset/sealedPage+1)
		for at := range mask {
			mask[at] = true
		}
	}
	s.asked = append(s.asked, mask)
	first := offset / sealedPage
	for at, want := range mask {
		if !want {
			continue
		}
		page := first + uint64(at)
		start := max(offset, page*sealedPage)
		stop := min(offset+uint64(len(dst)), (page+1)*sealedPage)
		for cursor := start; cursor < stop; cursor++ {
			dst[cursor-offset] = byte(page + 1)
		}
	}
	return nil
}

func (s *recordingSource) locate(context.Context, string, uint64, uint64) ([]control.Extent, error) {
	return nil, nil
}

// heldPages is the pager state a seal holds: the pages it took, filled with one
// byte of their own.
type heldPages struct{ pages []uint64 }

func (h heldPages) PageSize() int        { return sealedPage }
func (h heldPages) DirtyPages() []uint64 { return h.pages }
func (h heldPages) ReadDirty(_ context.Context, _ uint64, dst []byte) error {
	for at := range dst {
		dst[at] = 0xaa
	}
	return nil
}
func (heldPages) UnpublishedAge() time.Duration                    { return 0 }
func (heldPages) Settle(context.Context) (int, error)              { return 0, nil }
func (heldPages) Hold()                                            {}
func (heldPages) Share(context.Context, control.Ref, string) error { return nil }
func (heldPages) Retire(context.Context, bool) error               { return nil }

// A pager's window over a volume whose seal holds some of its pages is one read
// of the inherited checkpoint, not one per stretch the seal left: the pages the
// seal holds come out of this host's own memory and every other wanted page is
// asked for together.
func TestASealedWindowAsksTheCheckpointOnce(t *testing.T) {
	const pages = 8
	parent := &recordingSource{}
	sealed := newSealedSource(parent, control.Ref{VM: "vm", Sequence: 2},
		map[string]DirtySource{"ram": heldPages{pages: []uint64{2, 5}}},
		map[string]checkpoint.Geometry{"ram": {PageSize: sealedPage, SegmentPages: 16 << 10}})
	wanted := []bool{false, true, true, true, false, true, true, false}
	got := bytes.Repeat([]byte{0xfe}, pages*sealedPage)
	if err := sealed.readPages(t.Context(), "ram", 0, got, wanted); err != nil {
		t.Fatal(err)
	}
	if len(parent.asked) != 1 {
		t.Fatalf("the window asked the checkpoint %d times, want once", len(parent.asked))
	}
	if want := []bool{false, true, false, true, false, false, true, false}; !slices.Equal(parent.asked[0], want) {
		t.Fatalf("the checkpoint was asked for %v, want %v: the wanted pages the seal does not hold", parent.asked[0], want)
	}
	for page := range uint64(pages) {
		want := byte(0xfe)
		switch {
		case !wanted[page]:
		case page == 2 || page == 5:
			want = 0xaa
		default:
			want = byte(page + 1)
		}
		at := page * sealedPage
		if expected := bytes.Repeat([]byte{want}, sealedPage); !bytes.Equal(got[at:at+sealedPage], expected) {
			t.Fatalf("page %d reads back as %#x..., want %#x", page, got[at:at+8], want)
		}
	}
}
