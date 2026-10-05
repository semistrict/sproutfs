// Copyright 2022 The Fuchsia Authors
// Ported from zircon/kernel/vm/compression.cc, vm/include/vm/compression.h, vm/compressor.cc and
// vm/include/vm/compressor.h at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"
	"sync"
	"sync/atomic"
)

// Compression is how a spilled page is kept: a reference in its slot of the
// page list, and its bytes in a storage (plans/zircon-pager-port-2026-10-05.md,
// "Spill and give-back"). The storage is the spill file (spillstorage.go), and
// the strategy stores a page as it is (StoreAsIs). LZ4 is not ported.
//
// D5 departs from Zircon here. Zircon allocates storage when it compresses,
// and a compression can fail for want of it. A page here that the guest may
// store into takes its slot of the storage before it is dirty: a reservation,
// which the page holds while it is Dirty or AwaitingClean (page.go). Its
// spill writes into that slot and its refault reads it back and keeps it, so
// a spill never needs space. An anonymous page, which no pager backs, is
// stored as Zircon stores it.
//
// Left out: the timestamp Zircon stores after the compressed bytes, which
// times how long a page stayed compressed. A page stored as it is fills its
// slot, so there is no room for it, and nothing here reads the buckets it
// feeds. Dump and CreateDefault, which print to the kernel log and read boot
// options, are not ported either.

// CompressedStorage keeps compressed pages, Zircon's VmCompressedStorage.
// Instances may be used concurrently. The storage is a file here, so what
// reads or writes it takes a context, and can fail.
type CompressedStorage interface {
	// Store keeps data, the compressed bytes, and returns a reference to
	// them. Zircon passes the buffer page and may get a page back; here the
	// storage copies the bytes it keeps. It fails where the storage has no
	// room, which is Zircon's nullopt, or where it cannot write.
	Store(ctx context.Context, data []byte) (ReferenceValue, error)
	// Free gives back what a reference holds, and the reference.
	Free(ref ReferenceValue)
	// CompressedData reads the bytes a reference holds into dst and returns
	// their length and the reference's metadata. Zircon returns a pointer to
	// the bytes, which cannot fail; reading a file can.
	CompressedData(ctx context.Context, ref ReferenceValue, dst []byte) (int, uint32, error)
	// GetMetadata and SetMetadata are the metadata kept with a reference.
	GetMetadata(ref ReferenceValue) uint32
	SetMetadata(ref ReferenceValue, metadata uint32)
	// GetMemoryUsage is what the storage holds.
	GetMemoryUsage() StorageMemoryUsage

	// D5: Reserve takes a reference that holds nothing yet, or reports
	// false where the storage has no room left.
	Reserve() (ReferenceValue, bool)
	// D5: StoreReserved writes the bytes of refs, which Reserve made, one
	// page apart in data; only the last may be shorter than a page. It needs
	// no room. It returns how many writes it took.
	StoreReserved(ctx context.Context, refs []ReferenceValue, data []byte) (int, error)
	// D5: Unstore drops the bytes a reference holds and keeps the
	// reference, for a page that holds it again.
	Unstore(ref ReferenceValue)
}

// StorageMemoryUsage is VmCompressedStorage::MemoryUsage.
type StorageMemoryUsage struct {
	// UncompressedContentBytes is the pages stored, in bytes.
	UncompressedContentBytes uint64
	// CompressedStorageBytes is what the storage takes to hold them.
	CompressedStorageBytes uint64
	// CompressedStorageUsedBytes is the bytes stored.
	CompressedStorageUsedBytes uint64
}

// CompressionStrategy compresses and decompresses a page, Zircon's
// VmCompressionStrategy.
type CompressionStrategy interface {
	// Compress compresses src into dst, which is limit bytes long, and
	// returns the compressed length, or StrategyZero if src is all zeros, or
	// StrategyFail if it does not fit.
	Compress(src, dst []byte, limit uint64) StrategyResult
	// Decompress decompresses src, a page compressed, into dst.
	Decompress(src, dst []byte)
}

