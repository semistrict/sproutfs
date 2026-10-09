// Copyright 2025 The Fuchsia Authors
// Ported from zircon/kernel/vm/slot_page_storage.cc and vm/include/vm/slot_page_storage.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"sync"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// SpillStorage is the spill file as compressed storage: the storage a spilled
// page's reference names (plans/zircon-pager-port-2026-10-05.md, "Spill and
// give-back"). It takes the shape of Zircon's VmSlotPageStorage: a reference
// is the id of an allocation record shifted past the alignment bits, and the
// record says where the bytes are, how long they are, and the metadata kept
// for the user. Zircon's ids come from a slab allocator.
//
// What it does not take: Zircon divides a page into 64 slots and packs
// compressed pages into them, with a list of pages per longest free run.
// Pages are stored as they are here, so an allocation is one whole page of
// the file, at its id times the page size, and there is nothing to pack.
//
// What stays the pager's own (the plan's "stays ours", and D5):
//
//   - The file is truncated and its whole extent allocated when the storage
//     is made. A spilled dirty page is the only copy of what the guest wrote,
//     so a spill must never fail for want of disk, and the space a sparse
//     file has not used yet is free space another writer on the node can
//     take.
//   - A reservation is an allocation handed out before there are bytes to
//     put in it (D5). Allocations are handed out lowest first, and one given
//     back is the next handed out, so state is kept only for what has been
//     handed out: a budget of a trillion pages costs nothing until it is used.
//   - The bytes are checked against a CRC32C when they are read back. The
//     file is scratch on a local device, so the authority for what it holds
//     is here, not in the file: a page that comes back short, zeroed or
//     holding bytes nobody wrote is a page the device lost, and it is
//     refused, not handed to a guest as memory it never wrote.
//   - A published version (plans/local-writeback-2026-10-09.md, step 1). An
//     allocation whose bytes a checkpoint published can be kept under the
//     key it was published as, K, instead of given back: it is then no
//     reservation, a load reads it by its key, and whenever a reservation
//     wants an allocation and none is free, the oldest version not read
//     since it was last passed over is dropped and its allocation handed
//     out. So versions take only what reservations leave, and a full budget
//     of reservations is never refused for them.
//
// An allocation given back keeps its blocks. Punching them would give them
// back to the filesystem, where another writer can take them, and the next
// page stored there would have nowhere to go. The stale bytes are never
// read: an allocation holds no bytes until it is written again.
type SpillStorage[K comparable] struct {
	file     platform.File
	pageSize uint64
	budget   int
	// maxVersions bounds the published versions kept, which is the memory
	// their index costs.
	maxVersions int

	mu sync.Mutex
	// next is how many ids have been handed out, ever; free is those given
	// back since, to be handed out again last-given-back first.
	next int
	free []int
	// allocations is the record of every id handed out so far.
	allocations []spillAllocation
	// versions is each published version by its key, and queue the versions
	// in the order they were published, oldest at head, which is the order
	// they are dropped in. An entry its key no longer names was dropped
	// already and is passed over. published counts the versions ever kept,
	// which numbers each one.
	versions  map[K]spillVersion[K]
	queue     []spillVersion[K]
	head      int
	published uint64
	// stored and storedBytes are Zircon's stored_items_ and
	// total_compressed_item_size_: the allocations that hold bytes, and how
	// many.
	stored      int
	storedBytes uint64
}

// spillVersion is one published version: its key, its allocation, and its
// number, which tells it from a later version of the same key in the same
// allocation.
type spillVersion[K comparable] struct {
	key    K
	id     int
	number uint64
}

// spillAllocation is one allocation, Zircon's Allocation.
type spillAllocation struct {
	// sum is the CRC32C of the bytes, which they must come back with.
	sum uint32
	// length is how many bytes it holds, a page or fewer.
	length uint32
	// metadata is the user's, kept for it.
	metadata uint32
	// written says the allocation holds bytes to be read back.
	written bool
	// version says the allocation is a published version, and read that it
	// was read since the queue last passed over it.
	version, read bool
}

