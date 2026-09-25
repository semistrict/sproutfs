package real

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/semistrict/sproutfs/platform"
)

type Disk struct {
	root string
}

func NewDisk(root string) (*Disk, error) {
	if root == "" {
		return nil, platform.ErrInvalidPath
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, err
	}
	return &Disk{root: absolute}, nil
}

func (d *Disk) Open(ctx context.Context, name string, options platform.OpenOptions) (platform.File, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	path, err := d.resolve(name)
	if err != nil {
		return nil, err
	}
	if options.Create {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, unchangedSpaceError(err)
		}
	}
	flags := os.O_RDWR
	if options.Create {
		flags |= os.O_CREATE
	}
	if options.Exclusive {
		flags |= os.O_EXCL
	}
	if options.Truncate {
		flags |= os.O_TRUNC
	}
	permissions := cmp.Or(options.Permissions, 0o640)
	handle, err := os.OpenFile(path, flags, permissions)
	if err != nil {
		return nil, unchangedSpaceError(err)
	}
	if options.Create {
		if err := handle.Chmod(permissions); err != nil {
			_ = handle.Close()
			return nil, normalizeFileError(err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			_ = handle.Close()
			return nil, normalizeFileError(err)
		}
	}
	if err := context.Cause(ctx); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return &file{handle: handle}, nil
}

func (d *Disk) Remove(ctx context.Context, name string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	path, err := d.resolve(name)
	if err != nil {
		return err
	}
	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return unchangedSpaceError(removeErr)
	}
	// A preceding unlink may have succeeded while its directory sync failed.
	// Retrying an absent path must finish that durability step. If an ancestor
	// is absent too, sync the nearest existing directory that records it.
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		err := syncDirectory(directory)
		if err == nil {
			return normalizeFileError(removeErr)
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(directory) == directory {
			return normalizeFileError(err)
		}
	}
}

func (d *Disk) Rename(ctx context.Context, oldName, newName string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	oldPath, err := d.resolve(oldName)
	if err != nil {
		return err
	}
	newPath, err := d.resolve(newName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o750); err != nil {
		return unchangedSpaceError(err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return unchangedSpaceError(err)
	}
	if err := syncDirectory(filepath.Dir(oldPath)); err != nil {
		return normalizeFileError(err)
	}
	if filepath.Dir(oldPath) != filepath.Dir(newPath) {
		return normalizeFileError(syncDirectory(filepath.Dir(newPath)))
	}
	return nil
}

func (d *Disk) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if _, err := d.resolve(prefix); err != nil {
			return nil, err
		}
	}
	var names []string
	err := filepath.WalkDir(d.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(d.root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return nil, normalizeFileError(err)
	}
	slices.Sort(names)
	return names, nil
}

func (d *Disk) SyncNamespace(ctx context.Context) error {
	var directories []string
	if err := filepath.WalkDir(d.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return normalizeFileError(err)
	}
	// Children precede parents, including parents of newly created directories.
	for i := len(directories) - 1; i >= 0; i-- {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if err := syncDirectory(directories[i]); err != nil {
			return normalizeFileError(err)
		}
	}
	return nil
}

func (d *Disk) resolve(name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.IndexByte(name, 0) >= 0 {
		return "", platform.ErrInvalidPath
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", platform.ErrInvalidPath
	}
	resolved := filepath.Join(d.root, clean)
	relative, err := filepath.Rel(d.root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", platform.ErrInvalidPath
	}
	return resolved, nil
}

type file struct {
	handle *os.File
}

func (f *file) ReadAt(ctx context.Context, destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, platform.ErrInvalidRange
	}
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	n, err := f.handle.ReadAt(destination, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		err = normalizeFileError(err)
	}
	return n, err
}

func (f *file) WriteAt(ctx context.Context, source []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, platform.ErrInvalidRange
	}
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	n, err := f.handle.WriteAt(source, offset)
	if err != nil {
		return n, normalizeFileError(err)
	}
	if n != len(source) {
		return n, io.ErrShortWrite
	}
	return n, context.Cause(ctx)
}

func (f *file) Truncate(ctx context.Context, size int64) error {
	if size < 0 {
		return platform.ErrInvalidRange
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return normalizeFileError(f.handle.Truncate(size))
}

func (f *file) Sync(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := f.handle.Sync(); err != nil {
		return normalizeFileError(err)
	}
	return context.Cause(ctx)
}

func (f *file) Size(ctx context.Context) (int64, error) {
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	info, err := f.handle.Stat()
	if err != nil {
		return 0, normalizeFileError(err)
	}
	return info.Size(), nil
}

func (f *file) Close() error { return normalizeFileError(f.handle.Close()) }

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func normalizeFileError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.Join(platform.ErrNotFound, err)
	case errors.Is(err, os.ErrExist):
		return errors.Join(platform.ErrAlreadyExists, err)
	case errors.Is(err, os.ErrClosed):
		return errors.Join(platform.ErrClosed, err)
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return errors.Join(platform.ErrNoSpace, err)
	default:
		return err
	}
}

var _ platform.Disk = (*Disk)(nil)
var _ platform.File = (*file)(nil)

func (f *file) PunchHole(ctx context.Context, offset, length int64) error {
	if offset < 0 || length <= 0 || offset > int64(^uint64(0)>>1)-length {
		return platform.ErrInvalidRange
	}
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		err := punchHole(f.handle.Fd(), offset, length)
		if !errors.Is(err, syscall.EINTR) {
			return normalizeFileError(err)
		}
	}
}

// Only failures before the target operation may carry this guarantee. Never
// apply it to a directory-sync failure after create, unlink or rename.
func unchangedSpaceError(err error) error {
	err = normalizeFileError(err)
	if errors.Is(err, platform.ErrNoSpace) {
		return errors.Join(err, platform.ErrFileUnchanged)
	}
	return err
}
