//go:build !unix

package host

import "os"

// fileExtents is the whole file where the kernel cannot be asked.
func fileExtents(_ *os.File, size int64) ([]Extent, error) {
	return []Extent{{Length: size}}, nil
}
