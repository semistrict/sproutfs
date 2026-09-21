//go:build linux && (amd64 || arm64)

package vmmemory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

// LinuxArena owns the host's fixed-size shared memfd. The descriptor must only
// be given to trusted mapping clients in the Host's sharing domain.
type LinuxArena struct {
	file    *os.File
	mapping []byte
	pages   int
	// pageSize is the slot size this arena was made with, which must be the
	// page of the pager it is given to. A host runs one arena per pager, so the
	// two arenas of one host need not agree about it.
	pageSize int
}

// NewLinuxArena creates pages slots of pageSize bytes each. The backing is an
// explicit 2 MiB HugeTLB memfd, so a slot size other than that is refused here:
// the pool's page is what the arena is made of, and a 4 KiB arena is ordinary
// memory this build does not yet allocate. The host must provision the pool;
// allocation never falls back to ordinary pages.
func NewLinuxArena(pages int, pageSize uint64) (*LinuxArena, error) {
	if pageSize != checkpoint.PageSize2MiB {
		return nil, fmt.Errorf("%w: a HugeTLB arena's slot is %d bytes, not %d",
			ErrConfig, checkpoint.PageSize2MiB, pageSize)
	}
	size := int(pageSize)
	if pages < 1 || uint64(pages) > uint64(^uint64(0)>>1)/pageSize {
		return nil, ErrConfig
	}
	f, err := vmwire.HugeMemfd("sproutfs-memory", int64(pages)*int64(size))
	if err != nil {
		return nil, err
	}
	mapping, err := syscall.Mmap(int(f.Fd()), 0, pages*size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_NORESERVE)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &LinuxArena{file: f, mapping: mapping, pages: pages, pageSize: size}, nil
}

func (a *LinuxArena) offset(ctx context.Context, slot int, length int) (int64, error) {
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	if slot < 0 || slot >= a.pages || length != a.pageSize {
		return 0, ErrRange
	}
	return int64(slot) * int64(a.pageSize), nil
}
func (a *LinuxArena) Read(ctx context.Context, slot int, dst []byte) error {
	off, err := a.offset(ctx, slot, len(dst))
	if err != nil {
		return err
	}
	n, err := a.file.ReadAt(dst, off)
	if err == nil && n != len(dst) {
		err = io.ErrUnexpectedEOF
	}
	return err
}
func (a *LinuxArena) Write(ctx context.Context, slot int, src []byte) error {
	off, err := a.offset(ctx, slot, len(src))
	if err != nil {
		return err
	}
	// HugeTLB files do not implement write(2). Allocate before touching the
	// mmap so pool exhaustion is an error rather than a process-killing SIGBUS.
	if err := a.Zero(ctx, slot, 1); err != nil {
		return err
	}
	copy(a.mapping[off:off+int64(len(src))], src)
	return nil
}

// Zero allocates count consecutive punched slots without writing them. Their
// pages are cleared when a mapping first installs them, and a read before that
// sees the hole's zeros. KEEP_SIZE leaves the sealed size alone.
func (a *LinuxArena) Zero(ctx context.Context, slot, count int) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if count < 1 || slot < 0 || slot > a.pages-count {
		return ErrRange
	}
	const keepSize = 1 // FALLOC_FL_KEEP_SIZE
	return a.fallocate(ctx, keepSize, int64(slot)*int64(a.pageSize), int64(count)*int64(a.pageSize))
}

// Equal compares two slots where they are, without copying either out. The
// arena is mapped into this process, so a settle's comparison is one
// bytes.Equal over the pair and the memory traffic is the pages themselves.
func (a *LinuxArena) Equal(ctx context.Context, first, second int) (bool, error) {
	x, err := a.offset(ctx, first, a.pageSize)
	if err != nil {
		return false, err
	}
	y, err := a.offset(ctx, second, a.pageSize)
	if err != nil {
		return false, err
	}
	size := int64(a.pageSize)
	return bytes.Equal(a.mapping[x:x+size], a.mapping[y:y+size]), nil
}

func (a *LinuxArena) Release(ctx context.Context, slot int) error {
	off, err := a.offset(ctx, slot, a.pageSize)
	if err != nil {
		return err
	}
	return a.fallocate(ctx, 3, off, int64(a.pageSize))
}

// HugeTLB allocation can observe a runtime signal after dropping its locks.
// Retrying the same allocation/punch is safe even after partial progress.
func (a *LinuxArena) fallocate(ctx context.Context, mode uint32, offset, length int64) error {
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		err := syscall.Fallocate(int(a.file.Fd()), mode, offset, length)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// Close is valid only after every process using this arena has detached.
func (a *LinuxArena) Close() error { return errors.Join(syscall.Munmap(a.mapping), a.file.Close()) }

// AllocatedBytes reports physically allocated memfd blocks, including slots
// between allocation and mapping. The arena is never allowed to exceed its size.
func (a *LinuxArena) AllocatedBytes() (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(a.file.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Blocks) * 512, nil
}
