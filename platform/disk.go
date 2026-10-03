package platform

import (
	"context"
	"io/fs"
)

type OpenOptions struct {
	Create    bool
	Exclusive bool
	Truncate  bool
	// Lock takes an exclusive lock on the file for as long as the returned
	// File is open, across every process on the machine. Open fails with
	// ErrLocked while another open File holds it. The lock ends with the File
	// or with the process that holds it, so a process that dies leaves no
	// lock behind. It does not combine with Truncate, which would change the
	// file before the lock is known to be free.
	Lock bool
	// Permissions controls newly created files. Zero selects the adapter's
	// secure default. Existing files may be narrowed to this mode by adapters.
	Permissions fs.FileMode
}

func (o OpenOptions) Validate() error {
	if o.Exclusive && !o.Create || o.Lock && o.Truncate || o.Permissions&^fs.FileMode(0o777) != 0 {
		return ErrInvalidPath
	}
	return nil
}

// Disk is the durable local-storage seam used by a process. File content is
// not guaranteed to survive power loss until Sync succeeds.
type Disk interface {
	Open(context.Context, string, OpenOptions) (File, error)
	// Remove durably records absence, including when returning ErrNotFound.
	// An ambiguous unlink must be retryable without skipping directory sync.
	Remove(context.Context, string) error
	Rename(context.Context, string, string) error
	// List returns lexicographically ordered file names beginning with prefix.
	// Names are relative to the disk root and use forward slashes.
	List(context.Context, string) ([]string, error)
	// SyncNamespace makes the current directory entries durable. Owners use it
	// before reconciling an ambiguous rename or unlink. It does not sync contents.
	SyncNamespace(context.Context) error
}

// Disks opens a Disk rooted at one local directory. It is the port a package
// that makes directories of its own is given instead of an adapter: a VMM's
// private staging sits beside the sockets and the binary an external process
// needs real paths for, so the directory is the package's and only the Disk
// over it is the process's choice of adapter.
type Disks func(root string) (Disk, error)

// DiskSpace reports the backing filesystem, including use outside this process.
// Available excludes blocks reserved for privileged users. Adapters without a
// physical filesystem may omit this optional interface.
type DiskSpace interface {
	Space(context.Context) (FilesystemSpace, error)
}

type FilesystemSpace struct {
	// ID identifies the backing filesystem across directory and file handles.
	ID               string
	Total, Available uint64
	// AllocationUnit is the filesystem block/fragment size used to round file
	// reservations. Zero selects byte granularity for nonphysical adapters.
	AllocationUnit int64
}

// FileAllocation reports the bytes the filesystem holds for one file. A sparse
// file holds less than its size. Adapters without a physical filesystem may
// omit this optional interface.
type FileAllocation interface {
	Allocated(context.Context) (int64, error)
}

// DeviceWrites reports the bytes the device under a directory has written, as
// the device counts them. The count covers every writer on the device, not only
// this process. It can go backwards when the device is replaced.
type DeviceWrites interface {
	BytesWritten(context.Context) (uint64, error)
}

type File interface {
	ReadAt(context.Context, []byte, int64) (int, error)
	WriteAt(context.Context, []byte, int64) (int, error)
	Truncate(context.Context, int64) error
	Sync(context.Context) error
	Size(context.Context) (int64, error)
	Close() error
}

// SparseFile can discard physical backing while preserving file offsets and
// length. A successful PunchHole makes the range read as zeroes. Like writes,
// it is volatile until Sync; scratch spill does not require durability.
type SparseFile interface {
	File
	PunchHole(context.Context, int64, int64) error
}

// AllocatingFile can reserve physical space for a range before it is written,
// so a later write into the range does not fail for want of space. The file
// grows to cover the range, and what was not written reads as zeroes. Like
// writes, it is volatile until Sync. An adapter whose filesystem cannot reserve
// space returns errors.ErrUnsupported, and a full filesystem ErrNoSpace.
type AllocatingFile interface {
	File
	Allocate(context.Context, int64, int64) error
}
