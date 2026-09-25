package real

import "syscall"

func punchHole(fd uintptr, offset, length int64) error {
	return syscall.Fallocate(int(fd), 3, offset, length) // KEEP_SIZE | PUNCH_HOLE
}
