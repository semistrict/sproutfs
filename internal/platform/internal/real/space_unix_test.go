//go:build linux || darwin

package real

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform"
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

func TestDiskSpaceErrorsPreserveSystemCause(t *testing.T) {
	for _, cause := range []error{syscall.ENOSPC, syscall.EDQUOT} {
		err := normalizeFileError(cause)
		if !errors.Is(err, platform.ErrNoSpace) || !errors.Is(err, cause) {
			t.Fatalf("lost space classification or cause: %v", err)
		}
	}
}
