package real

import "syscall"

func allocate(fd uintptr, offset, length int64) error {
	return syscall.Fallocate(int(fd), 0, offset, length)
}
