// Copyright 2023 The Fuchsia Authors
// Ported from zircon/kernel/vm/unittests/compression_unittest.cc
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// The four cases of compression_unittest.cc the plan ports, over the spill's
// storage and StoreAsIs in place of the slot storage and LZ4, and the D5
// cases beside them: a page that holds a reservation is compressed into it.
// Not ported: lz4_compress_smoke_test and lz4_zero_dedupe_test, since LZ4 is
// not ported, and slot_page_storage_size_rounding, since the spill stores
// whole pages and packs nothing.

// writePattern is write_pattern.
func writePattern(page *VmPage, length int, offset uint64) {
	for i := range length {
		page.data[i] = byte((uint64(i) + offset) % 256)
	}
}

// validatePattern is validate_pattern.
func validatePattern(data []byte, offset uint64) bool {
	for i, b := range data {
		if b != byte((uint64(i)+offset)%256) {
			return false
		}
	}
	return true
}

// newTestCompression is a compression over the case's spill with threshold.
func newTestCompression(env *vmoEnv, threshold uint64) *Compression {
	return NewCompression(env.pmm, env.ps, env.storage, StoreAsIs{}, threshold)
}

// allocTestPage is pmm_alloc_page, which the cases assert succeeds.
func allocTestPage(t *testing.T, env *vmoEnv) *VmPage {
	t.Helper()
	page, err := env.pmm.AllocPage()
	mustNotFail(t, "alloc a page", err)
	return page
}

const (
	metadataStart  = 0xdeadbeef
	metadataUpdate = 0xc0ffee
)

// The high-level compression and decompression flow keeps a page's bytes and
// its metadata. Zircon's compression_smoke_test.
func TestCompressionKeepsAPagesBytesAndMetadata(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		// StoreAsIs stores a page whole, so a threshold below a page fails
		// every page: the case's 70% is LZ4's.
		compression := newTestCompression(env, env.ps)
		// Compress a page full of content and get its reference.
		var compressedRef ReferenceValue
		{
			page := allocTestPage(t, env)
			writePattern(page, int(env.ps), 0)
			guard := compression.AcquireCompressor()
			compressor := guard.Get()
			mustNotFail(t, "arm", compressor.Arm())
			tempRef := compressor.Start(PageAndMetadata{Page: page, Metadata: metadataStart})
			expect(t, "the metadata started with", compression.GetMetadata(tempRef), uint32(metadataStart))
			compressor.Compress(env.ctx)
			// Changing the metadata before the result is taken is legal, and
			// the result carries the change.
			compression.SetMetadata(tempRef, metadataUpdate)
			expect(t, "the metadata changed", compression.GetMetadata(tempRef), uint32(metadataUpdate))
			result := compressor.TakeCompressionResult()
			expect(t, "a reference", result.Kind, CompressedToRef)
			compressedRef = result.Ref
			expect(t, "the temporary reference's metadata", compression.GetMetadata(tempRef), uint32(metadataUpdate))
			expect(t, "the reference's metadata", compression.GetMetadata(compressedRef), uint32(metadataUpdate))
			compressor.ReturnTempReference(tempRef)
			compressor.Finalize()
			guard.Release()
			env.pmm.FreePage(page)
		}
		// Decompress it and check the bytes and the metadata were kept.
		{
			page := allocTestPage(t, env)
			metadata, err := compression.Decompress(env.ctx, compressedRef, page.data)
			mustNotFail(t, "decompress", err)
			expect(t, "the bytes", validatePattern(page.data, 0), true)
			expect(t, "the metadata", metadata, uint32(metadataUpdate))
			env.pmm.FreePage(page)
		}
		expect(t, "the storage holds nothing", env.storage.GetMemoryUsage().UncompressedContentBytes, uint64(0))
	})
}