// spillChecksums is what every stored page is checked against when it is read
// back.
var spillChecksums = crc32.MakeTable(crc32.Castagnoli)

// MaxSpillPages is the most allocations a SpillStorage can hand out: every id
// a reference holds once shifted past the alignment bits, but the one the
// temporary reference is. Zircon's slot storage bounds its ids the same way.
const MaxSpillPages = 1<<(32-ReferenceAlignBits) - 1

// The guards that put back defects the spill has had, for the tests that must
// catch them (scripts/mutation/guards.json).
const (
	// bugSpillSparse leaves the file sparse instead of allocating its extent.
	bugSpillSparse = "spill-sparse"
	// bugForgetSpill writes a page and does not record that the allocation
	// holds it.
	bugForgetSpill = "pager-forget-spill"
)

// NewSpillStorage makes file the storage of budget pages of pageSize bytes,
// which keeps at most maxVersions published versions: whatever the file held
// is dropped, since the spill is scratch, and its whole extent is allocated.
// The file must be a platform.AllocatingFile. A budget beyond MaxSpillPages,
// or below one, is ErrOutOfRange.
func NewSpillStorage[K comparable](ctx context.Context, file platform.File, pageSize uint64, budget, maxVersions int) (*SpillStorage[K], error) {
	if err := checkSpillBudget(budget); err != nil {
		return nil, err
	}
	if err := file.Truncate(ctx, 0); err != nil {
		return nil, err
	}
	size := int64(budget) * int64(pageSize)
	if sim.Bug(ctx, bugSpillSparse) {
		if err := file.Truncate(ctx, size); err != nil {
			return nil, err
		}
	} else {
		allocating, ok := file.(platform.AllocatingFile)
		if !ok {
			return nil, fmt.Errorf("%w: a spill file must allocate its extent", ErrNotSupported)
		}
		if err := allocating.Allocate(ctx, 0, size); err != nil {
			return nil, fmt.Errorf("allocating the spill file's %d bytes: %w", size, err)
		}
	}
	return &SpillStorage[K]{file: file, pageSize: pageSize, budget: budget,
		maxVersions: max(maxVersions, 0), versions: make(map[K]spillVersion[K])}, nil
}

// checkSpillBudget refuses a budget no reference can name, and none at all.
func checkSpillBudget(budget int) error {
	if budget < 1 || budget > MaxSpillPages {
		return fmt.Errorf("%w: a spill of %d pages, not 1 to %d", ErrOutOfRange, budget, MaxSpillPages)
	}
	return nil
}

// refToID is RefToAllocLocked's id: the allocation a reference names.
func (s *SpillStorage[K]) refToID(ref ReferenceValue) int {
	id := int(ref.Value() >> ReferenceAlignBits)
	assert(id < s.next, "the reference was handed out")
	return id
}

// idToRef is AllocToRefLocked: the reference of an allocation.
func idToRef(id int) ReferenceValue {
	return MakeReferenceValue(uint32(id) << ReferenceAlignBits)
}

// Available is how many more allocations may be handed out, a published
// version's among them.
func (s *SpillStorage[K]) Available() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.free) + s.budget - s.next + len(s.versions)
}

// Reserve hands out an allocation that holds nothing, or reports false where
// the budget has none left (D5). With none free, it drops the oldest
// published version for its allocation.
func (s *SpillStorage[K]) Reserve() (ReferenceValue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.free); n > 0 {
		id := s.free[n-1]
		s.free = s.free[:n-1]
		return idToRef(id), true
	}
	if s.next < s.budget {
		id := s.next
		s.next++
		s.allocations = append(s.allocations, spillAllocation{})
		return idToRef(id), true
	}
	if id, ok := s.dropOldestLocked(); ok {
		return idToRef(id), true
	}
	return ReferenceValue{}, false
}

