package real

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/semistrict/sproutfs/platform"
)

// Devices opens the network disks attached to this machine as block devices,
// under one directory of the kernel's names for them: on Compute Engine,
// /dev/disk/by-id with the prefix google-, followed by the device name a disk
// was attached under (GCEDeviceName). A device is opened with O_EXCL, which
// on Linux is an exclusive claim of the block device: a second open, by any
// process of the machine, fails with EBUSY while the first is open.
type Devices struct {
	dir, prefix string
	// name is the device name a volume is attached under.
	name func(volume string) string
}

// NewDevices opens devices named prefix plus name(volume) in dir.
func NewDevices(dir, prefix string, name func(string) string) *Devices {
	return &Devices{dir: dir, prefix: prefix, name: name}
}

// Open opens a volume's block device for this process alone.
func (d *Devices) Open(ctx context.Context, volume string) (platform.File, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	name := d.name(volume)
	if name == "" || name != filepath.Base(name) {
		return nil, fmt.Errorf("%w: volume %q names no device", platform.ErrInvalidPath, volume)
	}
	handle, err := openExclusive(filepath.Join(d.dir, d.prefix+name))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, errors.Join(platform.ErrNotFound, fmt.Errorf("volume %q is not attached here: %w", volume, err))
	case errors.Is(err, syscall.EBUSY):
		return nil, errors.Join(platform.ErrLocked, fmt.Errorf("another process holds volume %q: %w", volume, err))
	case err != nil:
		return nil, normalizeFileError(err)
	}
	return &deviceFile{plain: file{handle: handle}}, nil
}

// deviceFile is a block device: a file of the disk's size that cannot be
// truncated, allocated or punched. It reads and writes as a file does, and
// hands sendfile the device as it hands it a file.
type deviceFile struct {
	plain file
}

func (f *deviceFile) ReadAt(ctx context.Context, destination []byte, offset int64) (int, error) {
	return f.plain.ReadAt(ctx, destination, offset)
}

func (f *deviceFile) WriteAt(ctx context.Context, source []byte, offset int64) (int, error) {
	return f.plain.WriteAt(ctx, source, offset)
}

func (f *deviceFile) Sync(ctx context.Context) error { return f.plain.Sync(ctx) }
func (f *deviceFile) Close() error                   { return f.plain.Close() }

// SyscallConn is the device as the kernel names it, for sendfile.
func (f *deviceFile) SyscallConn() (syscall.RawConn, error) { return f.plain.SyscallConn() }

func (f *deviceFile) Truncate(context.Context, int64) error {
	return fmt.Errorf("a block device: %w", errors.ErrUnsupported)
}

// Size is the device's size, which a block device's stat does not report:
// where a seek to its end lands.
func (f *deviceFile) Size(ctx context.Context) (int64, error) {
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	size, err := f.plain.handle.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, normalizeFileError(err)
	}
	return size, nil
}

var (
	_ platform.Devices = (*Devices)(nil)
	_ platform.File    = (*deviceFile)(nil)
)
