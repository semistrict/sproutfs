// Copyright 2016 The Fuchsia Authors
// Copyright 2026 The Fuchsia Authors
// Copyright (c) 2014 Travis Geiselbrecht
// Ported from zircon/kernel/vm/include/vm/vm_page_list.h, vm/include/vm/vm_constants.h and
// vm/include/vm/page.h at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

// Package zirconvm is a Go port of the page layer of Zircon's VM, which is
// replacing the pager's own (plans/zircon-pager-port-2026-10-05.md). It copies
// Zircon's structure, functions and logic. It keeps Zircon's byte offsets, with
// the pager's page size, 4 KiB or 2 MiB, in place of Zircon's constant
// kPageSize. Where it departs from Zircon, a comment beside the line says why.
//
// Zircon's walks return zx_status_t from their callbacks: ZX_ERR_NEXT to go
// on, ZX_ERR_STOP to end the walk without failing it, and any other status to
// fail it. Here a callback returns nil to go on, ErrStop to stop, and any
// other error to fail the walk.
//
// Zircon checks its invariants with DEBUG_ASSERT and ASSERT. Both panic here,
// in every build.
package zirconvm

import (
	"errors"
	"fmt"
)

// ErrStop ends a walk early without failing it. It is Zircon's ZX_ERR_STOP.
var ErrStop = errors.New("zirconvm: stop")

// ErrNoMemory is Zircon's ZX_ERR_NO_MEMORY: a slot could not be had. A Go
// allocation does not fail, so here it means only an offset at or past the
// page list's MaxSize.
var ErrNoMemory = errors.New("zirconvm: no slot for the offset")

// assert is Zircon's DEBUG_ASSERT and ASSERT.
func assert(cond bool, what string) {
	if !cond {
		panic("zirconvm: assertion failed: " + what)
	}
}

// The page list constants, from vm_constants.h.
const (
	typeBits              = 3
	pageType              = 0b000
	zeroMarkerType        = 0b001
	referenceType         = 0b010
	intervalType          = 0b011
	parentContentType     = 0b100
	intervalSentinelBits  = 2
	intervalTypeBits      = 2
	intervalBits          = typeBits + intervalSentinelBits + intervalTypeBits
	pageFanOut            = 16
	markerShareCountShift = typeBits
	intervalSentinelShift = typeBits
	intervalTypeShift     = intervalSentinelShift + intervalSentinelBits
)

// bitMask is BIT_MASK32: the low n bits set.
func bitMask(n uint) uint32 { return 1<<n - 1 }

// PageOrMarker is the content of one slot of a page list, Zircon's
// VmPageOrMarker. It is in one of six states:
//   - Empty: it holds nothing.
//   - Page: it holds a page, which it owns until ReleasePage is called.
//   - Reference: it holds a reference to content elsewhere, which it owns until
//     ReleaseReference is called.
//   - Marker: it is not a page, but it is not empty either. A marker says the
//     page was deduplicated to zero, where Empty says the parent holds the
//     content.
//   - Interval: the slot is part of a sparse interval. An interval has a Start
//     sentinel and an End sentinel, and every offset between them is empty. An
//     interval of one page is a Slot sentinel, which is both.
//   - ParentContent: there may be content for the slot, but the parent's page
//     list must be checked for it. How it differs from Empty is up to the VMO.
//
// The page list tries to keep two invariants, as Zircon's does. A node is
// never wholly empty: it has at least one slot that is not Empty. And an
// interval spans as much as it can: no two intervals sit side by side that one
// interval could represent.
//
// The zero value is Empty. Slots are read by value, as Zircon reads them
// through a const pointer; a slot is changed through a *PageOrMarker that
// LookupOrAllocate or a removing walk hands out, or through a PageOrMarkerRef.
type PageOrMarker[P any] struct {
	// page is the page of a Page slot. Zircon packs a pmm index into raw_ in
	// place of a pointer; a Go page is held by pointer, and raw is then 0.
	page *P
	// raw is Zircon's raw_: the type in the low three bits, and what the type
	// carries in the rest.
	raw uint32
	// pageShift is the page size of the list an interval sentinel belongs to.
	// Zircon's AwaitingClean length is in bytes over its constant kPageShift;
	// the pager's page size is not a constant, so a sentinel carries it.
	pageShift uint8
}

