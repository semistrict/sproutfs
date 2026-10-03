package real

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/semistrict/sproutfs/platform"
)

// NewDeviceWrites reads the bytes written by the block device that holds dir,
// from /sys/dev/block/<major>:<minor>/stat. A partition has a stat file of its
// own. A directory on no block device, such as one on tmpfs or overlayfs, has
// none, and is refused.
func NewDeviceWrites(ctx context.Context, dir string) (platform.DeviceWrites, error) {
	var stat unix.Stat_t
	if err := unix.Stat(dir, &stat); err != nil {
		return nil, err
	}
	device := uint64(stat.Dev)
	major, minor := unix.Major(device), unix.Minor(device)
	if major == 0 {
		return nil, fmt.Errorf("%s is on device %d:%d, which is no block device: %w",
			dir, major, minor, platform.ErrUnavailable)
	}
	writes := deviceWrites{path: fmt.Sprintf("/sys/dev/block/%d:%d/stat", major, minor)}
	if _, err := writes.BytesWritten(ctx); err != nil {
		return nil, fmt.Errorf("the device under %s: %w", dir, err)
	}
	return writes, nil
}
