//go:build unix

package host

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// fileExtents asks the kernel where a file's data is. A filesystem that cannot
// say — one that refuses SEEK_DATA, or a FUSE filesystem without lseek, which
// reports the whole file as data — costs the reads it always did, from the
// first offset it could not answer for.
func fileExtents(file *os.File, size int64) ([]Extent, error) {
	fd := int(file.Fd())
	var extents []Extent
	for offset := int64(0); offset < size; {
		data, err := unix.Seek(fd, offset, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			// Nothing but a hole from offset to the end.
			return extents, nil
		}
		if err != nil {
			return append(extents, Extent{Offset: offset, Length: size - offset}), nil
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return append(extents, Extent{Offset: data, Length: size - data}), nil
		}
		hole = min(hole, size)
		if hole <= data {
			return append(extents, Extent{Offset: data, Length: size - data}), nil
		}
		extents = append(extents, Extent{Offset: data, Length: hole - data})
		offset = hole
	}
	return extents, nil
}
