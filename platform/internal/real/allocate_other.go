//go:build !linux

package real

import "errors"

// allocate is Linux's fallocate alone. Darwin's F_PREALLOCATE reserves space
// only past the end of a file, not for a range, so a file there reserves
// nothing ahead and its writes find space as they land.
func allocate(uintptr, int64, int64) error { return errors.ErrUnsupported }
