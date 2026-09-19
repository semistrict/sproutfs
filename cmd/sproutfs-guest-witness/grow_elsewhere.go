//go:build !linux

package main

import (
	"fmt"
	"os"
	"runtime"
)

// The witness is a guest binary, and a guest is Linux. These stand in where it
// is built for a developer's own machine, so that the arithmetic, the mount
// table reading and the superblock reading are compiled and tested there while
// the two calls that are Linux and nothing else say so rather than not
// existing.

func deviceSize(path string) (uint64, error) {
	return 0, fmt.Errorf("the size of %s is a Linux ioctl, and this is %s", path, runtime.GOOS)
}

func resizeFilesystem(point string, blocks uint64) error {
	return fmt.Errorf("growing %s to %d blocks is a Linux ioctl, and this is %s",
		point, blocks, runtime.GOOS)
}

// deviceAndAttributes has nothing to report off Linux: there is no PMEM under a
// developer's machine, and so nothing for a witness there to require.
func deviceAndAttributes(*os.File) (string, uint64, error) { return "", 0, nil }
