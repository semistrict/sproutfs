//go:build !linux

package real

import (
	"context"
	"fmt"
	"runtime"

	"github.com/semistrict/sproutfs/platform"
)

// NewDeviceWrites refuses on every system but Linux, which is the only one
// whose device counters this host reads.
func NewDeviceWrites(_ context.Context, dir string) (platform.DeviceWrites, error) {
	return nil, fmt.Errorf("the device under %s: reading its bytes written needs Linux, not %s: %w",
		dir, runtime.GOOS, platform.ErrUnavailable)
}