// A page of zeros is found and reported as zeros. Zircon's
// compression_zero_test.
func TestCompressionFindsAPageOfZeros(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		compression := newTestCompression(env, env.ps)
		page := allocTestPage(t, env)
		clear(page.data)
		guard := compression.AcquireCompressor()
		compressor := guard.Get()
		mustNotFail(t, "arm", compressor.Arm())
		tempRef := compressor.Start(PageAndMetadata{Page: page})
		compressor.Compress(env.ctx)
		expect(t, "zeros", compressor.TakeCompressionResult().Kind, CompressedToZero)
		compressor.ReturnTempReference(tempRef)
		compressor.Finalize()
		guard.Release()
		env.pmm.FreePage(page)
		expect(t, "nothing stored", env.storage.Available(), testSpillPages)
	})
}

// A page that cannot be compressed comes back with its metadata. Zircon's
// compression_fail_test.
func TestCompressionThatFailsGivesThePageBack(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		// A threshold of nothing fails every page.
		compression := newTestCompression(env, 0)
		page := allocTestPage(t, env)
		writePattern(page, int(env.ps), 0)
		guard := compression.AcquireCompressor()
		compressor := guard.Get()
		mustNotFail(t, "arm", compressor.Arm())
		tempRef := compressor.Start(PageAndMetadata{Page: page, Metadata: metadataStart})
		expect(t, "the metadata started with", compression.GetMetadata(tempRef), uint32(metadataStart))
		compressor.Compress(env.ctx)
		compression.SetMetadata(tempRef, metadataUpdate)
		expect(t, "the metadata changed", compression.GetMetadata(tempRef), uint32(metadataUpdate))
		result := compressor.TakeCompressionResult()
		expect(t, "a failure", result.Kind, CompressFailed)
		expect(t, "the page given back", result.Src.Page, page)
		expect(t, "with the changed metadata", result.Src.Metadata, uint32(metadataUpdate))
		compressor.ReturnTempReference(tempRef)
		compressor.Finalize()
		guard.Release()
		env.pmm.FreePage(page)
	})
}

// The temporary reference moved while the compression runs is a page with the
// metadata, and the compression's result is still there to free. Zircon's
// compression_move_reference_test.
func TestCompressionWhoseTemporaryReferenceMovesStillHasAResult(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		compression := newTestCompression(env, env.ps)
		page := allocTestPage(t, env)
		writePattern(page, int(env.ps), 0)
		guard := compression.AcquireCompressor()
		compressor := guard.Get()
		mustNotFail(t, "arm", compressor.Arm())
		tempRef := compressor.Start(PageAndMetadata{Page: page, Metadata: metadataStart})
		expect(t, "the metadata started with", compression.GetMetadata(tempRef), uint32(metadataStart))
		compressor.Compress(env.ctx)
		compression.SetMetadata(tempRef, metadataUpdate)
		expect(t, "the metadata changed", compression.GetMetadata(tempRef), uint32(metadataUpdate))
		// Moving the temporary reference before the result is taken ends it.
		moved, ok := compression.MoveReference(tempRef)
		expect(t, "moved", ok, true)
		expect(t, "the moved page's metadata", moved.Metadata, uint32(metadataUpdate))
		expect(t, "the moved page's bytes", validatePattern(moved.Page.data, 0), true)
		expect(t, "the temporary reference is back", compressor.IsTempReferenceInUse(), false)
		// The result can still be had, but its metadata is empty.
		result := compressor.TakeCompressionResult()
		expect(t, "a reference", result.Kind, CompressedToRef)
		expect(t, "no metadata", compression.GetMetadata(result.Ref), uint32(0))
		compressor.Free(result.Ref)
		compressor.Finalize()
		guard.Release()
		env.pmm.FreePage(moved.Page)
		env.pmm.FreePage(page)
		expect(t, "the result was freed", env.storage.Available(), testSpillPages)
	})
}

