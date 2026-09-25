package platform

import (
	"context"
	"io/fs"
)

type OpenOptions struct {
	Create    bool
	Exclusive bool
	Truncate  bool
	// Permissions controls newly created files. Zero selects the adapter's
	// secure default. Existing files may be narrowed to this mode by adapters.
	Permissions fs.FileMode
}

func (o OpenOptions) Validate() error {
	if o.Exclusive && !o.Create || o.Permissions&^fs.FileMode(0o777) != 0 {
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
