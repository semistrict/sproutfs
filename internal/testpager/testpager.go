// Package testpager is a simulated arena and a simulated process memory region
// for suites that run a pager without a kernel: the simulation's campaigns, and
// the host and migration suites. The arena is the files a pager makes of it,
// and a mapping is one guest's page table over them.
//
// An isolated arena also holds its pager to who may read what, on every file it
// hands a memory region and every map. A file given writable is one memory
// region's private file: it is given to that one mapping only, as its file 0,
// and to nobody read-only. Every other file is only ever given read-only. A map
// names a file its mapping holds, and a writable map names file 0. So a guest's
// stores reach only its own region's file, and no page two regions map is in a
// private file, under everything a suite plays.
package testpager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/vmmemory"
)

// Arena is one pager's simulated page store: the files that pager makes. One
// lock covers every file, because a guest's store and the seal that
// write-protects it must be one step whichever file the page is in.
type Arena struct {
	mu       sync.Mutex
	files    []*File
	isolated bool
}

// NewArena is an arena for a pager of the given mode.
func NewArena(mode vmmemory.ArenaMode) *Arena {
	return &Arena{isolated: mode == vmmemory.ArenaIsolated}
}

// File is one file of an arena: a byte slice at every offset a page has been
// put at.
//
// It is a map rather than one entry per offset, because a real file is sparse:
// it has more addresses than it may ever hold pages at once, an offset costs
// nothing until a page is put there, and releasing one punches that memory
// back out.
type File struct {
	arena   *Arena
	id      int
	offsets int
	slots   map[int][]byte
	// writer is the one mapping this file was given to writable, and readers
	// how many were given it read-only. closed marks a file the pager gave back.
	writer  *Mapping
	readers int
	closed  bool
}

// File makes a file of offsets slots, every one of them a hole.
func (a *Arena) File(_ context.Context, offsets int) (vmmemory.ArenaFile, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := &File{arena: a, id: len(a.files), offsets: offsets, slots: make(map[int][]byte)}
	a.files = append(a.files, f)
	return f, nil
}

// Held is how many pages every file of the arena holds, which is the memory a
// pager's budget bounds.
func (a *Arena) Held() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	held := 0
	for _, f := range a.files {
		held += len(f.slots)
	}
	return held
}

// at refuses an address this file does not have, and a file its pager gave
// back.
func (f *File) at(slot int) error {
	if slot < 0 || slot >= f.offsets {
		return fmt.Errorf("arena offset %d is outside its %d", slot, f.offsets)
	}
	if f.closed {
		return fmt.Errorf("arena file %d was closed", f.id)
	}
	return nil
}

func (f *File) Read(_ context.Context, slot int, dst []byte) error {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	if err := f.at(slot); err != nil {
		return err
	}
	// An offset no page has been put at is a hole, and a hole reads as zeros.
	clear(dst)
	copy(dst, f.slots[slot])
	return nil
}

func (f *File) Write(_ context.Context, slot int, src []byte) error {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	if err := f.at(slot); err != nil {
		return err
	}
	if f.slots[slot] != nil {
		return fmt.Errorf("write into allocated slot %d of file %d", slot, f.id)
	}
	f.slots[slot] = bytes.Clone(src)
	return nil
}

// Equal compares two slots where they are, as a real file mapped into this
// process does, so a settle costs the comparison and no copy.
func (f *File) Equal(_ context.Context, first, second int) (bool, error) {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	return bytes.Equal(f.slots[first], f.slots[second]), nil
}

func (f *File) Release(_ context.Context, slot int) error {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	if err := f.at(slot); err != nil {
		return err
	}
	delete(f.slots, slot)
	return nil
}

// Close gives the file back, which its pager does only once nothing of it is
// held.
func (f *File) Close() error {
	f.arena.mu.Lock()
	defer f.arena.mu.Unlock()
	if len(f.slots) != 0 {
		return fmt.Errorf("arena file %d closed holding %d pages", f.id, len(f.slots))
	}
	f.closed = true
	return nil
}

// Page is what one page of a mapping maps: a slot of one file of the arena,
// or zeros where Slot is -1.
type Page struct {
	File, Slot int
	Writable   bool
}

// Mapping is one simulated process memory region's page table. Every lookup
// takes the arena lock, because a guest storing into a page races the seal
// that write-protects it: the store and the writability check must be one
// step, exactly as the hardware makes them.
type Mapping struct {
	arena *Arena
	mu    sync.Mutex
	pages map[uint64]Page
	// files is every file this mapping was given, by the number its maps name
	// it by.
	files map[int]*File
}