// D5: a page that holds a reservation is compressed into it, and is
// decompressed back holding it: the spill takes no other room, and a page
// spilled again goes where it went before.
func TestAPageIsCompressedIntoItsReservation(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		compression := newTestCompression(env, env.ps)
		reservation, ok := compression.Reserve()
		expect(t, "reserved", ok, true)
		page := allocTestPage(t, env)
		page.setReservation(reservation)
		writePattern(page, int(env.ps), 3)
		guard := compression.AcquireCompressor()
		compressor := guard.Get()
		mustNotFail(t, "arm", compressor.Arm())
		tempRef := compressor.Start(PageAndMetadata{Page: page, Metadata: metadataStart})
		compressor.Compress(env.ctx)
		result := compressor.TakeCompressionResult()
		expect(t, "a reference", result.Kind, CompressedToRef)
		expect(t, "the page's own reservation", result.Ref, reservation)
		expect(t, "with the metadata", compression.GetMetadata(result.Ref), uint32(metadataStart))
		compressor.ReturnTempReference(tempRef)
		compressor.Finalize()
		guard.Release()
		expect(t, "nothing else reserved", env.storage.Available(), testSpillPages-1)
		page.takeReservation()
		env.pmm.FreePage(page)

		back := allocTestPage(t, env)
		metadata, kept, err := compression.DecompressReserved(env.ctx, result.Ref, back.data)
		mustNotFail(t, "decompress", err)
		expect(t, "the bytes", validatePattern(back.data, 3), true)
		expect(t, "the metadata", metadata, uint32(metadataStart))
		expect(t, "the reservation kept", kept, reservation)
		expect(t, "still reserved", env.storage.Available(), testSpillPages-1)
		expect(t, "holding no bytes", env.storage.Holds(reservation), false)
		compression.Free(kept)
		env.pmm.FreePage(back)
		expect(t, "given back", env.storage.Available(), testSpillPages)
	})
}

// D5: a temporary reference moved while the page holding a reservation is
// compressed takes the reservation with the bytes, and the result the
// compression wrote into it is dropped, not freed, since the moved page holds
// it now.
func TestAMovedTemporaryReferenceTakesTheReservation(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		compression := newTestCompression(env, env.ps)
		reservation, _ := compression.Reserve()
		page := allocTestPage(t, env)
		page.setReservation(reservation)
		writePattern(page, int(env.ps), 5)
		guard := compression.AcquireCompressor()
		compressor := guard.Get()
		mustNotFail(t, "arm", compressor.Arm())
		tempRef := compressor.Start(PageAndMetadata{Page: page})
		compressor.Compress(env.ctx)
		moved, _ := compression.MoveReference(tempRef)
		got, reserved := moved.Page.DebugReservation()
		expect(t, "the moved page holds a reservation", reserved, true)
		expect(t, "the page's", got, reservation)
		_, reserved = page.DebugReservation()
		expect(t, "the compressed page holds none", reserved, false)
		result := compressor.TakeCompressionResult()
		expect(t, "into the reservation", result.Ref, reservation)
		compressor.Free(result.Ref)
		compressor.Finalize()
		guard.Release()
		expect(t, "still reserved", env.storage.Available(), testSpillPages-1)
		expect(t, "holding no bytes", env.storage.Holds(reservation), false)
		env.node.FreePage(moved.Page)
		env.pmm.FreePage(page)
		expect(t, "given back with the page", env.storage.Available(), testSpillPages)
	})
}

// D5: a storage that cannot read its bytes back fails the decompression, and
// the reference keeps them.
func TestADecompressionThatCannotReadKeepsTheReference(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		compression := newTestCompression(env, env.ps)
		page := allocTestPage(t, env)
		writePattern(page, int(env.ps), 7)
		ref, err := env.storage.Store(env.ctx, page.data)
		mustNotFail(t, "store", err)
		env.disk.FailNext(sim.DiskRead, 1)
		_, err = compression.Decompress(env.ctx, ref, page.data)
		expect(t, "the read failed", errors.Is(err, platform.ErrInjectedFault), true)
		expect(t, "the bytes are kept", env.storage.Holds(ref), true)
		clear(page.data)
		_, err = compression.Decompress(env.ctx, ref, page.data)
		mustNotFail(t, "decompress", err)
		expect(t, "the bytes", validatePattern(page.data, 7), true)
		env.pmm.FreePage(page)
		expect(t, "given back", env.storage.Available(), testSpillPages)
	})
}
