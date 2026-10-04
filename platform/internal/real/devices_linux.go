package real

import (
	"os"
	"syscall"
)

// openExclusive opens a block device read and write with O_EXCL, which Linux
// takes as an exclusive claim of the device across every process.
func openExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_EXCL, 0)
}