// ReferenceValue is a reference to content held elsewhere, Zircon's
// VmPageOrMarker::ReferenceValue. Its low ReferenceAlignBits bits are zero, so
// a slot can keep its type there.
type ReferenceValue struct{ value uint32 }

// ReferenceAlignBits is the number of low bits of a reference that must be
// zero.
const ReferenceAlignBits = 3

// MakeReferenceValue makes a reference from its value, which must be aligned.
func MakeReferenceValue(raw uint32) ReferenceValue {
	assert(raw&bitMask(ReferenceAlignBits) == 0, "a reference is aligned")
	return ReferenceValue{value: raw}
}

// Value is the reference's value.
func (r ReferenceValue) Value() uint32 { return r.value }

// SentinelType is which end of a sparse interval a slot is.
type SentinelType uint32

const (
	// SentinelSlot is an interval of a single page.
	SentinelSlot SentinelType = iota
	// SentinelStart is the first page of an interval of several.
	SentinelStart
	// SentinelEnd is the last page of an interval of several.
	SentinelEnd
)

// IntervalType is what a sparse interval is of. Zircon supports one.
type IntervalType uint32

// IntervalZero is a range of zero pages.
const IntervalZero IntervalType = 0

// IntervalDirtyState is the dirty state of a zero interval, as
// VmCowPages::DirtyState is of a page. AwaitingClean is not a state here: an
// interval whose start carries a nonzero AwaitingClean length is
// AwaitingClean for that length, which splits and merges more easily.
type IntervalDirtyState uint32

const (
	IntervalUntracked IntervalDirtyState = iota
	IntervalClean
	IntervalDirty
	intervalNumStates
)

func (s IntervalDirtyState) String() string {
	switch s {
	case IntervalUntracked:
		return "Untracked"
	case IntervalClean:
		return "Clean"
	case IntervalDirty:
		return "Dirty"
	}
	return fmt.Sprintf("IntervalDirtyState(%d)", uint32(s))
}

// The bits of a zero interval above the interval bits, Zircon's ZeroRange.
const (
	// zeroRangeAlignBits is kIntervalBits.
	zeroRangeAlignBits = intervalBits
	// dirtyStateBits is VM_PAGE_OBJECT_DIRTY_STATE_BITS of page.h.
	dirtyStateBits           = 2
	dirtyStateShift          = zeroRangeAlignBits
	awaitingCleanLengthShift = zeroRangeAlignBits + dirtyStateBits
)

// Zircon's static_asserts on the layout. A constant conversion of a negative
// number to uint does not compile, so each line holds only while its
// condition does. That the AwaitingClean length's shift is no larger than the
// page shift is checked by NewPageList, since the page shift is not a
// constant here.
const (
	_ = uint(1<<dirtyStateBits - intervalNumStates)                           // every dirty state fits its bits
	_ = uint(awaitingCleanLengthShift - (dirtyStateShift + dirtyStateBits))   // the length is above the dirty state
	_ = uint(ReferenceAlignBits-typeBits) + uint(typeBits-ReferenceAlignBits) // a reference leaves the type bits free
	_ = uint(1<<intervalSentinelBits - (SentinelEnd + 1))                     // every sentinel fits its bits
	_ = uint(1<<intervalTypeBits - (IntervalZero + 1))                        // every interval type fits its bits
)

// zeroRange is the state a zero interval keeps in its sentinel: its dirty
// state, and the length of it that is AwaitingClean.
type zeroRange uint32

func makeZeroRange(val uint32, state IntervalDirtyState) zeroRange {
	z := zeroRange(val)
	assert(uint32(z)&bitMask(zeroRangeAlignBits) == 0, "a zero range is aligned")
	assert(z.dirtyState() == IntervalUntracked, "a new zero range has no dirty state")
	z.setDirtyState(state)
	return z
}

