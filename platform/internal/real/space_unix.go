//go:build linux || darwin

package real

import (
	"context"
	"fmt"
	"math"
	"syscall"

	"github.com/semistrict/sproutfs/platform"
)

func (d *Disk) Space(ctx context.Context) (platform.FilesystemSpace, error) {
	return SpaceAt(ctx, d.root)
}

// SpaceAt measures an existing path without creating or opening a disk owner.
// It also permits admission checks before a new owned directory is created.
func SpaceAt(ctx context.Context, path string) (platform.FilesystemSpace, error) {
	if err := context.Cause(ctx); err != nil {
		return platform.FilesystemSpace{}, err
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return platform.FilesystemSpace{}, err
	}
	return filesystemSpace(stat, context.Cause(ctx))
}

func (f *file) Space(ctx context.Context) (platform.FilesystemSpace, error) {
	if err := context.Cause(ctx); err != nil {
		return platform.FilesystemSpace{}, err
	}
	var stat syscall.Statfs_t
	if err := syscall.Fstatfs(int(f.handle.Fd()), &stat); err != nil {
		return platform.FilesystemSpace{}, err
	}
	return filesystemSpace(stat, context.Cause(ctx))
}

func filesystemSpace(stat syscall.Statfs_t, err error) (platform.FilesystemSpace, error) {
	if err != nil {
		return platform.FilesystemSpace{}, err
	}
	block := filesystemBlockSize(stat)
	if block == 0 || block > math.MaxInt64 || stat.Blocks > math.MaxUint64/block || stat.Bavail > stat.Blocks {
		return platform.FilesystemSpace{}, platform.ErrInvalidRange
	}
	return platform.FilesystemSpace{ID: fmt.Sprint(stat.Fsid), Total: stat.Blocks * block, Available: stat.Bavail * block, AllocationUnit: int64(block)}, nil
}

var _ platform.DiskSpace = (*Disk)(nil)
var _ platform.DiskSpace = (*file)(nil)
