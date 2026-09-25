//go:build linux && (amd64 || arm64)

package vmmemory

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"
)

// HugePageBytes is how much of this file's huge mapping the kernel has
// installed as 2 MiB pages, from this process's own smaps: zero for a file
// with no huge mapping. A huge page Zero allocates is installed there by the
// fault that allocated it, and split out of it again by a release inside it.
func (a *LinuxFile) HugePageBytes() (uint64, error) {
	if a.huge == nil {
		return 0, nil
	}
	f, err := os.Open("/proc/self/smaps")
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	want := strconv.FormatUint(uint64(uintptr(unsafe.Pointer(&a.huge[0]))), 16)
	found := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if base, _, header := strings.Cut(line, "-"); header && !strings.ContainsAny(base, ": ") {
			if found {
				break
			}
			found = base == want
			continue
		}
		if value, ok := strings.CutPrefix(line, "ShmemPmdMapped:"); ok && found {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%q: %w", line, err)
			}
			return kb << 10, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no ShmemPmdMapped for the mapping at %s", want)
}
