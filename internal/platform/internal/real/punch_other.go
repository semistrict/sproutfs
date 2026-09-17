//go:build !linux && !darwin

package real

import "errors"

func punchHole(uintptr, int64, int64) error { return errors.ErrUnsupported }