// Publish keeps the bytes ref holds as the published version key names,
// instead of giving ref back. The allocation is a reservation no more, and
// ref names nothing from here. Where ref holds no bytes, key has a version
// already, or no version may be kept, it is given back as Free gives it, and
// Publish reports false.
func (s *SpillStorage[K]) Publish(ref ReferenceValue, key K) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.refToID(ref)
	allocation := &s.allocations[id]
	_, known := s.versions[key]
	if !allocation.written || known || s.maxVersions == 0 {
		s.freeLocked(id)
		return false
	}
	if len(s.versions) == s.maxVersions {
		dropped, ok := s.dropOldestLocked()
		assert(ok, "a full index has a version to drop")
		s.free = append(s.free, dropped)
	}
	allocation.version, allocation.read = true, false
	s.published++
	version := spillVersion[K]{key: key, id: id, number: s.published}
	s.versions[key] = version
	s.queue = append(s.queue, version)
	return true
}

// ReadVersion reads the published version key names into dst, a page, and
// reports whether there is one. The read holds no lock, so a version dropped
// while it is read, whose allocation may be another page's by the time the
// read ends, is a miss: the version must still be the same one once the read
// is over, and nothing hands its allocation out before it is dropped. A
// version whose bytes do not match their checksum is dropped, and the error is
// ErrIODataIntegrity: the device lost it, and the store still has it.
func (s *SpillStorage[K]) ReadVersion(ctx context.Context, key K, dst []byte) (bool, error) {
	s.mu.Lock()
	version, ok := s.versions[key]
	if !ok {
		s.mu.Unlock()
		return false, nil
	}
	allocation := &s.allocations[version.id]
	allocation.read = true
	length, sum := allocation.length, allocation.sum
	s.mu.Unlock()
	dst = dst[:length]
	n, err := s.file.ReadAt(ctx, dst, int64(version.id)*int64(s.pageSize))
	if err == nil && n != len(dst) {
		err = io.ErrUnexpectedEOF
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.versions[key]; !ok || current.number != version.number {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if crc32.Checksum(dst, spillChecksums) != sum {
		delete(s.versions, key)
		s.freeLocked(version.id)
		return false, fmt.Errorf("%w: the spill's version of a page does not match its checksum", ErrIODataIntegrity)
	}
	return true, nil
}

// HasVersion reports whether the storage keeps a published version of key.
func (s *SpillStorage[K]) HasVersion(key K) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.versions[key]
	return ok
}

// WriteVersion keeps data, a page, as the published version of key, in an
// allocation no reservation holds: a free one, one never handed out, or the
// oldest version's. It reports false, and writes nothing, where key has a
// version already, none may be kept, or every allocation is a reservation or
// another write's. The allocation is the write's alone until the write ends,
// so a caller that bounds its writes in flight and admits reservations
// against a budget that many allocations short of the storage's never finds
// a reservation refused for one.
func (s *SpillStorage[K]) WriteVersion(ctx context.Context, key K, data []byte) (bool, error) {
	assert(len(data) > 0 && uint64(len(data)) <= s.pageSize, "a version is a page")
	s.mu.Lock()
	if _, known := s.versions[key]; known || s.maxVersions == 0 {
		s.mu.Unlock()
		return false, nil
	}
	id, ok := -1, false
	switch {
	case len(s.versions) >= s.maxVersions:
		id, ok = s.dropOldestLocked()
	case len(s.free) > 0:
		id, ok = s.free[len(s.free)-1], true
		s.free = s.free[:len(s.free)-1]
	case s.next < s.budget:
		id, ok = s.next, true
		s.next++
		s.allocations = append(s.allocations, spillAllocation{})
	default:
		id, ok = s.dropOldestLocked()
	}
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	n, err := s.file.WriteAt(ctx, data, int64(id)*int64(s.pageSize))
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.freeLocked(id)
		return false, err
	}
	if _, known := s.versions[key]; known {
		// Another write kept the same version first.
		s.freeLocked(id)
		return false, nil
	}
	allocation := &s.allocations[id]
	*allocation = spillAllocation{sum: crc32.Checksum(data, spillChecksums), length: uint32(len(data)),
		written: true, version: true}
	s.stored++
	s.storedBytes += uint64(len(data))
	s.published++
	version := spillVersion[K]{key: key, id: id, number: s.published}
	s.versions[key] = version
	s.queue = append(s.queue, version)
	return true, nil
}

