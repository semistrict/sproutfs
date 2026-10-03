//go:build darwin

package real_test

import (
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

// Darwin reserves no range ahead of a write, so a file there says it cannot,
// and a caller writes into the range as it is.
func TestAllocateIsUnsupportedOnDarwin(t *testing.T) {
	disk, err := real.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := disk.Open(t.Context(), "cache", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.(platform.AllocatingFile).Allocate(t.Context(), 0, 1<<20); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("allocating on darwin returned %v, want %v", err, errors.ErrUnsupported)
	}
}
