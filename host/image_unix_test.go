//go:build unix

package host

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The kernel says where a sparse file's data is, and a hole of megabytes is
// not in it: what an import reads of a 16 GiB root image is what it holds.
func TestFileExtentsLeaveOutTheHoles(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "image"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	page := make([]byte, 4096)
	for i := range page {
		page[i] = 0xa5
	}
	for _, at := range []int64{0, 3 << 20} {
		if _, err := file.WriteAt(page, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Truncate(4 << 20); err != nil {
		t.Fatal(err)
	}
	extents, err := fileExtents(file, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	var covered int64
	for _, extent := range extents {
		covered += extent.Length
	}
	// A filesystem allocates in blocks of its own, so an extent may be wider
	// than the page written; it is never the three MiB between them.
	if covered > 1<<20 || !slices.ContainsFunc(extents, func(e Extent) bool { return e.Offset <= 0 && e.Offset+e.Length >= 4096 }) ||
		!slices.ContainsFunc(extents, func(e Extent) bool { return e.Offset <= 3<<20 && e.Offset+e.Length >= 3<<20+4096 }) {
		t.Fatalf("extents %+v: want both written pages and not the hole between them", extents)
	}
}
