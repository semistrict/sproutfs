// Copyright 2022 The Fuchsia Authors
// Ported from zircon/kernel/vm/compression.cc, vm/include/vm/compression.h, vm/compressor.cc and
// vm/include/vm/compressor.h at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import "sync"

// This is the part of Zircon's compression that VmCompressor and VmCowPages
// use, ported for step 9 of the plan before step 6 makes the spill file its
// storage (with D5). Left out: LZ4 and the slot storage (step 6 brings its
// shape), the statistics, and the timestamp Zircon stores after the
// compressed bytes to time how long a page stayed compressed.

// CompressedStorage keeps compressed pages, Zircon's VmCompressedStorage.
type CompressedStorage interface {
	// Store keeps data, the compressed bytes, and returns a reference to
	// them, or false if it could not. Zircon passes the buffer page and may
	// get a page back; here the storage copies the bytes it keeps.
	Store(data []byte) (ReferenceValue, bool)
	// Free gives back what a reference holds.
	Free(ref ReferenceValue)
	// CompressedData is the bytes a reference holds and its metadata.
	CompressedData(ref ReferenceValue) ([]byte, uint32)
	// GetMetadata and SetMetadata are the metadata kept with a reference.
	GetMetadata(ref ReferenceValue) uint32
	SetMetadata(ref ReferenceValue, metadata uint32)
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
// lends while it compresses.
const tempReferenceValue = ^uint32(0) &^ (1<<ReferenceAlignBits - 1)

// Compression compresses pages into a storage, Zircon's VmCompression.
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

// Compress compresses a page's bytes.
func (c *Compression) Compress(src []byte) CompressResult {
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
	result := c.strategy.Compress(src, c.bufferPage.data, c.compressionThreshold)
	switch result.Kind {
	case CompressFailed:
		return CompressResult{Kind: CompressFailed}
	case CompressedToZero:
		return CompressResult{Kind: CompressedToZero}
	}
	assert(result.Size > 0 && result.Size <= c.compressionThreshold, "the compressed size is within the threshold")
	ref, ok := c.storage.Store(c.bufferPage.data[:result.Size])
	if !ok {
		return CompressResult{Kind: CompressFailed}
	}
	// The storage never makes the temporary reference.
	assert(!c.IsTempReference(ref), "the storage did not make the temporary reference")
	return CompressResult{Kind: CompressedToRef, Ref: ref}
}

// Decompress decompresses a reference into dst and frees it, and returns its
// metadata.
func (c *Compression) Decompress(ref ReferenceValue, dst []byte) uint32 {
	if c.IsTempReference(ref) {
		return c.decompressTempReference(ref, dst)
	}
	src, metadata := c.storage.CompressedData(ref)
	c.strategy.Decompress(src, dst)
	// Now that it is decompressed, free the storage.
	c.storage.Free(ref)
	return metadata
}

// Free frees a reference without decompressing it.
func (c *Compression) Free(ref ReferenceValue) {
	if c.IsTempReference(ref) {
		c.freeTempReference(ref)
		return
	}
	c.storage.Free(ref)
}

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

// MoveReference turns the temporary reference back into a page, by copying
// it to the compressor's spare page. Any other reference is left as it is.
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
// so the compression cannot finish meanwhile.
func (c *Compression) moveTempReference(ref ReferenceValue) PageAndMetadata {
	assert(c.IsTempReference(ref), "the reference is the temporary reference")
	assert(c.instance.usingTempReference, "the temporary reference is in use")
	assert(c.instance.page != nil, "the compressor has a page")
	assert(c.instance.sparePage != nil, "the compressor has a spare page")
	metadata := c.Decompress(ref, c.instance.sparePage.data)
	ret := c.instance.sparePage
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
	copy(dst, c.instance.page.data)
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
	c.tempReferenceMetadata = src.Metadata
	c.state = compressorStarted
	return MakeReferenceValue(c.tempReference)
}

// Compress compresses the page, with no lock held.
func (c *Compressor) Compress() {
	assert(c.state == compressorStarted, "the compressor is started")
	c.result = c.compression.Compress(c.page.data)
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
	c.result = CompressResult{}
	c.hasResult = false
	c.state = compressorFinalized
}

// Free frees a reference Compress made that turned out not to be needed.
func (c *Compressor) Free(ref ReferenceValue) {
	assert(!c.IsTempReference(ref), "the temporary reference is not freed this way")
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

// IsIdle reports whether no compression is in progress.
func (c *Compressor) IsIdle() bool {
	return c.state == compressorReady || c.state == compressorFinalized
}

// IsTempReference reports whether ref is this compressor's temporary
// reference.
func (c *Compressor) IsTempReference(ref ReferenceValue) bool { return ref.Value() == c.tempReference }

// IsTempReferenceInUse reports whether the temporary reference is lent out.
func (c *Compressor) IsTempReferenceInUse() bool { return c.usingTempReference }
