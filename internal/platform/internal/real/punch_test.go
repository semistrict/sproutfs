//go:build linux || darwin

package real_test

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/internal/real"
)

func TestPunchHoleReturnsPhysicalBlocksWithoutChangingOffsets(t *testing.T) {
	root := t.TempDir()
	disk, err := real.NewDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	file, err := disk.Open(t.Context(), "scratch", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data := bytes.Repeat([]byte{0x73}, 2<<20)
	if _, err := file.WriteAt(t.Context(), data, 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	blocks := func() int64 {
		stat, err := os.Stat(filepath.Join(root, "scratch"))
		if err != nil {
			t.Fatal(err)
		}
		return stat.Sys().(*syscall.Stat_t).Blocks
	}
	before := blocks()
	sparse, ok := file.(platform.SparseFile)
	if !ok {
		t.Fatal("real disk does not expose physical reclamation")
	}
	if err := sparse.PunchHole(t.Context(), 512<<10, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := blocks(); after >= before {
		t.Fatalf("hole retained physical blocks: before=%d after=%d", before, after)
	}
	clear(data[512<<10 : 1536<<10])
	got := make([]byte, len(data))
	if _, err := file.ReadAt(t.Context(), got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("punch changed bytes outside the discarded range")
	}
	if size, err := file.Size(t.Context()); err != nil || size != int64(len(data)) {
		t.Fatalf("punch changed length: %d %v", size, err)
	}
}