func (z zeroRange) dirtyState() IntervalDirtyState {
	return IntervalDirtyState((uint32(z) & (bitMask(dirtyStateBits) << dirtyStateShift)) >> dirtyStateShift)
}

func (z *zeroRange) setDirtyState(state IntervalDirtyState) {
	// Only dirty and untracked zero ranges are allowed for now.
	assert(state == IntervalDirty || state == IntervalUntracked, "a zero range is Dirty or Untracked")
	*z &^= zeroRange(bitMask(dirtyStateBits) << dirtyStateShift)
	*z |= zeroRange(uint32(state) << dirtyStateShift)
}

// setAwaitingCleanLength keeps a length that is a whole number of pages, so
// only the bits above the page shift are stored.
func (z *zeroRange) setAwaitingCleanLength(length uint64, pageShift uint8) {
	assert(length == 0 || z.dirtyState() == IntervalDirty, "only a Dirty interval is AwaitingClean")
	assert(length&(1<<pageShift-1) == 0, "an AwaitingClean length is page rounded")
	length = (length >> pageShift) << awaitingCleanLengthShift
	*z &= zeroRange(bitMask(awaitingCleanLengthShift))
	*z |= zeroRange(uint32(length))
}

func (z zeroRange) awaitingCleanLength(pageShift uint8) uint64 {
	length := uint64(uint32(z) &^ bitMask(awaitingCleanLengthShift))
	return (length >> awaitingCleanLengthShift) << pageShift
}

// Empty is a slot that holds nothing.
func Empty[P any]() PageOrMarker[P] { return PageOrMarker[P]{raw: pageType} }

// Marker is a zero marker.
func Marker[P any]() PageOrMarker[P] { return PageOrMarker[P]{raw: zeroMarkerType} }

// MarkerWithShareCount is a zero marker shared shareCount times.
func MarkerWithShareCount[P any](shareCount uint32) PageOrMarker[P] {
	return PageOrMarker[P]{raw: zeroMarkerType | shareCount<<markerShareCountShift}
}

// ParentContent says the parent's page list may hold the content.
func ParentContent[P any]() PageOrMarker[P] { return PageOrMarker[P]{raw: parentContentType} }

// Page is a slot holding p, which must not be nil: a nil page is Empty.
func Page[P any](p *P) PageOrMarker[P] {
	assert(p != nil, "a page is not nil")
	return PageOrMarker[P]{page: p, raw: pageType}
}

// Reference is a slot holding ref.
func Reference[P any](ref ReferenceValue) PageOrMarker[P] {
	return PageOrMarker[P]{raw: ref.value | referenceType}
}

// zeroInterval is a sentinel of a zero interval. Only the page list makes one,
// so a caller cannot make interval sentinels at will. pageShift is the list's.
func zeroInterval[P any](sentinel SentinelType, state IntervalDirtyState, pageShift uint8) PageOrMarker[P] {
	sentinelBits := uint32(sentinel) << intervalSentinelShift
	typeBits := uint32(IntervalZero) << intervalTypeShift
	return PageOrMarker[P]{
		raw:       uint32(makeZeroRange(0, state)) | typeBits | sentinelBits | intervalType,
		pageShift: pageShift,
	}
}

func (s PageOrMarker[P]) typ() uint32 { return s.raw & bitMask(typeBits) }

// IsPage reports whether the slot holds a page.
func (s PageOrMarker[P]) IsPage() bool { return !s.IsEmpty() && s.typ() == pageType }

// IsMarker reports whether the slot is a zero marker.
func (s PageOrMarker[P]) IsMarker() bool { return s.typ() == zeroMarkerType }

// IsEmpty reports whether the slot holds nothing.
func (s PageOrMarker[P]) IsEmpty() bool { return s.raw == pageType && s.page == nil }

// IsReference reports whether the slot holds a reference.
func (s PageOrMarker[P]) IsReference() bool { return s.typ() == referenceType }

