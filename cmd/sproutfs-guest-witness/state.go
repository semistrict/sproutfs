package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// state is what one guest holds: a buffer of memory it keeps resident and a
// file of the same size on its disk, both of them the pattern of the seed this
// witness was filled under.
//
// Both halves matter and for different reasons. The memory is what a fork
// shares, what a migration streams and what a stop publishes out of the host's
// pages; the file is what goes through the guest's own filesystem into the
// PMEM volume behind it. A guest that came back with its memory whole and its
// disk rewound, or the other way round, passes half of this and fails the
// other.
type state struct {
	// mu admits one operation at a time: a check reading every byte must not
	// run beside a mutate rewriting some of them.
	mu sync.Mutex
	// seed is what this witness was last filled under. A mutate uses it; a
	// check is told one instead, because the expectation is the script's.
	seed   uint64
	memory []byte
	file   *os.File
	size   int64
}

// mismatchError is one byte that is not what it must be: where it was, its
// offset, and the two values. The first one found ends the check, because the
// offset is what an operator needs and a million more of them say nothing
// extra.
type mismatchError struct {
	// Where is "memory" or "disk".
	Where     string
	Offset    int64
	Got, Want byte
	// Seed and Step are what the check was made against, which is the other
	// half of what an operator needs: a guest can be whole and be the wrong
	// guest.
	Seed, Step uint64
}

func (m *mismatchError) Error() string {
	return fmt.Sprintf("%s differs at offset %d: it holds %#02x and (seed %d, step %d) is %#02x",
		m.Where, m.Offset, m.Got, m.Seed, m.Step, m.Want)
}

// open allocates the buffer and the file a witness holds. The file is created
// and truncated to the size: a witness is the whole of what this guest is
// checked on, so what was in it before is not.
func open(path string, size int64) (*state, error) {
	if size <= 0 || size%pageBytes != 0 {
		return nil, fmt.Errorf("a witness is a whole number of %d-byte pages, not %d bytes",
			pageBytes, size)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the witness file %s: %w", path, err)
	}
	if err := requireDAX(file, path); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &state{memory: make([]byte, size), file: file, size: size}, nil
}

func (s *state) close() error { return s.file.Close() }

// pages is how many pages this witness holds.
func (s *state) pages() uint64 { return uint64(s.size) / pageBytes }

// fill writes the pattern of (seed, step 0) over every page of memory and of
// the file, and makes seed this witness's own. It is what a guest that has just
// booted runs, and what a fork's child runs once it has been checked against
// its parent: from there the child diverges under a seed of its own, which is
// what makes two children of one parent tell each other apart.
func (s *state) fill(seed uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed = seed
	for page := range s.pages() {
		writePage(s.memory[page*pageBytes:(page+1)*pageBytes], seed, 0, page)
	}
	if err := s.writeThrough(0, s.memory); err != nil {
		return err
	}
	return s.file.Sync()
}

// mutate rewrites the pages this step takes with the pattern of (this
// witness's seed, step), in memory and in the file. It reports how many pages
// it wrote, which is what says a step was a scattered fraction rather than
// everything or nothing.
//
// Every step from one upwards must be applied in order: a check works out which
// step last wrote each page, so a run that skipped one would expect bytes
// nothing ever wrote.
func (s *state) mutate(step uint64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if step == 0 {
		return 0, fmt.Errorf("step 0 is the fill: mutate to 1 or higher")
	}
	written := 0
	for page := range s.pages() {
		if !touches(s.seed, step, page) {
			continue
		}
		at := int64(page) * pageBytes
		writePage(s.memory[at:at+pageBytes], s.seed, step, page)
		if err := s.writeThrough(at, s.memory[at:at+pageBytes]); err != nil {
			return written, err
		}
		written++
	}
	if err := s.file.Sync(); err != nil {
		return written, err
	}
	return written, nil
}

