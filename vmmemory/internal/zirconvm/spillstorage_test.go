// Copyright 2025 The Fuchsia Authors
// Ported from zircon/kernel/vm/slot_page_storage.cc and vm/include/vm/slot_page_storage.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Cases of the port's own for the spill's storage: what the pager's
// reservations test held of its dirty budget (vmmemory's
// reservations_internal_test.go, which this replaces), and the file it keeps
// pages in.

// A dirty budget is a count, not an allocation: a storage admitting the most
// pages a reference can name holds state only for the allocations it has
// handed out, lowest first, and one given back is the next handed out.
func TestReservationsCostWhatIsTakenNotWhatIsAdmitted(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		// The bookkeeping alone: a file of this extent is not allocated.
		s := &SpillStorage[int]{pageSize: env.ps, budget: MaxSpillPages}
		for want := range 3 {
			ref, ok := s.Reserve()
			expect(t, "reserved", ok, true)
			expect(t, "the next lowest", ref, idToRef(want))
		}
		s.Free(idToRef(1))
		ref, ok := s.Reserve()
		expect(t, "reserved again", ok, true)
		expect(t, "the one given back", ref, idToRef(1))
		expect(t, "available", s.Available(), MaxSpillPages-3)
		expect(t, "state for three", len(s.allocations), 3)
	})
}

// What a reservation holds is recorded with the checksum it is read back
// with, and a reservation given back holds nothing.
func TestAReservationGivenBackHoldsNothing(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 4)
		refs := make([]ReferenceValue, 3)
		for i := range refs {
			refs[i], _ = s.Reserve()
		}
		_, err := s.StoreReserved(env.ctx, refs[1:2], pattern(env.ps, 'R'))
		mustNotFail(t, "store", err)
		expect(t, "holds its bytes", s.Holds(refs[1]), true)
		got := make([]byte, env.ps)
		n, _, err := s.CompressedData(env.ctx, refs[1], got)
		mustNotFail(t, "read", err)
		expect(t, "a page", n, int(env.ps))
		expect(t, "the bytes", string(got) == string(pattern(env.ps, 'R')), true)
		s.Free(refs[1])
		expect(t, "holds nothing once given back", s.Holds(refs[1]), false)
		ref, _ := s.Reserve()
		expect(t, "handed out again", ref, refs[1])
		_, _, err = s.CompressedData(env.ctx, ref, got)
		expect(t, "nothing to read", errors.Is(err, ErrBadState), true)
	})
}

// A budget runs out, and a store past it fails for want of space.
func TestReservationsRunOut(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 2)
		s.Reserve()
		s.Reserve()
		_, ok := s.Reserve()
		expect(t, "a third", ok, false)
		_, err := s.Store(env.ctx, pattern(env.ps, 'X'))
		expect(t, "no space", err, ErrNoSpace)
	})
}

// A budget no reference can name, or none at all, is refused.
func TestASpillBeyondWhatAReferenceNamesIsRefused(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		disk := env.runtime.NewDisk("refused", sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: 1 << 30}})
		file, err := disk.Open(env.ctx, "spill", platform.OpenOptions{Create: true})
		mustNotFail(t, "open", err)
		_, err = NewSpillStorage[int](env.ctx, file, env.ps, MaxSpillPages+1, 0)
		expect(t, "too many", errors.Is(err, ErrOutOfRange), true)
		_, err = NewSpillStorage[int](env.ctx, file, env.ps, 0, 0)
		expect(t, "none", errors.Is(err, ErrOutOfRange), true)
		// The bounds themselves are budgets: one page, and the most a
		// reference names, which is not allocated here.
		one, err := NewSpillStorage[int](env.ctx, file, env.ps, 1, 0)
		mustNotFail(t, "one page", err)
		expect(t, "one available", one.Available(), 1)
		mustNotFail(t, "the most", checkSpillBudget(MaxSpillPages))
	})
}