// StoreAsIs is the strategy of the spill: a page is stored as it is, and a
// page of zeros is found, as Zircon's LZ4 strategy finds one. It takes the
// place of LZ4, which is not ported.
type StoreAsIs struct{}

// Compress copies src to dst, or reports zeros, or fails where src does not
// fit within limit.
func (StoreAsIs) Compress(src, dst []byte, limit uint64) StrategyResult {
	if isZero(src) {
		return StrategyResult{Kind: CompressedToZero}
	}
	if uint64(len(src)) > limit {
		return StrategyResult{Kind: CompressFailed}
	}
	return StrategyResult{Kind: CompressedToRef, Size: uint64(copy(dst, src))}
}

// Decompress copies src to dst.
func (StoreAsIs) Decompress(src, dst []byte) { copy(dst, src) }

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// StrategyResult is what a CompressionStrategy made of a page: a length, or
// one of Zircon's ZeroTag and FailTag.
type StrategyResult struct {
	Kind CompressKind
	Size uint64
}

// CompressKind is which of Zircon's variant a result holds.
type CompressKind uint8

const (
	// CompressedToRef is a compressed reference, or for a strategy a length.
	CompressedToRef CompressKind = iota
	// CompressedToZero is Zircon's ZeroTag: the page was zeros.
	CompressedToZero
	// CompressFailed is Zircon's FailTag: the page could not be compressed
	// or kept.
	CompressFailed
)

// PageAndMetadata is a page with the metadata that goes with it, Zircon's
// VmCompressor::PageAndMetadata.
type PageAndMetadata struct {
	Page     *VmPage
	Metadata uint32
}

// CompressResult is the result of a compression, Zircon's
// VmCompressor::CompressResult: a reference, ZeroTag, or FailTag with the
// page that was to be compressed.
type CompressResult struct {
	Kind CompressKind
	Ref  ReferenceValue
	Src  PageAndMetadata
}

// tempReferenceValue is kTempReferenceValue: the one reference a compressor
// lends while it compresses. Storage never makes it.
const tempReferenceValue = ^uint32(0) &^ (1<<ReferenceAlignBits - 1)

// CompressionStats is VmCompression::Stats, without the decompression age
// buckets: see the timestamp above.
type CompressionStats struct {
	MemoryUsage                   StorageMemoryUsage
	TotalPageCompressionAttempts  uint64
	FailedPageCompressionAttempts uint64
	TotalPageDecompressions       uint64
	CompressedPageEvictions       uint64
}

// Compression compresses pages into a storage, Zircon's VmCompression.
// Each one has its own references, which do not move to another.
type Compression struct {
	storage              CompressedStorage
	strategy             CompressionStrategy
	compressionThreshold uint64
	pmm                  Pmm
	pageSize             uint64

	// instanceLock is over instance, the one compressor.
	instanceLock sync.Mutex
	instance     Compressor

	// compressionLock is over bufferPage, which compressions write into.
	compressionLock sync.Mutex
	bufferPage      *VmPage

	// decompressBuffers lend the buffer a decompression reads the storage
	// into. Zircon decompresses from the storage's own memory; a file is
	// read first.
	decompressBuffers sync.Pool

	// The statistics. Zircon also times compressions by thread runtime,
	// which Go does not keep.
	compressionAttempts  atomic.Uint64
	compressionSuccess   atomic.Uint64
	compressionZeroPage  atomic.Uint64
	compressionFail      atomic.Uint64
	decompressions       atomic.Uint64
	decompressionSkipped atomic.Uint64
}

