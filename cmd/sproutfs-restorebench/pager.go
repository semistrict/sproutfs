package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A read through a pager is what a guest's fault costs: the real pager, with
// the host's read-ahead run, over the real store, faulting one page at a time
// and reading it out of the arena once its fault returns. What the guest does
// not have is a VMM: the mapping here records what the pager maps, and the
// arena is this process's memory.
//
// Two units read through one: unitFault faults as the pager does, its page
// first and the rest of its run behind it, and unitRunFirst marks every fault
// as a post-copy stream's, which reads its whole run before the page is
// installed, as every fault did before 2026-10-04 (vmmemory.WithStream).

// pagerResidentBytes is the memory the pager's arena may hold, past which it
// evicts. Pages read back are clean, so an eviction costs no spill.
const pagerResidentBytes = 2 << 30

// pagerReader is one read's pager: a host of its own, so nothing a case reads
// is resident when it begins, and the one memory region it attaches.
type pagerReader struct {
	host    *vmmemory.Host
	region  *vmmemory.MemoryRegion
	mapping *recordingMapping
	arena   *memoryArena
	spill   platform.File
	// runFirst marks every fault as a stream's.
	runFirst bool
}

// newPagerReader attaches the published volume to a pager of page geometry's
// page with the host's read-ahead run.
func newPagerReader(ctx context.Context, disk platform.Disk, store *checkpoint.Store, index *checkpoint.Index,
	pageSize, pages uint64, runFirst bool) (*pagerReader, error) {
	resident := int(min(pages, pagerResidentBytes/pageSize))
	readAhead := int(faultRunBytes / pageSize)
	budget, err := resource.New(int64(resident) * int64(pageSize) * 2)
	if err != nil {
		return nil, err
	}
	spill, err := disk.Open(ctx, fmt.Sprintf("pager-spill-%d", spillNumber.Add(1)), platform.OpenOptions{Create: true})
	if err != nil {
		return nil, err
	}
	arena := &memoryArena{pageSize: int(pageSize)}
	host, err := vmmemory.New(ctx, budget, vmmemory.Config{PageSize: pageSize, ResidentPages: resident,
		LogicalPages: int(pages), DirtyPages: readAhead, ReadAheadPages: readAhead, Arena: vmmemory.ArenaShared,
		ConcurrentIO: 16}, arena, spill)
	if err != nil {
		return nil, errorsJoinClose(err, spill)
	}
	mapping := &recordingMapping{arena: arena, pages: make(map[uint64]mappedSlot)}
	backing := &storeBacking{store: store, index: index, size: pages * pageSize, pageSize: pageSize}
	region, err := host.Attach(ctx, vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: backing}, mapping)
	if err != nil {
		_ = host.Close(ctx)
		return nil, errorsJoinClose(err, spill)
	}
	return &pagerReader{host: host, region: region, mapping: mapping, arena: arena, spill: spill,
		runFirst: runFirst}, nil
}

var spillNumber atomic.Uint64

func errorsJoinClose(err error, file platform.File) error {
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("%w (closing the spill file: %v)", err, closeErr)
	}
	return err
}