// check requires every byte of memory and every byte of the file to be what
// (seed, step) says. The seed and the step are the caller's rather than this
// witness's: a guest that is whole can still be the wrong guest, and a check
// against the seed the script believes this VM carries is what catches a fork
// whose child came back holding a sibling's bytes.
//
// The file is read again rather than compared against the buffer. Reading it
// through the guest's own filesystem is the only thing that says the bytes
// reached the volume behind it; comparing two copies in this process would
// report a disk that was never written as though it held what memory does.
func (s *state) check(seed, step uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make([]byte, pageBytes)
	for page := range s.pages() {
		at := int64(page) * pageBytes
		expect(want, seed, step, page)
		if err := compare("memory", at, s.memory[at:at+pageBytes], want, seed, step); err != nil {
			return err
		}
	}
	return checkDisk(s.file, s.size, seed, step)
}

// checkFile requires every byte of the witness file at path to be what (seed,
// step) says, with no witness resident and no memory to check.
//
// It is what a cold-started guest answers with. A cold start discards the
// guest's memory and the VMM state with it, so the process that held the buffer
// is gone and /run, which is a tmpfs, was cleared by the boot: the file on the
// guest's own disk is the whole of what survived, and it is exactly what the
// last checkpoint published. So this opens the file as it is, takes its size
// from the file itself, and checks it page by page against the same arithmetic
// every other check uses.
func checkFile(path string, seed, step uint64) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("the witness file %s: %w", path, err)
	}
	defer file.Close()
	if err := requireDAX(file, path); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("the witness file %s: %w", path, err)
	}
	size := info.Size()
	if size <= 0 || size%pageBytes != 0 {
		return fmt.Errorf("the witness file %s is %d bytes, which is not a whole number of %d-byte pages",
			path, size, pageBytes)
	}
	return checkDisk(file, size, seed, step)
}

// checkDisk requires every byte of size bytes of file to be what (seed, step)
// says, naming the first that is not.
//
// The file is read in chunks rather than a page at a time: a witness of a few
// hundred megabytes is tens of thousands of pages, and a check that made a read
// call for each of them would spend the whole of its time in the guest's
// syscall path rather than on its disk.
func checkDisk(file *os.File, size int64, seed, step uint64) error {
	want := make([]byte, pageBytes)
	got := make([]byte, checkChunk)
	for at := int64(0); at < size; at += checkChunk {
		chunk := got[:min(int64(len(got)), size-at)]
		if _, err := file.ReadAt(chunk, at); err != nil {
			return fmt.Errorf("reading the witness file at %d: %w", at, err)
		}
		for offset := int64(0); offset < int64(len(chunk)); offset += pageBytes {
			page := uint64(at+offset) / pageBytes
			expect(want, seed, step, page)
			if err := compare("disk", at+offset,
				chunk[offset:offset+pageBytes], want, seed, step); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkChunk is how much of the file one read call takes. It is a whole number
// of pages, so the comparison below it still happens a page at a time.
const checkChunk = 1 << 20

// compare reports the first byte of one page that is not what it must be.
func compare(where string, at int64, got, want []byte, seed, step uint64) error {
	for index := range got {
		if got[index] != want[index] {
			return &mismatchError{Where: where, Offset: at + int64(index),
				Got: got[index], Want: want[index], Seed: seed, Step: step}
		}
	}
	return nil
}

// writeThrough writes bytes into the file at an offset. It is a plain write:
// the point of the file is that the guest's filesystem and the volume behind it
// carry the bytes, so nothing here bypasses either.
func (s *state) writeThrough(at int64, data []byte) error {
	if _, err := s.file.WriteAt(data, at); err != nil {
		return fmt.Errorf("writing the witness file at %d: %w", at, err)
	}
	return nil
}

// parseSize reads a size the way an operator writes one: plain bytes, or a
// number with a K, M or G suffix, with or without an iB after it. A size that
// is not a whole number of pages is refused rather than rounded — the pattern
// is generated per page, and a partial page at the end is a page the
// expectation and the guest would disagree about.
func parseSize(text string) (int64, error) {
	trimmed := strings.TrimSpace(text)
	unit := int64(1)
	for _, suffix := range []struct {
		name  string
		scale int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	} {
		if rest, found := strings.CutSuffix(trimmed, suffix.name); found {
			trimmed, unit = rest, suffix.scale
			break
		}
	}
	count, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("%q is not a size such as 256M", text)
	}
	size := count * unit
	if size%pageBytes != 0 {
		return 0, fmt.Errorf("%q is %d bytes, which is not a whole number of %d-byte pages",
			text, size, pageBytes)
	}
	return size, nil
}