// NewCompression compresses pages of pageSize bytes from pmm into storage
// with strategy. A page that does not compress to threshold bytes or fewer is
// not kept.
func NewCompression(pmm Pmm, pageSize uint64, storage CompressedStorage, strategy CompressionStrategy,
	threshold uint64) *Compression {
	assert(storage != nil, "there is a storage")
	assert(strategy != nil, "there is a strategy")
	assert(threshold <= pageSize, "the threshold fits a page")
	c := &Compression{
		storage:              storage,
		strategy:             strategy,
		compressionThreshold: threshold,
		pmm:                  pmm,
		pageSize:             pageSize,
	}
	c.decompressBuffers.New = func() any {
		buffer := make([]byte, pageSize)
		return &buffer
	}
	// Only one compressor is supported, so only one temporary reference.
	c.instance = Compressor{compression: c, tempReference: tempReferenceValue, state: compressorFinalized}
	return c
}

// CompressorGuard holds the compressor, Zircon's
// VmCompression::CompressorGuard.
type CompressorGuard struct{ c *Compression }

// AcquireCompressor takes the one compressor.
func (c *Compression) AcquireCompressor() CompressorGuard {
	c.instanceLock.Lock()
	return CompressorGuard{c: c}
}

// Get is the compressor held.
func (g CompressorGuard) Get() *Compressor { return &g.c.instance }

// Release gives the compressor back, which must not be in the middle of a
// compression. It is the guard's destructor.
func (g CompressorGuard) Release() {
	assert(g.c.instance.IsIdle(), "the compressor is idle")
	g.c.instanceLock.Unlock()
}

// Compress compresses a page's bytes into new storage.
func (c *Compression) Compress(ctx context.Context, src []byte) CompressResult {
	return c.compress(ctx, src, nil)
}

// compressInto compresses a page's bytes into the reservation the page holds
// (D5), which needs no room.
func (c *Compression) compressInto(ctx context.Context, src []byte, reservation ReferenceValue) CompressResult {
	return c.compress(ctx, src, &reservation)
}

// compress is Compress, into reservation where there is one.
func (c *Compression) compress(ctx context.Context, src []byte, reservation *ReferenceValue) CompressResult {
	c.compressionLock.Lock()
	defer c.compressionLock.Unlock()
	// The buffer page is allocated once and kept.
	if c.bufferPage == nil {
		page, err := c.pmm.AllocPage()
		if err != nil {
			return CompressResult{Kind: CompressFailed}
		}
		c.bufferPage = page
	}
	c.compressionAttempts.Add(1)
	result := c.strategy.Compress(src, c.bufferPage.bytesHere(), c.compressionThreshold)
	switch result.Kind {
	case CompressFailed:
		c.compressionFail.Add(1)
		return CompressResult{Kind: CompressFailed}
	case CompressedToZero:
		c.compressionZeroPage.Add(1)
		return CompressResult{Kind: CompressedToZero}
	}
	assert(result.Size > 0 && result.Size <= c.compressionThreshold, "the compressed size is within the threshold")
	data := c.bufferPage.bytesHere()[:result.Size]
	var ref ReferenceValue
	if reservation != nil {
		ref = *reservation
		if _, err := c.storage.StoreReserved(ctx, []ReferenceValue{ref}, data); err != nil {
			c.compressionFail.Add(1)
			return CompressResult{Kind: CompressFailed}
		}
	} else {
		stored, err := c.storage.Store(ctx, data)
		if err != nil {
			c.compressionFail.Add(1)
			return CompressResult{Kind: CompressFailed}
		}
		ref = stored
	}
	// The storage never makes the temporary reference.
	assert(!c.IsTempReference(ref), "the storage did not make the temporary reference")
	c.compressionSuccess.Add(1)
	return CompressResult{Kind: CompressedToRef, Ref: ref}
}