// NewMapping is an empty page table over the arena.
func NewMapping(a *Arena) *Mapping {
	return &Mapping{arena: a, pages: make(map[uint64]Page), files: make(map[int]*File)}
}

// GiveFile holds the pager to who may read a file.
func (m *Mapping) GiveFile(_ context.Context, number int, file vmmemory.ArenaFile, writable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	f, ok := file.(*File)
	switch {
	case !ok:
		return fmt.Errorf("file %d is a %T, not this arena's", number, file)
	case m.files[number] != nil:
		return fmt.Errorf("file %d given twice", number)
	case writable != (number == 0):
		return fmt.Errorf("file %d given writable=%t, and only file 0 is writable", number, writable)
	case !m.arena.isolated:
		// A shared arena is one file, and every memory region's.
	case writable && (f.writer != nil || f.readers > 0):
		return fmt.Errorf("file %d given writable to a second memory region", f.id)
	case !writable && f.writer != nil:
		return fmt.Errorf("the private file %d given read-only to another memory region", f.id)
	}
	if writable {
		f.writer = m
	} else {
		f.readers++
	}
	m.files[number] = f
	return nil
}

func (m *Mapping) DropFile(_ context.Context, number int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[number]
	if f == nil {
		return fmt.Errorf("drop of file %d, which was never given", number)
	}
	for page, p := range m.pages {
		if p.File == f.id && p.Slot >= 0 {
			return fmt.Errorf("drop of file %d while page %d maps it", number, page)
		}
	}
	delete(m.files, number)
	return nil
}

func (m *Mapping) Map(_ context.Context, page uint64, file, slot, count int, writable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[file]
	if f == nil || (writable && file != 0) {
		return fmt.Errorf("map of file %d writable=%t, which this memory region may not map so", file, writable)
	}
	for i := range count {
		m.pages[page+uint64(i)] = Page{f.id, slot + i, writable}
	}
	return nil
}

func (m *Mapping) MapZero(_ context.Context, page uint64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)] = Page{-1, -1, false}
	}
	return nil
}

func (m *Mapping) Protect(_ context.Context, page uint64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		p, ok := m.pages[page+uint64(i)]
		if !ok {
			return fmt.Errorf("protect of unmapped page %d", page+uint64(i))
		}
		p.Writable = false
		m.pages[page+uint64(i)] = p
	}
	return nil
}

func (m *Mapping) Revoke(_ context.Context, page uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pages, page)
	return nil
}

func (m *Mapping) Resolve(_ context.Context, page uint64, count int, writable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		p, ok := m.pages[page+uint64(i)]
		if !ok || p.Writable != writable {
			return errors.New("invalid resolution")
		}
	}
	return nil
}

// Page reports what one page maps, and whether it maps anything.
func (m *Mapping) Page(page uint64) (Page, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[page]
	return p, ok
}

// Read is the bytes one page maps, nil for zeros, and whether it maps
// anything.
func (m *Mapping) Read(page uint64) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[page]
	if !ok || p.Slot < 0 {
		return nil, ok
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	return bytes.Clone(m.arena.files[p.File].slots[p.Slot]), true
}

// Store writes one page's bytes the way a guest does: only a mapping that is
// writable right now accepts the store. A page a seal has write-protected
// reports false, which is the trap the caller answers with a write fault.
func (m *Mapping) Store(page uint64, value byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[page]
	if !ok || !p.Writable || p.Slot < 0 {
		return false
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	slot := m.arena.files[p.File].slots[p.Slot]
	for i := range slot {
		slot[i] = value
	}
	return true
}

// Forget empties the page table, which is what a process that has gone leaves.
func (m *Mapping) Forget() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.pages)
}

// Runs counts the mappings this page table is to its VMM process, which is
// what the kernel holds a VMA for: a run of consecutive pages at consecutive
// offsets of one file, with the same write access, is one mapping, and every
// break in any of them is another. A zero mapping owns no offset and is left
// out: what the placement rule governs is where the pages a pager holds sit,
// and a range of zeros is one mapping wherever they are.
func (m *Mapping) Runs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	numbers := make([]uint64, 0, len(m.pages))
	for page, p := range m.pages {
		if p.Slot >= 0 {
			numbers = append(numbers, page)
		}
	}
	slices.Sort(numbers)
	count := 0
	for i, page := range numbers {
		if i > 0 {
			previous, current := m.pages[numbers[i-1]], m.pages[page]
			if numbers[i-1]+1 == page && current.File == previous.File && current.Slot == previous.Slot+1 &&
				previous.Writable == current.Writable {
				continue
			}
		}
		count++
	}
	return count
}