// Consecutive reservations are written in one write, and the bytes of each
// come back from its own place.
func TestConsecutiveReservationsAreOneWrite(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := int(env.ps)
		s, _ := env.newSpillStorage(t, 4)
		refs := make([]ReferenceValue, 4)
		for i := range refs {
			refs[i], _ = s.Reserve()
		}
		data := append(append(pattern(env.ps, 'a'), pattern(env.ps, 'b')...), pattern(env.ps, 'd')...)
		writes, err := s.StoreReserved(env.ctx, []ReferenceValue{refs[0], refs[1], refs[3]}, data)
		mustNotFail(t, "store", err)
		expect(t, "two runs, two writes", writes, 2)
		for i, want := range map[int]byte{0: 'a', 1: 'b', 3: 'd'} {
			got := make([]byte, ps)
			_, _, err := s.CompressedData(env.ctx, refs[i], got)
			mustNotFail(t, "read", err)
			expect(t, "the bytes", string(got) == string(pattern(env.ps, want)), true)
		}
		expect(t, "the third holds nothing", s.Holds(refs[2]), false)
		usage := s.GetMemoryUsage()
		expect(t, "three pages stored", usage, StorageMemoryUsage{
			UncompressedContentBytes:   3 * env.ps,
			CompressedStorageBytes:     4 * env.ps,
			CompressedStorageUsedBytes: 3 * env.ps,
		})
	})
}

// Bytes the device changed are refused, not handed back.
func TestBytesTheDeviceChangedAreRefused(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 2)
		ref, err := s.Store(env.ctx, pattern(env.ps, 'C'))
		mustNotFail(t, "store", err)
		_, err = s.file.WriteAt(env.ctx, []byte{'X'}, int64(ref.Value()>>ReferenceAlignBits)*int64(env.ps)+1)
		mustNotFail(t, "change a byte", err)
		_, _, err = s.CompressedData(env.ctx, ref, make([]byte, env.ps))
		expect(t, "refused", errors.Is(err, ErrIODataIntegrity), true)
	})
}

// The storage truncates and allocates its file when it is made, and gives
// the space back on release, after which nothing holds bytes.
func TestTheSpillHoldsItsExtentUntilReleased(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 3)
		allocated, err := s.file.(platform.FileAllocation).Allocated(env.ctx)
		mustNotFail(t, "allocated", err)
		expect(t, "the whole extent", allocated, 3*int64(env.ps))
		ref, err := s.Store(env.ctx, pattern(env.ps, 'Z'))
		mustNotFail(t, "store", err)
		mustNotFail(t, "release", s.Release(env.ctx))
		size, err := s.file.Size(env.ctx)
		mustNotFail(t, "size", err)
		expect(t, "truncated", size, int64(0))
		expect(t, "holds nothing", s.Holds(ref), false)
		expect(t, "nothing stored", s.GetMemoryUsage().UncompressedContentBytes, uint64(0))
	})
}

// storeVersion reserves an allocation, writes data to it and publishes it as
// key's version.
func storeVersion(t *testing.T, env *vmoEnv, s *SpillStorage[int], key int, data []byte) {
	t.Helper()
	ref, ok := s.Reserve()
	expect(t, "reserved", ok, true)
	_, err := s.StoreReserved(env.ctx, []ReferenceValue{ref}, data)
	mustNotFail(t, "store", err)
	expect(t, "published", s.Publish(ref, key), true)
}

// readVersion is what ReadVersion reads of key, and whether it found it.
func readVersion(t *testing.T, env *vmoEnv, s *SpillStorage[int], key int) (string, bool) {
	t.Helper()
	got := make([]byte, env.ps)
	found, err := s.ReadVersion(env.ctx, key, got)
	mustNotFail(t, "read the version", err)
	return string(got), found
}

// A reservation's bytes, published, are kept under their key and read back by
// it, and the allocation is no reservation any more. A reservation that holds
// no bytes, or a key that has a version already, gives the allocation back
// instead.
func TestAPublishedReservationIsKeptUnderItsKey(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 4)
		storeVersion(t, env, s, 7, pattern(env.ps, 'P'))
		got, found := readVersion(t, env, s, 7)
		expect(t, "found", found, true)
		expect(t, "the bytes", got == string(pattern(env.ps, 'P')), true)
		_, found = readVersion(t, env, s, 8)
		expect(t, "another key", found, false)
		expect(t, "one version", s.Versions(), 1)
		expect(t, "every allocation available", s.Available(), 4)
		empty, _ := s.Reserve()
		expect(t, "no bytes, not published", s.Publish(empty, 9), false)
		again, _ := s.Reserve()
		_, err := s.StoreReserved(env.ctx, []ReferenceValue{again}, pattern(env.ps, 'Q'))
		mustNotFail(t, "store", err)
		expect(t, "a second version of a key, not published", s.Publish(again, 7), false)
		got, _ = readVersion(t, env, s, 7)
		expect(t, "the first version stays", got == string(pattern(env.ps, 'P')), true)
		expect(t, "both given back", s.Available(), 4)
	})
}