// Decompress decompresses a reference into dst and frees it, and returns its
// metadata. Zircon's cannot fail; reading the storage can, and then the
// reference is left as it was.
func (c *Compression) Decompress(ctx context.Context, ref ReferenceValue, dst []byte) (uint32, error) {
	if c.IsTempReference(ref) {
		return c.decompressTempReference(ref, dst), nil
	}
	metadata, err := c.decompressFromStorage(ctx, ref, dst)
	if err != nil {
		return 0, err
	}
	// Now that it is decompressed, free the storage.
	c.storage.Free(ref)
	return metadata, nil
}

// DecompressReserved decompresses a reference into dst for a page that is to
// hold its reservation (D5): the reference itself, which keeps its place and
// holds no bytes from here, or for the temporary reference the reservation of
// the page being compressed, which goes with the bytes. It returns the
// metadata and the reservation.
func (c *Compression) DecompressReserved(ctx context.Context, ref ReferenceValue, dst []byte) (uint32, ReferenceValue, error) {
	if c.IsTempReference(ref) {
		reservation := c.instance.takeReservation()
		return c.decompressTempReference(ref, dst), reservation, nil
	}
	metadata, err := c.decompressFromStorage(ctx, ref, dst)
	if err != nil {
		return 0, ReferenceValue{}, err
	}
	c.storage.Unstore(ref)
	return metadata, ref, nil
}

// decompressFromStorage reads a reference's bytes and decompresses them into
// dst.
func (c *Compression) decompressFromStorage(ctx context.Context, ref ReferenceValue, dst []byte) (uint32, error) {
	buffer := c.decompressBuffers.Get().(*[]byte)
	defer c.decompressBuffers.Put(buffer)
	n, metadata, err := c.storage.CompressedData(ctx, ref, *buffer)
	if err != nil {
		return 0, err
	}
	c.decompressions.Add(1)
	c.strategy.Decompress((*buffer)[:n], dst)
	return metadata, nil
}

// Free frees a reference without decompressing it.
func (c *Compression) Free(ref ReferenceValue) {
	if c.IsTempReference(ref) {
		c.freeTempReference(ref)
		return
	}
	c.storage.Free(ref)
	c.decompressionSkipped.Add(1)
}

// Reserve takes a reservation for a page about to be dirty (D5), or reports
// false where the storage has no room left.
func (c *Compression) Reserve() (ReferenceValue, bool) { return c.storage.Reserve() }

// GetMetadata is a reference's metadata. The caller holds the lock of the
// object that holds the reference.
func (c *Compression) GetMetadata(ref ReferenceValue) uint32 {
	if c.IsTempReference(ref) {
		return c.instance.tempReferenceMetadata
	}
	return c.storage.GetMetadata(ref)
}

// SetMetadata sets a reference's metadata. The caller holds the lock of the
// object that holds the reference.
func (c *Compression) SetMetadata(ref ReferenceValue, metadata uint32) {
	if c.IsTempReference(ref) {
		c.instance.tempReferenceMetadata = metadata
		return
	}
	c.storage.SetMetadata(ref, metadata)
}

// GetStats is the compression's statistics.
func (c *Compression) GetStats() CompressionStats {
	return CompressionStats{
		MemoryUsage:                   c.storage.GetMemoryUsage(),
		TotalPageCompressionAttempts:  c.compressionAttempts.Load(),
		FailedPageCompressionAttempts: c.compressionFail.Load(),
		TotalPageDecompressions:       c.decompressions.Load(),
		CompressedPageEvictions:       c.decompressionSkipped.Load(),
	}
}

// MoveReference turns the temporary reference back into a page, by copying
// it to the compressor's spare page. Any other reference is left as it is.
// It must be called where a reference moves in or out of a page list.
func (c *Compression) MoveReference(ref ReferenceValue) (PageAndMetadata, bool) {
	if c.IsTempReference(ref) {
		return c.moveTempReference(ref), true
	}
	return PageAndMetadata{}, false
}

// IsTempReference reports whether ref is the compressor's temporary reference.
func (c *Compression) IsTempReference(ref ReferenceValue) bool {
	return ref.Value() == tempReferenceValue
}

