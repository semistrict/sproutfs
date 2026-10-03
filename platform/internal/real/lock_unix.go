//go:build linux || darwin

package real

import (
	"errors"
	"os"
	"syscall"

	"github.com/semistrict/sproutfs/platform"
)

// lock takes the file's exclusive lock without waiting. flock locks the open
// file description, so the lock lasts while the handle is open and ends with
// the process however it ends; Go opens every file close-on-exec, so no child
// process inherits it.
func lock(handle *os.File) error {
	for {
		err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errors.Join(platform.ErrLocked, err)
		default:
			return err
		}
	}
}