// IsPageOrRef reports whether the slot holds a page or a reference, the two
// kinds of content it owns.
func (s PageOrMarker[P]) IsPageOrRef() bool { return s.IsPage() || s.IsReference() }

// IsInterval reports whether the slot is an interval sentinel.
func (s PageOrMarker[P]) IsInterval() bool { return s.typ() == intervalType }

// IsParentContent reports whether the slot defers to the parent.
func (s PageOrMarker[P]) IsParentContent() bool { return s.typ() == parentContentType }

// Page is the slot's page. The slot must hold one.
func (s PageOrMarker[P]) Page() *P {
	assert(s.IsPage(), "the slot holds a page")
	return s.page
}

// Reference is the slot's reference. The slot must hold one.
func (s PageOrMarker[P]) Reference() ReferenceValue {
	assert(s.IsReference(), "the slot holds a reference")
	return ReferenceValue{value: s.raw &^ bitMask(ReferenceAlignBits)}
}

// GetMarkerShareCount is how many times a marker is shared.
func (s PageOrMarker[P]) GetMarkerShareCount() uint32 {
	assert(s.IsMarker(), "the slot is a marker")
	return s.raw >> markerShareCountShift
}

func (s PageOrMarker[P]) intervalSentinel() SentinelType {
	return SentinelType((s.raw & (bitMask(intervalSentinelBits) << intervalSentinelShift)) >> intervalSentinelShift)
}

func (s PageOrMarker[P]) intervalType() IntervalType {
	return IntervalType((s.raw & (bitMask(intervalTypeBits) << intervalTypeShift)) >> intervalTypeShift)
}

// IsIntervalStart reports whether the slot starts an interval of several pages.
func (s PageOrMarker[P]) IsIntervalStart() bool {
	return s.IsInterval() && s.intervalSentinel() == SentinelStart
}

// IsIntervalEnd reports whether the slot ends an interval of several pages.
func (s PageOrMarker[P]) IsIntervalEnd() bool {
	return s.IsInterval() && s.intervalSentinel() == SentinelEnd
}

// IsIntervalSlot reports whether the slot is an interval of one page.
func (s PageOrMarker[P]) IsIntervalSlot() bool {
	return s.IsInterval() && s.intervalSentinel() == SentinelSlot
}

// IsIntervalZero reports whether the slot is a sentinel of a zero interval.
func (s PageOrMarker[P]) IsIntervalZero() bool {
	return s.IsInterval() && s.intervalType() == IntervalZero
}

func (s PageOrMarker[P]) zeroRange() zeroRange {
	return zeroRange(s.raw &^ bitMask(intervalBits))
}

// IsZeroIntervalClean reports whether a zero interval is Clean.
func (s PageOrMarker[P]) IsZeroIntervalClean() bool {
	assert(s.IsIntervalZero(), "the slot is a zero interval")
	return s.zeroRange().dirtyState() == IntervalClean
}

// IsZeroIntervalDirty reports whether a zero interval is Dirty.
func (s PageOrMarker[P]) IsZeroIntervalDirty() bool {
	assert(s.IsIntervalZero(), "the slot is a zero interval")
	return s.zeroRange().dirtyState() == IntervalDirty
}

// IsZeroIntervalUntracked reports whether a zero interval is Untracked.
func (s PageOrMarker[P]) IsZeroIntervalUntracked() bool {
	assert(s.IsIntervalZero(), "the slot is a zero interval")
	return s.zeroRange().dirtyState() == IntervalUntracked
}

// GetZeroIntervalDirtyState is a zero interval's dirty state.
func (s PageOrMarker[P]) GetZeroIntervalDirtyState() IntervalDirtyState {
	assert(s.IsIntervalZero(), "the slot is a zero interval")
	return s.zeroRange().dirtyState()
}

// GetZeroIntervalAwaitingCleanLength is how much of a zero interval, from its
// start, is AwaitingClean. The slot must be a start or a single slot.
func (s PageOrMarker[P]) GetZeroIntervalAwaitingCleanLength() uint64 {
	assert(s.IsIntervalZero(), "the slot is a zero interval")
	assert(s.IsIntervalStart() || s.IsIntervalSlot(), "the slot starts its interval")
	return s.zeroRange().awaitingCleanLength(s.pageShift)
}