// read is the walk's reader: each page of [offset, offset+len(dst)) faulted
// in where the pager does not map it, then copied out of the arena.
func (p *pagerReader) read(ctx context.Context, offset uint64, dst []byte) error {
	if p.runFirst {
		ctx = vmmemory.WithStream(ctx)
	}
	size := uint64(p.arena.pageSize)
	for at := uint64(0); at < uint64(len(dst)); at += size {
		page := (offset + at) / size
		for {
			if p.mapping.copyOut(page, dst[at:at+size]) {
				break
			}
			if err := p.region.Fault(ctx, page, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// close settles the prefetches, detaches the memory region and closes the
// pager, and reports what the pager did.
func (p *pagerReader) close(ctx context.Context) (vmmemory.Stats, error) {
	if err := p.region.SettlePrefetches(ctx); err != nil {
		return vmmemory.Stats{}, err
	}
	stats, err := p.host.Stats(ctx)
	if err != nil {
		return vmmemory.Stats{}, err
	}
	p.mapping.forget()
	if err := p.region.Detach(ctx); err != nil {
		return stats, err
	}
	if err := p.host.Close(ctx); err != nil {
		return stats, err
	}
	return stats, p.spill.Close()
}

// storeBacking is one volume of a published checkpoint, read through a store,
// as a pager's backing. It is what a volume.Volume is to a pager with nothing
// written since the checkpoint.
type storeBacking struct {
	store          *checkpoint.Store
	index          *checkpoint.Index
	size, pageSize uint64
}

func (b *storeBacking) Size() uint64     { return b.size }
func (b *storeBacking) PageSize() uint64 { return b.pageSize }

func (b *storeBacking) Load(ctx context.Context, offset uint64, dst []byte) error {
	return b.store.Read(ctx, b.index, volume, offset, dst)
}

func (b *storeBacking) LoadPages(ctx context.Context, offset uint64, dst []byte, wanted []bool) error {
	return b.store.ReadPages(ctx, b.index, volume, offset, dst, wanted)
}

func (b *storeBacking) Verify(context.Context) error { return nil }

func (b *storeBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	return b.index.Locate(ctx, volume, offset, length)
}

// memoryArena is a pager's arena in this process's memory: one map of slots a
// file, a slot holding nothing until a page is put there.
type memoryArena struct {
	pageSize int
	mu       sync.Mutex
}

func (a *memoryArena) File(_ context.Context, _ int) (vmmemory.ArenaFile, error) {
	return &memoryFile{arena: a, slots: make(map[int][]byte)}, nil
}

type memoryFile struct {
	arena *memoryArena
	slots map[int][]byte
}

func (f *memoryFile) Read(_ context.Context, slot int, dst []byte) error {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	clear(dst)
	copy(dst, f.slots[slot])
	return nil
}

func (f *memoryFile) Write(_ context.Context, slot int, src []byte) error {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	page := f.slots[slot]
	if page == nil {
		page = make([]byte, f.arena.pageSize)
		f.slots[slot] = page
	}
	copy(page, src)
	return nil
}

func (f *memoryFile) Release(_ context.Context, slot int) error {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	delete(f.slots, slot)
	return nil
}

// mappedSlot is the file and slot one page maps.
type mappedSlot struct {
	file *memoryFile
	slot int
}

// recordingMapping is what the pager maps, as a VMM's would be, without a VMM:
// each page's file and slot, and the files it was given.
type recordingMapping struct {
	arena *memoryArena
	files map[int]*memoryFile
	pages map[uint64]mappedSlot
}

func (m *recordingMapping) GiveFile(_ context.Context, number int, file vmmemory.ArenaFile, _ bool) error {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	f, ok := file.(*memoryFile)
	if !ok {
		return fmt.Errorf("file %d is a %T, not this arena's", number, file)
	}
	if m.files == nil {
		m.files = make(map[int]*memoryFile)
	}
	m.files[number] = f
	return nil
}

func (m *recordingMapping) DropFile(_ context.Context, number int) error {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	delete(m.files, number)
	return nil
}

func (m *recordingMapping) Map(_ context.Context, page uint64, file, slot, count int, _ bool) error {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	f := m.files[file]
	if f == nil {
		return fmt.Errorf("a map of file %d, which was never given", file)
	}
	for at := range count {
		m.pages[page+uint64(at)] = mappedSlot{file: f, slot: slot + at}
	}
	return nil
}

func (m *recordingMapping) MapZero(_ context.Context, page uint64, count int) error {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	for at := range count {
		m.pages[page+uint64(at)] = mappedSlot{slot: -1}
	}
	return nil
}

func (m *recordingMapping) Revoke(_ context.Context, page uint64) error {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	delete(m.pages, page)
	return nil
}

func (m *recordingMapping) Resolve(context.Context, uint64, int, bool) error { return nil }
func (m *recordingMapping) Protect(context.Context, uint64, int) error       { return nil }

// copyOut copies the page the mapping maps at page into dst, and reports
// whether it maps one. A revocation takes the page away under the same lock,
// before its slot goes back, so what it copies is the page's.
func (m *recordingMapping) copyOut(page uint64, dst []byte) bool {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	mapped, ok := m.pages[page]
	if !ok {
		return false
	}
	if mapped.slot < 0 {
		clear(dst)
		return true
	}
	copy(dst, mapped.file.slots[mapped.slot])
	return true
}

// forget drops every mapping, as a VMM's exit does, before the detach.
func (m *recordingMapping) forget() {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	clear(m.pages)
}