// Versions take only what reservations leave. A reservation that finds no
// allocation free takes the oldest version's, and one read since the queue
// last passed over it goes to the back once: a whole budget of reservations
// is never refused for versions.
func TestAReservationTakesTheOldestUnreadVersionsAllocation(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 3)
		for key := range 3 {
			storeVersion(t, env, s, key, pattern(env.ps, byte('a'+key)))
		}
		readVersion(t, env, s, 0)
		_, ok := s.Reserve()
		expect(t, "reserved over versions", ok, true)
		_, found := readVersion(t, env, s, 1)
		expect(t, "the oldest unread version went", found, false)
		got, found := readVersion(t, env, s, 0)
		expect(t, "the oldest, read since, stays", found, true)
		expect(t, "with its bytes", got == string(pattern(env.ps, 'a')), true)
		_, ok = s.Reserve()
		expect(t, "a second", ok, true)
		_, ok = s.Reserve()
		expect(t, "a third", ok, true)
		expect(t, "no version left", s.Versions(), 0)
		_, ok = s.Reserve()
		expect(t, "the budget, all reservations", ok, false)
	})
}

// The index of versions is bounded: a version past the bound drops the oldest.
func TestVersionsPastTheirBoundDropTheOldest(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		disk := env.runtime.NewDisk("bounded", sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: 8 * int64(env.ps)}})
		file, err := disk.Open(env.ctx, "spill", platform.OpenOptions{Create: true})
		mustNotFail(t, "open", err)
		s, err := NewSpillStorage[int](env.ctx, file, env.ps, 4, 2)
		mustNotFail(t, "make", err)
		for key := range 3 {
			storeVersion(t, env, s, key, pattern(env.ps, byte('a'+key)))
		}
		expect(t, "two versions", s.Versions(), 2)
		_, found := readVersion(t, env, s, 0)
		expect(t, "the oldest went", found, false)
		_, found = readVersion(t, env, s, 2)
		expect(t, "the newest stays", found, true)
		expect(t, "the dropped allocation is free", s.Available(), 4)
		none, err := NewSpillStorage[int](env.ctx, file, env.ps, 4, 0)
		mustNotFail(t, "make", err)
		ref, _ := none.Reserve()
		_, err = none.StoreReserved(env.ctx, []ReferenceValue{ref}, pattern(env.ps, 'n'))
		mustNotFail(t, "store", err)
		expect(t, "a storage that keeps none publishes none", none.Publish(ref, 1), false)
	})
}

// A version the device changed is dropped and reported, so the page is read
// from the store instead.
func TestAVersionTheDeviceChangedIsDropped(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 2)
		storeVersion(t, env, s, 5, pattern(env.ps, 'V'))
		_, err := s.file.WriteAt(env.ctx, []byte{'X'}, 1)
		mustNotFail(t, "change a byte", err)
		found, err := s.ReadVersion(env.ctx, 5, make([]byte, env.ps))
		expect(t, "refused", errors.Is(err, ErrIODataIntegrity), true)
		expect(t, "not found", found, false)
		expect(t, "dropped", s.Versions(), 0)
		expect(t, "its allocation free", s.Available(), 2)
	})
}

// A version dropped while it is read is a miss, whatever the read brought
// back: its allocation may be another page's by then.
func TestAVersionDroppedWhileItIsReadIsAMiss(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 1)
		storeVersion(t, env, s, 3, pattern(env.ps, 'O'))
		s.file = &droppingFile{File: s.file, during: func() {
			ref, ok := s.Reserve()
			expect(t, "the version's allocation, reserved", ok, true)
			_, err := s.StoreReserved(env.ctx, []ReferenceValue{ref}, pattern(env.ps, 'N'))
			mustNotFail(t, "store over it", err)
		}}
		found, err := s.ReadVersion(env.ctx, 3, make([]byte, env.ps))
		mustNotFail(t, "read", err)
		expect(t, "a miss", found, false)
	})
}

// droppingFile runs during once, before its first read.
type droppingFile struct {
	platform.File
	during func()
}

func (f *droppingFile) ReadAt(ctx context.Context, b []byte, offset int64) (int, error) {
	if f.during != nil {
		during := f.during
		f.during = nil
		during()
	}
	return f.File.ReadAt(ctx, b, offset)
}

// Release drops every version with the file.
func TestReleaseDropsEveryVersion(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		s, _ := env.newSpillStorage(t, 2)
		storeVersion(t, env, s, 1, pattern(env.ps, 'R'))
		mustNotFail(t, "release", s.Release(env.ctx))
		_, found := readVersion(t, env, s, 1)
		expect(t, "gone", found, false)
		expect(t, "nothing stored", s.GetMemoryUsage().UncompressedContentBytes, uint64(0))
	})
}
