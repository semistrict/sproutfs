// Copyright 2025 The Fuchsia Authors
// Ported from zircon/kernel/vm/slot_page_storage.cc and vm/include/vm/slot_page_storage.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
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
		s := &SpillStorage{pageSize: env.ps, budget: MaxSpillPages}
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
		_, err = NewSpillStorage(env.ctx, file, env.ps, MaxSpillPages+1)
		expect(t, "too many", errors.Is(err, ErrOutOfRange), true)
		_, err = NewSpillStorage(env.ctx, file, env.ps, 0)
		expect(t, "none", errors.Is(err, ErrOutOfRange), true)
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
