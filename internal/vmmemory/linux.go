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

	"github.com/semistrict/sproutfs/internal/vmwire"
)

// LinuxArena owns the host's shared memfd. The descriptor must only be given to
// trusted mapping clients in the Host's sharing domain.
//
// Its offsets are not its pages. The memfd is sized to the addresses the pager
// may place a page at and is sparse: an offset costs nothing until a page is
// put there, and Release punches it back out again. A RAM pager places a
// private page at the offset it has within its 2 MiB range, so it owns a whole
// run of 512 offsets per range any of whose pages it has copied, while the
// memory behind them stays what its resident budget allows.
type LinuxArena struct {
	file    *os.File
	mapping []byte
	offsets int
	// pageSize is the slot size this arena was made with, which must be the
	// page of the pager it is given to. A host runs one arena per pager, so the
	// two arenas of one host need not agree about it.
	pageSize int
	// backing is what the memory behind those slots is, which the session
	// states so the client can check the descriptor it is given against it.
	backing uint64
}

// NewLinuxArena creates offsets addresses of pageSize bytes each, over the
// memory that page is: the host's provisioned 2 MiB HugeTLB pool for a 2 MiB
// slot, which never falls back to ordinary pages and reports pool exhaustion as
// an allocation error, and an ordinary shared memfd for a 4 KiB one, which is
// the pod's own memory and which a host with swap may swap. Every other slot
// size is refused, because it is not a page a volume can be published in.
//
// The file is sparse and the mapping takes no reservation, so an arena of far
// more addresses than its pager may hold pages costs address space and nothing
// else. How many of them may hold memory at once is the pager's budget, not
// this.
func NewLinuxArena(offsets int, pageSize uint64) (*LinuxArena, error) {
	backing, err := vmwire.BackingFor(pageSize)
	if err != nil {
		return nil, fmt.Errorf("%w: an arena's slot is a pager's page: %w", ErrConfig, err)
	}
	size := int(pageSize)
	if offsets < 1 || uint64(offsets) > uint64(^uint64(0)>>1)/pageSize {
		return nil, ErrConfig
	}
	// The name carries the page, because a host has two of these and /proc is
	// where a qualification reads which memory a guest's mapping is really on.
	f, err := vmwire.ArenaMemfd(fmt.Sprintf("sproutfs-memory-%dk", pageSize>>10),
		pageSize, int64(offsets)*int64(size))
	if err != nil {
		return nil, err
	}
	mapping, err := syscall.Mmap(int(f.Fd()), 0, offsets*size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_NORESERVE)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &LinuxArena{file: f, mapping: mapping, offsets: offsets, pageSize: size, backing: backing}, nil
}

// PageSize is the slot this arena was made with, which must be the page of the
// pager it is given to.
func (a *LinuxArena) PageSize() uint64 { return uint64(a.pageSize) }

// Offsets is how many addresses this arena has, which is what its memfd is
// sized to and what an ATTACH states. Only the offsets a page has been put at
// hold memory; AllocatedBytes is how much that is.
func (a *LinuxArena) Offsets() int { return a.offsets }

// Backing is what the memory behind the slots is, as a session states it.
func (a *LinuxArena) Backing() uint64 { return a.backing }

func (a *LinuxArena) offset(ctx context.Context, slot int, length int) (int64, error) {
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	if slot < 0 || slot >= a.offsets || length != a.pageSize {
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
	// HugeTLB files do not implement write(2), so both arenas go through the
	// mmap. Allocate before touching it so exhaustion — of the pool, or of the
	// pod's memory — is an error rather than a process-killing SIGBUS.
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
	if count < 1 || slot < 0 || slot > a.offsets-count {
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
// between allocation and mapping. It is the memory this arena really holds,
// which is the pages put at its offsets and not the offsets themselves.
func (a *LinuxArena) AllocatedBytes() (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(a.file.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Blocks) * 512, nil
}