// Versions is how many published versions the storage keeps.
func (s *SpillStorage[K]) Versions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.versions)
}

// dropOldestLocked drops the oldest published version not read since the
// queue last passed over it, and returns its allocation, which holds nothing
// and is handed out by the caller. A version read since goes to the back,
// unread: a second chance, as a clock gives one. It reports false where no
// version is kept. Caller holds s.mu.
func (s *SpillStorage[K]) dropOldestLocked() (int, bool) {
	for len(s.versions) > 0 {
		entry := s.queue[s.head]
		s.queue[s.head] = spillVersion[K]{}
		s.head++
		if s.head*2 >= len(s.queue) {
			s.queue = append(s.queue[:0], s.queue[s.head:]...)
			s.head = 0
		}
		if current, ok := s.versions[entry.key]; !ok || current.number != entry.number {
			continue
		}
		allocation := &s.allocations[entry.id]
		if allocation.read {
			allocation.read = false
			s.queue = append(s.queue, entry)
			continue
		}
		delete(s.versions, entry.key)
		s.forgetLocked(allocation)
		*allocation = spillAllocation{}
		return entry.id, true
	}
	return 0, false
}

// Store keeps data in a new allocation, as Zircon's Store does: it fails with
// ErrNoSpace where the budget has none left.
func (s *SpillStorage[K]) Store(ctx context.Context, data []byte) (ReferenceValue, error) {
	ref, ok := s.Reserve()
	if !ok {
		return ReferenceValue{}, ErrNoSpace
	}
	if _, err := s.StoreReserved(ctx, []ReferenceValue{ref}, data); err != nil {
		s.Free(ref)
		return ReferenceValue{}, err
	}
	return ref, nil
}

// StoreReserved writes the bytes of refs, which are in ascending order, one
// page apart in data; only the last may be shorter than a page. Each run of
// consecutive allocations is one write, and it returns how many writes it
// took. The allocations are recorded as holding their bytes first, as the
// writes need no lock (D5). Nothing here needs room: the extent is the file's
// already.
func (s *SpillStorage[K]) StoreReserved(ctx context.Context, refs []ReferenceValue, data []byte) (int, error) {
	ps := int(s.pageSize)
	assert(len(refs) > 0, "there is something to store")
	assert(len(data) > (len(refs)-1)*ps && len(data) <= len(refs)*ps, "the bytes are the references' pages")
	page := func(i int) []byte { return data[i*ps : min((i+1)*ps, len(data))] }
	ids := make([]int, len(refs))
	s.mu.Lock()
	for i, ref := range refs {
		ids[i] = s.refToID(ref)
		assert(i == 0 || ids[i] > ids[i-1], "the references ascend")
		bytes := page(i)
		allocation := &s.allocations[ids[i]]
		s.forgetLocked(allocation)
		// The guard writes the bytes and does not record that the allocation
		// holds them, so a read back finds one that holds none.
		written := !sim.Bug(ctx, bugForgetSpill)
		*allocation = spillAllocation{sum: crc32.Checksum(bytes, spillChecksums),
			length: uint32(len(bytes)), metadata: allocation.metadata, written: written}
		if written {
			s.stored++
			s.storedBytes += uint64(len(bytes))
		}
	}
	s.mu.Unlock()
	writes := 0
	for start := 0; start < len(ids); {
		end := start + 1
		for end < len(ids) && ids[end] == ids[end-1]+1 {
			end++
		}
		bytes := data[start*ps : min(end*ps, len(data))]
		n, err := s.file.WriteAt(ctx, bytes, int64(ids[start])*int64(ps))
		if err == nil && n != len(bytes) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return writes, err
		}
		writes++
		start = end
	}
	return writes, nil
}

