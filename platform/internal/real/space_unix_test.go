//go:build linux || darwin

package real

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/semistrict/sproutfs/platform"
)

func TestDiskSpace(t *testing.T) {
	disk, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	space, err := disk.Space(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if space.ID == "" || space.Total == 0 || space.Available > space.Total || space.AllocationUnit <= 0 {
		t.Fatalf("invalid filesystem space: %+v", space)
	}
	file, err := disk.Open(t.Context(), "file", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	backing, err := file.(platform.DiskSpace).Space(t.Context())
	if err != nil || backing.ID != space.ID || backing.Total != space.Total || backing.AllocationUnit != space.AllocationUnit {
		t.Fatalf("file and directory disagree: %+v %+v %v", space, backing, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := disk.Space(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// A file's allocation is what it holds, not its size: a sparse file holds
// nothing until it is written.
func TestAFileIsAllocatedOnlyWhereItWasWritten(t *testing.T) {
	disk, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opened, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := opened.Truncate(t.Context(), 1<<30); err != nil {
		t.Fatal(err)
	}
	sparse, err := opened.(platform.FileAllocation).Allocated(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if sparse != 0 {
		t.Fatalf("a 1 GiB file nothing was written to holds %d bytes, want 0", sparse)
	}
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i%251 + 1)
	}
	if _, err := opened.WriteAt(t.Context(), data, 0); err != nil {
		t.Fatal(err)
	}
	if err := opened.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	written, err := opened.(platform.FileAllocation).Allocated(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if written != 1<<20 {
		t.Fatalf("a 1 GiB file with 1 MiB written holds %d bytes, want 1 MiB", written)
	}
}

func TestDiskSpaceErrorsPreserveSystemCause(t *testing.T) {
	for _, cause := range []error{syscall.ENOSPC, syscall.EDQUOT} {
		err := normalizeFileError(cause)
		if !errors.Is(err, platform.ErrNoSpace) || !errors.Is(err, cause) {
			t.Fatalf("lost space classification or cause: %v", err)
		}
	}
}
