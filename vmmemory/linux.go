//go:build linux && (amd64 || arm64)

package vmmemory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/semistrict/sproutfs/internal/vmwire"
	"golang.org/x/sys/unix"
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
	file *os.File
	// mapping is every access this process makes to the arena's pages. On an
	// ordinary memfd it is advised never to be huge, so that a page allocated
	// through it is the one page the pager asked for and counted, whatever the
	// host's policy for its shared memory is.
	mapping []byte
	// huge is the same file mapped a second time, aligned to 2 MiB and advised
	// to be huge, on an ordinary memfd whose kernel has transparent huge pages
	// and nil otherwise. It is used for one thing: allocating the whole 2 MiB
	// blocks of a zero run, each of which the kernel then allocates and clears
	// as one huge page instead of 512 ordinary ones.
	huge    []byte
	offsets int
	// pageSize is the slot size this arena was made with, which must be the
	// page of the pager it is given to. A host runs one arena per pager, so the
	// two arenas of one host need not agree about it.
	pageSize int
	// backing is what the memory behind those slots is, which the session
	// states so the client can check the descriptor it is given against it.
	backing uint64
	// hugePolicy is the host's policy for huge pages in its shared memory, as
	// this arena found it; see HugePolicy.
	hugePolicy string
}

// hugeBytes is the transparent huge page an ordinary memfd's 2 MiB block is
// allocated as. It is a range, and the PMEM arena's explicit page: the unit a
// whole range of a guest can be one allocation at.
const hugeBytes = 2 << 20

// NewLinuxArena creates offsets addresses of pageSize bytes each, over the
// memory that page is: the host's provisioned 2 MiB HugeTLB pool for a 2 MiB
// slot, which never falls back to ordinary pages and reports pool exhaustion as
// an allocation error, and an ordinary shared memfd for a 4 KiB one, which is
// the pod's own memory and which a host with swap may swap. Every other slot
// size is refused, because it is not a page a volume can be published in.
//
// The file is sparse and the mappings take no reservation, so an arena of far
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
	a := &LinuxArena{file: f, offsets: offsets, pageSize: size, backing: backing, hugePolicy: "hugetlb"}
	a.mapping, err = syscall.Mmap(int(f.Fd()), 0, offsets*size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_NORESERVE)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if backing != vmwire.BackingHugeTLB {
		if err := a.mapHuge(); err != nil {
			return nil, errors.Join(err, a.Close())
		}
	}
	return a, nil
}

// mapHuge gives an ordinary memfd's arena its two mappings: the ordinary one
// advised never to be huge, and the aligned one advised to be. A kernel built
// without transparent huge pages refuses both pieces of advice, and its arena
// has the ordinary mapping only, which is every page at 4 KiB.
func (a *LinuxArena) mapHuge() error {
	if err := unix.Madvise(a.mapping, unix.MADV_NOHUGEPAGE); err != nil {
		if !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("advising the arena's mapping against huge pages: %w", err)
		}
		a.hugePolicy = "never"
		return nil
	}
	policy, err := shmemHugePolicy()
	if err != nil {
		return err
	}
	a.hugePolicy = policy
	length := uintptr(len(a.mapping))
	// The mapping has to start on a 2 MiB boundary for the kernel to put a huge
	// page at its offsets, which are 2 MiB aligned in the file too. So the
	// address space is reserved with a block to spare, the file is mapped at
	// the first boundary within it, and the spare either side is given back.
	reserved, err := unix.MmapPtr(-1, 0, nil, length+hugeBytes, unix.PROT_NONE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_NORESERVE)
	if err != nil {
		return fmt.Errorf("reserving the arena's huge mapping: %w", err)
	}
	head := int((uintptr(reserved)+hugeBytes-1)&^(hugeBytes-1) - uintptr(reserved))
	aligned := unsafe.Add(reserved, head)
	if _, err := unix.MmapPtr(int(a.file.Fd()), 0, aligned, length,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_NORESERVE|unix.MAP_FIXED); err != nil {
		return errors.Join(fmt.Errorf("mapping the arena huge: %w", err), unix.MunmapPtr(reserved, length+hugeBytes))
	}
	a.huge = unsafe.Slice((*byte)(aligned), length)
	var spare []error
	if head > 0 {
		spare = append(spare, unix.MunmapPtr(reserved, uintptr(head)))
	}
	if tail := hugeBytes - head; tail > 0 {
		spare = append(spare, unix.MunmapPtr(unsafe.Add(aligned, length), uintptr(tail)))
	}
	if err := errors.Join(spare...); err != nil {
		return fmt.Errorf("giving back the spare around the arena's huge mapping: %w", err)
	}
	if err := unix.Madvise(a.huge, unix.MADV_HUGEPAGE); err != nil {
		return fmt.Errorf("advising the arena's huge mapping: %w", err)
	}
	return nil
}

