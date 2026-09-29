package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/semistrict/sproutfs/volume"
)

// Extent is one range of a guest image that holds data, in bytes.
type Extent struct {
	Offset, Length int64
}

// SparseSource is a guest image that says where its data is. An import reads
// only its data extents; everything between them is zeroes, which the digest
// hashes without reading and the template never writes. An *os.File is one
// without implementing this, through SEEK_DATA and SEEK_HOLE, on a filesystem
// that reports its holes.
type SparseSource interface {
	io.ReadSeeker
	// DataExtents is where the image's data is, ascending and apart, within
	// its size. A byte outside every extent must read as zero.
	DataExtents() ([]Extent, error)
}

// guestImage is a guest image and where its data is, found once so that the
// digest that names a template and the bytes imported under that name read
// the same ranges.
type guestImage struct {
	source  io.ReadSeeker
	size    int64
	extents []Extent
}

// scanImage finds where a guest image's data is. A source that cannot say is
// one extent of its whole size.
func scanImage(source io.ReadSeeker) (guestImage, error) {
	size, err := source.Seek(0, io.SeekEnd)
	if err != nil {
		return guestImage{}, err
	}
	var extents []Extent
	switch source := source.(type) {
	case SparseSource:
		extents, err = source.DataExtents()
	case *os.File:
		extents, err = fileExtents(source, size)
	default:
		if size > 0 {
			extents = []Extent{{Length: size}}
		}
	}
	if err != nil {
		return guestImage{}, err
	}
	var end int64
	for _, extent := range extents {
		if extent.Offset < end || extent.Length <= 0 || extent.Length > size-extent.Offset {
			return guestImage{}, fmt.Errorf("%w: data extent %+v of a %d-byte image is not ascending, apart and within it",
				ErrRequest, extent, size)
		}
		end = extent.Offset + extent.Length
	}
	return guestImage{source: source, size: size, extents: extents}, nil
}

// digest is the sha256 of the image, which is the whole of what names its
// template. A hole is hashed as the zeroes it reads as, so a sparse image and
// the same image written out in full name one template.
func (g guestImage) digest() ([sha256.Size]byte, error) {
	sum := sha256.New()
	buffer := make([]byte, importBatchBytes)
	zeroes := make([]byte, importBatchBytes)
	hashZeroes := func(count int64) {
		for count > 0 {
			n := min(count, int64(len(zeroes)))
			sum.Write(zeroes[:n])
			count -= n
		}
	}
	var at int64
	for _, extent := range g.extents {
		hashZeroes(extent.Offset - at)
		if err := g.read(extent, buffer, func(_ int64, data []byte) error {
			sum.Write(data)
			return nil
		}); err != nil {
			return [sha256.Size]byte{}, err
		}
		at = extent.Offset + extent.Length
	}
	hashZeroes(g.size - at)
	return [sha256.Size]byte(sum.Sum(nil)), nil
}

// importInto writes the image into a volume, checkpointing as it goes so that
// the import's cost is bounded by importCheckpointBytes rather than by the
// image. Only the data extents are read, and a batch of them that is all
// zeroes is not written either: the volume already reads as zeroes, and a
// page that is never written is a page no object is ever published for.
func (g guestImage) importInto(ctx context.Context, vm *volume.VM, name string) error {
	target := vm.Volume(name)
	if target == nil {
		return fmt.Errorf("%w: the template has no volume named %s", ErrRequest, name)
	}
	if uint64(g.size) > target.Size() {
		return fmt.Errorf("the %d-byte image is larger than the %d-byte volume", g.size, target.Size())
	}
	buffer := make([]byte, importBatchBytes)
	zeroes := make([]byte, importBatchBytes)
	var pending uint64
	for _, extent := range g.extents {
		if err := g.read(extent, buffer, func(offset int64, data []byte) error {
			if bytes.Equal(data, zeroes[:len(data)]) {
				return nil
			}
			if err := target.Write(ctx, uint64(offset), data); err != nil {
				return err
			}
			pending += uint64(len(data))
			if pending < importCheckpointBytes {
				return nil
			}
			pending = 0
			return vm.Checkpoint(ctx)
		}); err != nil {
			return err
		}
	}
	return nil
}

// read passes one extent to each in batches of at most the buffer, with the
// offset in the image each batch starts at.
func (g guestImage) read(extent Extent, buffer []byte, each func(offset int64, data []byte) error) error {
	if _, err := g.source.Seek(extent.Offset, io.SeekStart); err != nil {
		return err
	}
	for offset, end := extent.Offset, extent.Offset+extent.Length; offset < end; {
		batch := buffer[:min(int64(len(buffer)), end-offset)]
		if _, err := io.ReadFull(g.source, batch); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("reading the image at %d: %w", offset, err)
		}
		if err := each(offset, batch); err != nil {
			return err
		}
		offset += int64(len(batch))
	}
	return nil
}
