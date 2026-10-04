//go:build !linux

package real

import (
	"errors"
	"fmt"
	"os"
)

// openExclusive opens nothing: only Linux claims a block device exclusively.
func openExclusive(path string) (*os.File, error) {
	return nil, fmt.Errorf("opening %s exclusively: %w", path, errors.ErrUnsupported)
}
