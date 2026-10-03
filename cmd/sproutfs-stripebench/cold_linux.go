package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropCacheFunc drops a range of f from the page cache, so the next read of
// it reads the disk.
func dropCacheFunc(f *os.File) func(off, n int64) error {
	return func(off, n int64) error {
		return unix.Fadvise(int(f.Fd()), off, n, unix.FADV_DONTNEED)
	}
}
