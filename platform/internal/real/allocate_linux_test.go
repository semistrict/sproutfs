//go:build linux

package real_test

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

// Allocating a range reserves its blocks before anything is written, grows the
// file to cover it, and reads as zeroes.
func TestAllocateReservesARangeThatReadsZeroes(t *testing.T) {
	root := t.TempDir()
	disk, err := real.NewDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	file, err := disk.Open(t.Context(), "cache", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.(platform.AllocatingFile).Allocate(t.Context(), 1<<20, 1<<20); err != nil {
		t.Fatal(err)
	}
	if size, err := file.Size(t.Context()); err != nil || size != 2<<20 {
		t.Fatalf("the allocated file is %d bytes, %v; want 2 MiB", size, err)
	}
	stat, err := os.Stat(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if blocks := stat.Sys().(*syscall.Stat_t).Blocks * 512; blocks < 1<<20 {
		t.Fatalf("the allocated file holds %d bytes of blocks, want the 1 MiB allocated", blocks)
	}
	got := make([]byte, 1<<20)
	if _, err := file.ReadAt(t.Context(), got, 1<<20); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, 1<<20)) {
		t.Fatal("the allocated range does not read as zeroes")
	}
}