// moveTempReference copies the page being compressed into the spare page.
// The caller holds the lock of the object holding the temporary reference,
// so the compression cannot finish meanwhile. The spare page takes the
// reservation of the page being compressed with its bytes (D5).
func (c *Compression) moveTempReference(ref ReferenceValue) PageAndMetadata {
	assert(c.IsTempReference(ref), "the reference is the temporary reference")
	assert(c.instance.usingTempReference, "the temporary reference is in use")
	assert(c.instance.page != nil, "the compressor has a page")
	assert(c.instance.sparePage != nil, "the compressor has a spare page")
	ret := c.instance.sparePage
	if c.instance.page.reserved {
		ret.setReservation(c.instance.takeReservation())
	}
	metadata := c.decompressTempReference(ref, ret.bytesHere())
	c.instance.sparePage = nil
	return PageAndMetadata{Page: ret, Metadata: metadata}
}

func (c *Compression) freeTempReference(ref ReferenceValue) {
	assert(c.IsTempReference(ref), "the reference is the temporary reference")
	c.instance.ReturnTempReference(ref)
}

func (c *Compression) decompressTempReference(ref ReferenceValue, dst []byte) uint32 {
	assert(c.IsTempReference(ref), "the reference is the temporary reference")
	assert(c.instance.usingTempReference, "the temporary reference is in use")
	assert(c.instance.page != nil, "the compressor has a page")
	copy(dst, c.instance.page.bytesHere())
	metadata := c.instance.tempReferenceMetadata
	c.freeTempReference(ref)
	return metadata
}

// compressorState is VmCompressor::State.
type compressorState uint8

const (
	// compressorReady is armed and ready to start.
	compressorReady compressorState = iota
	// compressorStarted has lent its temporary reference and holds a page.
	compressorStarted
	// compressorCompressed has a result for the holder of the object's lock.
	compressorCompressed
	// compressorFinalized has finished and must be armed again.
	compressorFinalized
)

// Compressor is one compression in progress, Zircon's VmCompressor. The
// object that owns a page lends it to the compressor and holds the
// compressor's temporary reference in its slot meanwhile, so the page can be
// compressed with the object's lock dropped. Anyone who finds the temporary
// reference can decompress it, which copies the page. Back under the lock the
// owner swaps the temporary reference for the result, if it is still there.
type Compressor struct {
	compression   *Compression
	tempReference uint32
	state         compressorState
	// usingTempReference is only for asserts.
	usingTempReference bool
	// page is the page being compressed, read-only while compressing.
	page *VmPage
	// reservation is the reservation the page held when it was started,
	// which the compression writes into (D5), and reserved whether it held
	// one. They are the compressor's own, so the compression reads them with
	// no lock while the owner may hand the page's reservation on.
	reservation ReferenceValue
	reserved    bool
	// sparePage is what the temporary reference is copied into, should
	// someone need it as a page while it is lent out.
	sparePage *VmPage
	// result is the result of the last compression.
	result    CompressResult
	hasResult bool
	// tempReferenceMetadata is the metadata of the page behind the temporary
	// reference, which the owner may change while it is lent out.
	tempReferenceMetadata uint32
}

// Arm makes the compressor ready to start, allocating its spare page.
func (c *Compressor) Arm() error {
	assert(c.IsIdle(), "the compressor is idle")
	if c.sparePage == nil {
		page, err := c.compression.pmm.AllocPage()
		if err != nil {
			return err
		}
		c.sparePage = page
	}
	c.state = compressorReady
	return nil
}

// Start takes the page to compress and returns the temporary reference to
// put in its slot. The caller may change the metadata from here until it
// takes the result.
func (c *Compressor) Start(src PageAndMetadata) ReferenceValue {
	assert(c.state == compressorReady, "the compressor is ready")
	assert(c.sparePage != nil, "the compressor is armed")
	c.usingTempReference = true
	c.page = src.Page
	c.reservation, c.reserved = src.Page.reservation, src.Page.reserved
	c.tempReferenceMetadata = src.Metadata
	c.state = compressorStarted
	return MakeReferenceValue(c.tempReference)
}

