//go:build !linux && !darwin

package real

import (
	"errors"
	"os"
)

func lock(*os.File) error { return errors.ErrUnsupported }