// HugePolicy is the host's policy for transparent huge pages in its shared
// memory, as this arena found it, or "hugetlb" for an arena on the HugeTLB pool,
// whose pages are explicit and which that policy says nothing about.
//
// Under "advise", "within_size" or "always" the whole 2 MiB blocks of a zero
// run are huge pages; under "never" or "deny" they are 512 ordinary ones. It is
// a record of what the host gave and not a setting of the pager's, which runs
// the same either way: a huge page changes how much the kernel allocates and
// clears at once, and never what the pager owns, counts or releases, which
// stays one slot. Every other allocation is one ordinary page under any policy.
func (a *LinuxArena) HugePolicy() string { return a.hugePolicy }

// shmemHugePolicy is the bracketed word of the host's shmem_enabled, or never
// where the kernel has no such file at all.
func shmemHugePolicy() (string, error) {
	raw, err := os.ReadFile("/sys/kernel/mm/transparent_hugepage/shmem_enabled")
	if errors.Is(err, fs.ErrNotExist) {
		return "never", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the host's shmem huge-page policy: %w", err)
	}
	for _, field := range strings.Fields(string(raw)) {
		if chosen, ok := strings.CutPrefix(field, "["); ok {
			return strings.TrimSuffix(chosen, "]"), nil
		}
	}
	return "", fmt.Errorf("the host's shmem huge-page policy names none: %q", raw)
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
	// pod's memory — is an error rather than a process-killing SIGBUS. One slot
	// is always one ordinary page on an ordinary memfd; see Zero.
	if err := a.Zero(ctx, slot, 1); err != nil {
		return err
	}
	copy(a.mapping[off:off+int64(len(src))], src)
	return nil
}

// Zero allocates count consecutive punched slots without writing them, and a
// read of one sees the zeros the kernel cleared it to.
//
// On the HugeTLB pool that is a fallocate, which KEEP_SIZE keeps off the sealed
// size. On an ordinary memfd it is a populating write fault through a mapping,
// because a mapping is the one place the kernel lets a caller say which
// allocations may be huge: the whole 2 MiB blocks of the run go through the
// huge mapping and are one huge page each, and its ends through the ordinary
// mapping, one ordinary page per slot. The run is exactly what is allocated
// either way. A huge page for an end would take memory for slots the pager
// never asked for and never counted, which is why a single page — a store's
// copy, a load — is always an ordinary one.
func (a *LinuxArena) Zero(ctx context.Context, slot, count int) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if count < 1 || slot < 0 || slot > a.offsets-count {
		return ErrRange
	}
	start, end := slot*a.pageSize, (slot+count)*a.pageSize
	if a.backing == vmwire.BackingHugeTLB {
		const keepSize = 1 // FALLOC_FL_KEEP_SIZE
		return a.fallocate(ctx, keepSize, int64(start), int64(end-start))
	}
	first, last := (start+hugeBytes-1)&^(hugeBytes-1), end&^(hugeBytes-1)
	if a.huge == nil || first >= last {
		return populate(ctx, a.mapping[start:end])
	}
	return errors.Join(populate(ctx, a.mapping[start:first]), populate(ctx, a.huge[first:last]),
		populate(ctx, a.mapping[last:end]))
}

// populate allocates the pages behind b by write-faulting every one of them,
// which reports an allocation the pod has no memory for as an error rather than
// as a signal to whichever access would have faulted first. A fatal signal
// interrupts it, which the retry leaves to the context.
func populate(ctx context.Context, b []byte) error {
	for len(b) > 0 {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		err := unix.Madvise(b, unix.MADV_POPULATE_WRITE)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
	return nil
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
func (a *LinuxArena) Close() error {
	var huge error
	if a.huge != nil {
		huge = unix.MunmapPtr(unsafe.Pointer(&a.huge[0]), uintptr(len(a.huge)))
	}
	return errors.Join(syscall.Munmap(a.mapping), huge, a.file.Close())
}

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