// CompressedData reads the bytes ref holds into dst and returns their length
// and its metadata. Bytes that do not match the checksum they were written
// with are ErrIODataIntegrity: the device lost them. An allocation that holds
// none is ErrBadState.
func (s *SpillStorage[K]) CompressedData(ctx context.Context, ref ReferenceValue, dst []byte) (int, uint32, error) {
	s.mu.Lock()
	id := s.refToID(ref)
	allocation := s.allocations[id]
	s.mu.Unlock()
	if !allocation.written {
		return 0, 0, fmt.Errorf("%w: spill reference %#x holds no bytes", ErrBadState, ref.Value())
	}
	dst = dst[:allocation.length]
	n, err := s.file.ReadAt(ctx, dst, int64(id)*int64(s.pageSize))
	if err == nil && n != len(dst) {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return 0, 0, err
	}
	if crc32.Checksum(dst, spillChecksums) != allocation.sum {
		return 0, 0, fmt.Errorf("%w: spill reference %#x does not match its checksum", ErrIODataIntegrity, ref.Value())
	}
	return n, allocation.metadata, nil
}

// Holds reports whether ref holds bytes.
func (s *SpillStorage[K]) Holds(ref ReferenceValue) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allocations[s.refToID(ref)].written
}

// Unstore drops the bytes ref holds, and keeps ref handed out (D5).
func (s *SpillStorage[K]) Unstore(ref ReferenceValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	allocation := &s.allocations[s.refToID(ref)]
	s.forgetLocked(allocation)
	*allocation = spillAllocation{}
}

// Free gives ref back, holding nothing.
func (s *SpillStorage[K]) Free(ref ReferenceValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freeLocked(s.refToID(ref))
}

// freeLocked gives an allocation back, holding nothing. Caller holds s.mu.
func (s *SpillStorage[K]) freeLocked(id int) {
	s.forgetLocked(&s.allocations[id])
	s.allocations[id] = spillAllocation{}
	s.free = append(s.free, id)
}

// forgetLocked takes an allocation's bytes out of the counts.
func (s *SpillStorage[K]) forgetLocked(allocation *spillAllocation) {
	if allocation.written {
		s.stored--
		s.storedBytes -= uint64(allocation.length)
	}
}

// GetMetadata is the metadata kept with ref.
func (s *SpillStorage[K]) GetMetadata(ref ReferenceValue) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allocations[s.refToID(ref)].metadata
}

// SetMetadata sets the metadata kept with ref.
func (s *SpillStorage[K]) SetMetadata(ref ReferenceValue, metadata uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allocations[s.refToID(ref)].metadata = metadata
}

// GetMemoryUsage is what the storage holds: the pages stored, the file's
// extent, and the bytes stored.
func (s *SpillStorage[K]) GetMemoryUsage() StorageMemoryUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StorageMemoryUsage{
		UncompressedContentBytes:   uint64(s.stored) * s.pageSize,
		CompressedStorageBytes:     uint64(s.budget) * s.pageSize,
		CompressedStorageUsedBytes: s.storedBytes,
	}
}

// Release gives the file's space back once nothing will be stored again:
// it truncates the file, and every allocation holds nothing from then on, a
// published version's included.
func (s *SpillStorage[K]) Release(ctx context.Context) error {
	if err := s.file.Truncate(ctx, 0); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.allocations {
		s.forgetLocked(&s.allocations[id])
		s.allocations[id].written = false
	}
	for key, version := range s.versions {
		delete(s.versions, key)
		s.allocations[version.id] = spillAllocation{}
		s.free = append(s.free, version.id)
	}
	s.queue, s.head = nil, 0
	return nil
}
