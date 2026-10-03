package peer_test

import (
	"context"
	"errors"
	"io"
)

// byteFile is a platform.File over bytes, which a cache's read hands out a
// range of.
type byteFile struct{ data []byte }

func (f *byteFile) ReadAt(_ context.Context, dst []byte, offset int64) (int, error) {
	if offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(dst, f.data[offset:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}

func (f *byteFile) WriteAt(context.Context, []byte, int64) (int, error) {
	return 0, errors.ErrUnsupported
}
func (f *byteFile) Truncate(context.Context, int64) error { return errors.ErrUnsupported }
func (f *byteFile) Sync(context.Context) error            { return nil }
func (f *byteFile) Size(context.Context) (int64, error)   { return int64(len(f.data)), nil }
func (f *byteFile) Close() error                          { return nil }
