package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ext4ResizeFS is EXT4_IOC_RESIZE_FS, which the kernel declares as
// _IOW('f', 16, __u64): the argument is a pointer to the block count the
// filesystem is to have, and the descriptor is any file on the filesystem —
// the mount point itself, here. It is the one way to grow a mounted ext4 that
// needs no write open of the device under it.
const ext4ResizeFS = 0x40086610

// deviceSize is how many bytes the block device at path holds.
//
// The open is read-only on purpose. A 6.18 kernel refuses a write open of a
// device something has mounted, and the device this asks about is the one the
// guest's own root filesystem is on; a read open of it is still allowed, and
// BLKGETSIZE64 needs nothing more.
func deviceSize(path string) (uint64, error) {
	device, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("the device %s: %w", path, err)
	}
	defer device.Close()
	var size uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(),
		uintptr(unix.BLKGETSIZE64), uintptr(unsafe.Pointer(&size))); errno != 0 {
		return 0, fmt.Errorf("asking %s for its size: %w", path, errno)
	}
	return size, nil
}

// resizeFilesystem gives the filesystem mounted at point the block count it is
// to have. It is the ioctl and nothing else: no device is opened, so a mounted
// root filesystem is grown exactly as any other is.
func resizeFilesystem(point string, blocks uint64) error {
	mounted, err := os.Open(point)
	if err != nil {
		return fmt.Errorf("the mount point %s: %w", point, err)
	}
	defer mounted.Close()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, mounted.Fd(),
		uintptr(ext4ResizeFS), uintptr(unsafe.Pointer(&blocks))); errno != 0 {
		return errno
	}
	return nil
}