// SetZeroIntervalAwaitingCleanLength sets how much of a zero interval, from
// its start, is AwaitingClean. The slot must be a start or a single slot.
func (s *PageOrMarker[P]) SetZeroIntervalAwaitingCleanLength(length uint64) {
	assert(s.IsIntervalZero(), "the slot is a zero interval")
	assert(s.IsIntervalStart() || s.IsIntervalSlot(), "the slot starts its interval")
	z := s.zeroRange()
	z.setAwaitingCleanLength(length, s.pageShift)
	s.raw = s.raw&bitMask(intervalBits) | uint32(z)
}

// setIntervalSentinel replaces the sentinel type and keeps the rest.
func (s *PageOrMarker[P]) setIntervalSentinel(sentinel SentinelType) {
	s.raw &^= bitMask(intervalSentinelBits) << intervalSentinelShift
	s.raw |= uint32(sentinel) << intervalSentinelShift
}

// changeIntervalSentinel changes which end of an interval the slot is, keeping
// the rest of its state. Only a Slot may become a Start or an End, and only a
// Start or an End may become a Slot: those are the changes extending or
// clipping an interval makes.
func (s *PageOrMarker[P]) changeIntervalSentinel(sentinel SentinelType) {
	assert(s.IsInterval(), "the slot is an interval")
	old := s.intervalSentinel()
	assert(old != sentinel, "the sentinel changes")
	if old == SentinelStart || old == SentinelEnd {
		assert(sentinel == SentinelSlot, "a Start or an End becomes a Slot")
	} else {
		assert(old == SentinelSlot, "the old sentinel is a Slot")
		assert(sentinel == SentinelStart || sentinel == SentinelEnd, "a Slot becomes a Start or an End")
	}
	s.setIntervalSentinel(sentinel)
}

// release empties the slot and returns what it held.
func (s *PageOrMarker[P]) release() PageOrMarker[P] {
	old := *s
	*s = PageOrMarker[P]{}
	return old
}

// Take moves the content out of the slot and leaves it Empty. It is Zircon's
// move out of a VmPageOrMarker.
func (s *PageOrMarker[P]) Take() PageOrMarker[P] { return s.release() }

// Set moves content into the slot, Zircon's move assignment. The slot must not
// hold a page or a reference, which would be lost.
func (s *PageOrMarker[P]) Set(content PageOrMarker[P]) {
	assert(!s.IsPageOrRef(), "the slot holds no page or reference to lose")
	*s = content
}

// ReleasePage moves the page out of the slot and leaves it Empty.
func (s *PageOrMarker[P]) ReleasePage() *P {
	assert(s.IsPage(), "the slot holds a page")
	return s.release().page
}

// ReleaseReference moves the reference out of the slot and leaves it Empty.
func (s *PageOrMarker[P]) ReleaseReference() ReferenceValue {
	assert(s.IsReference(), "the slot holds a reference")
	return ReferenceValue{value: s.release().raw &^ bitMask(ReferenceAlignBits)}
}

// SwapReferenceForPage replaces the slot's reference with p and returns the
// reference.
func (s *PageOrMarker[P]) SwapReferenceForPage(p *P) ReferenceValue {
	assert(p != nil, "a page is not nil")
	ref := s.ReleaseReference()
	s.Set(Page(p))
	return ref
}

// SwapPageForReference replaces the slot's page with ref and returns the page.
func (s *PageOrMarker[P]) SwapPageForReference(ref ReferenceValue) *P {
	page := s.ReleasePage()
	s.Set(Reference[P](ref))
	return page
}

// SwapReferenceForReference replaces the slot's reference with ref and
// returns the old one.
func (s *PageOrMarker[P]) SwapReferenceForReference(ref ReferenceValue) ReferenceValue {
	old := s.ReleaseReference()
	s.Set(Reference[P](ref))
	return old
}

