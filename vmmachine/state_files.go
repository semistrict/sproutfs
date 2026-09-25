package vmmachine

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/semistrict/sproutfs/platform"
)

// MaxStateBytes bounds one captured or restored VMM state.
const MaxStateBytes = 64 << 20

var ErrStateTooLarge = errors.New("vmmachine: VMM state exceeds the supported size")

// stateFiles owns one process's private staging namespace, inside the scratch
// directory a starting host wiped. Files are temporary and never recovery
// authority: a capture writes one, reads it back and deletes it. All handles
// and external writers must stop before Close.
//
// Nothing here is accounted. The only file that can grow is the VMM's own
// capture, bounded at MaxStateBytes by the file-size limit the VMM process
// runs under, and the whole directory goes with the process.
type stateFiles struct {
	mu     sync.Mutex
	disk   platform.Disk
	closed bool
}

func newStateFiles(raw platform.Disk) (*stateFiles, error) {
	if raw == nil {
		return nil, errors.New("vmmachine: staging namespace is required")
	}
	return &stateFiles{disk: raw}, nil
}

func (s *stateFiles) write(ctx context.Context, name string, data []byte) (err error) {
	file, err := s.disk.Open(ctx, name, platform.OpenOptions{Create: true, Exclusive: true, Permissions: 0o600})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if n, err := file.WriteAt(ctx, data, 0); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	return file.Sync(ctx)
}

func (s *stateFiles) remove(ctx context.Context, name string) error {
	err := s.disk.Remove(ctx, name)
	if errors.Is(err, platform.ErrNotFound) {
		return nil
	}
	return err
}

// capture invokes produce, which must return only after the VMM has finished
// writing its state file or has exited, and reads back what it wrote.
//
// Nothing here is made durable. This file is staging: it is read back and
// removed before the guest resumes, a host that restarts wipes the directory it
// is in, and the bytes it holds become authority only once the checkpoint that
// carries them is published. Flushing it would be a disk wait inside the pause
// for bytes nothing will ever look for, so the VMM is told not to sync it
// either.
func (s *stateFiles) capture(ctx context.Context, produce func() error) (data []byte, err error) {
	file, err := s.disk.Open(ctx, "capture.state", platform.OpenOptions{Create: true, Exclusive: true, Permissions: 0o600})
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, file.Close(), s.remove(context.WithoutCancel(ctx), "capture.state"))
	}()
	if err := produce(); err != nil {
		return nil, err
	}
	size, err := file.Size(ctx)
	if err != nil {
		return nil, err
	}
	if size < 0 || size > MaxStateBytes {
		return nil, ErrStateTooLarge
	}
	data = make([]byte, size)
	if size > 0 {
		_, err = file.ReadAt(ctx, data, 0)
	}
	return data, err
}

// Close removes whatever staging is left. The process's directory is removed
// after it, so a failure here is reported rather than retried.
func (s *stateFiles) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	ctx := context.Background()
	paths, err := s.disk.List(ctx, "")
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := s.remove(ctx, path); err != nil {
			return err
		}
	}
	s.closed = true
	return nil
}