// Compress compresses the page, with no lock held: into the page's own
// reservation where it holds one (D5).
func (c *Compressor) Compress(ctx context.Context) {
	assert(c.state == compressorStarted, "the compressor is started")
	if c.reserved {
		c.result = c.compression.compressInto(ctx, c.page.bytesHere(), c.reservation)
	} else {
		c.result = c.compression.Compress(ctx, c.page.bytesHere())
	}
	c.hasResult = true
	c.state = compressorCompressed
}

// TakeCompressionResult takes the result of the compression, with the
// metadata the owner last set. Only the holder of the object's lock may.
func (c *Compressor) TakeCompressionResult() CompressResult {
	assert(c.state == compressorCompressed, "the compressor has compressed")
	assert(c.hasResult, "there is a result to take")
	ret := c.result
	c.result = CompressResult{}
	c.hasResult = false
	// Make the final result carry any change to the temporary reference's
	// metadata.
	if c.IsTempReferenceInUse() {
		switch ret.Kind {
		case CompressedToRef:
			c.compression.SetMetadata(ret.Ref, c.tempReferenceMetadata)
		case CompressFailed:
			ret.Src.Page = c.page
			ret.Src.Metadata = c.tempReferenceMetadata
		}
	}
	return ret
}

// Finalize ends the compression once the temporary reference is no longer in
// use, so the compressor can be armed again.
func (c *Compressor) Finalize() {
	assert(c.state == compressorCompressed, "the compressor has compressed")
	assert(!c.IsTempReferenceInUse(), "the temporary reference is back")
	assert(c.page != nil, "the compressor has a page")
	c.page = nil
	c.reservation, c.reserved = ReferenceValue{}, false
	c.result = CompressResult{}
	c.hasResult = false
	c.state = compressorFinalized
}

// Free frees a reference Compress made that turned out not to be needed.
// Where Compress wrote into the page's reservation (D5), only the bytes are
// dropped: the reservation stays with whoever holds the page now.
func (c *Compressor) Free(ref ReferenceValue) {
	assert(!c.IsTempReference(ref), "the temporary reference is not freed this way")
	if c.reserved && ref == c.reservation {
		c.compression.storage.Unstore(ref)
		return
	}
	c.compression.Free(ref)
}

// ReturnTempReference gives the temporary reference back. The page stays
// until Finalize, since Compress may still be reading it.
func (c *Compressor) ReturnTempReference(ref ReferenceValue) {
	assert(c.IsTempReference(ref), "the reference is the temporary reference")
	assert(c.usingTempReference, "the temporary reference is in use")
	assert(c.page != nil, "the compressor has a page")
	c.usingTempReference = false
	c.tempReferenceMetadata = 0
}

// takeReservation hands on the reservation the page being compressed holds,
// to the page its temporary reference becomes (D5). The caller holds the lock
// of the object holding the temporary reference.
func (c *Compressor) takeReservation() ReferenceValue {
	assert(c.page != nil, "the compressor has a page")
	assert(c.page.reserved, "the page being compressed holds a reservation")
	return c.page.takeReservation()
}

// IsIdle reports whether no compression is in progress.
func (c *Compressor) IsIdle() bool {
	return c.state == compressorReady || c.state == compressorFinalized
}

// IsTempReference reports whether ref is this compressor's temporary
// reference.
func (c *Compressor) IsTempReference(ref ReferenceValue) bool { return ref.Value() == c.tempReference }

// IsTempReferenceInUse reports whether the temporary reference is lent out.
func (c *Compressor) IsTempReferenceInUse() bool { return c.usingTempReference }