// SetMarkerShareCount sets how many times a marker is shared.
func (s *PageOrMarker[P]) SetMarkerShareCount(shareCount uint32) {
	assert(s.IsMarker(), "the slot is a marker")
	s.raw = zeroMarkerType | shareCount<<markerShareCountShift
}

// IncrementMarkerShareCount shares a marker once more.
func (s *PageOrMarker[P]) IncrementMarkerShareCount() {
	assert(s.IsMarker(), "the slot is a marker")
	s.raw += 1 << markerShareCountShift
}

// DecrementMarkerShareCount shares a marker once less. Its count must not be
// zero.
func (s *PageOrMarker[P]) DecrementMarkerShareCount() {
	assert(s.IsMarker(), "the slot is a marker")
	assert(s.GetMarkerShareCount() > 0, "a marker's share count is above zero")
	s.raw -= 1 << markerShareCountShift
}

// Swap puts other in the slot and returns what the slot held.
func (s *PageOrMarker[P]) Swap(other PageOrMarker[P]) PageOrMarker[P] {
	old := *s
	*s = other
	return old
}

// PageOrMarkerRef is a slot that may be changed only in limited ways: from one
// kind of content to another, and in its dirty state and share count. It is
// Zircon's VmPageOrMarkerRef. Most walks that are not meant to empty slots
// hand one out.
type PageOrMarkerRef[P any] struct {
	slot *PageOrMarker[P]
}

// Valid reports whether the reference names a slot.
func (r PageOrMarkerRef[P]) Valid() bool { return r.slot != nil }

// Get is the slot's content, to read.
func (r PageOrMarkerRef[P]) Get() PageOrMarker[P] {
	assert(r.slot != nil, "the reference names a slot")
	return *r.slot
}

// SwapReferenceForPage replaces the slot's reference with p and returns the
// reference.
func (r PageOrMarkerRef[P]) SwapReferenceForPage(p *P) ReferenceValue {
	assert(r.slot != nil, "the reference names a slot")
	return r.slot.SwapReferenceForPage(p)
}

// SwapPageForReference replaces the slot's page with ref and returns the page.
func (r PageOrMarkerRef[P]) SwapPageForReference(ref ReferenceValue) *P {
	assert(r.slot != nil, "the reference names a slot")
	return r.slot.SwapPageForReference(ref)
}

// SwapReferenceForReference replaces the slot's reference with ref and
// returns the old one.
func (r PageOrMarkerRef[P]) SwapReferenceForReference(ref ReferenceValue) ReferenceValue {
	assert(r.slot != nil, "the reference names a slot")
	return r.slot.SwapReferenceForReference(ref)
}

// SwapContent puts content, which must not be Empty, in the slot and returns
// what the slot held, which may be.
func (r PageOrMarkerRef[P]) SwapContent(content PageOrMarker[P]) PageOrMarker[P] {
	assert(!content.IsEmpty(), "the new content is not Empty")
	assert(r.slot != nil, "the reference names a slot")
	return r.slot.Swap(content)
}

// SetZeroIntervalAwaitingCleanLength sets how much of a zero interval is
// AwaitingClean.
func (r PageOrMarkerRef[P]) SetZeroIntervalAwaitingCleanLength(length uint64) {
	assert(r.slot != nil, "the reference names a slot")
	r.slot.SetZeroIntervalAwaitingCleanLength(length)
}

// GetMarkerShareCount is how many times a marker is shared.
func (r PageOrMarkerRef[P]) GetMarkerShareCount() uint32 {
	assert(r.slot != nil, "the reference names a slot")
	return r.slot.GetMarkerShareCount()
}

// IncrementMarkerShareCount shares a marker once more.
func (r PageOrMarkerRef[P]) IncrementMarkerShareCount() {
	assert(r.slot != nil, "the reference names a slot")
	r.slot.IncrementMarkerShareCount()
}

// DecrementMarkerShareCount shares a marker once less.
func (r PageOrMarkerRef[P]) DecrementMarkerShareCount() {
	assert(r.slot != nil, "the reference names a slot")
	r.slot.DecrementMarkerShareCount()
}
