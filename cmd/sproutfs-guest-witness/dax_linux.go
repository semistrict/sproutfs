package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// deviceAndAttributes reports the block device under an open file, by the name
// the kernel gives it, and the file's statx attributes. The device comes from
// /sys/dev/block/<major>:<minor>, a symlink whose last element is that name; a
// file on something with no block device under it — a tmpfs — has none.
func deviceAndAttributes(file *os.File) (string, uint64, error) {
	var st unix.Statx_t
	if err := unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BASIC_STATS, &st); err != nil {
		return "", 0, fmt.Errorf("statx: %w", err)
	}
	link, err := os.Readlink(fmt.Sprintf("/sys/dev/block/%d:%d", st.Dev_major, st.Dev_minor))
	if os.IsNotExist(err) {
		return "", st.Attributes, nil
	}
	if err != nil {
		return "", 0, err
	}
	return filepath.Base(link), st.Attributes, nil
}
