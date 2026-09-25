package real

import "syscall"

func filesystemBlockSize(stat syscall.Statfs_t) uint64 {
	// Linux reports block counts in fragment-size units when supplied.
	if stat.Frsize > 0 {
		return uint64(stat.Frsize)
	}
	return uint64(stat.Bsize)
}
