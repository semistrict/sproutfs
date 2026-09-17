package real

import (
	"syscall"
	"unsafe"
)

func punchHole(fd uintptr, offset, length int64) error {
	// fpunchhole_t and F_PUNCHHOLE from sys/fcntl.h.
	request := struct {
		flags, reserved uint32
		offset, length  int64
	}{offset: offset, length: length}
	_, _, err := syscall.Syscall(syscall.SYS_FCNTL, fd, 99, uintptr(unsafe.Pointer(&request)))
	if err != 0 {
		return err
	}
	return nil
}
