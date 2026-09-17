package real

import "syscall"

func filesystemBlockSize(stat syscall.Statfs_t) uint64 { return uint64(stat.Bsize) }
