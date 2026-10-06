package vmmemory

import "fmt"

// What the probe build's audit (probe_on.go) sees of a page, a frame, and of a
// binding. An ordinary build checks nothing.

// probePage is a page as the audit sees it.
type probePage interface {
	// probeAbsent reports no page at all: a nil frame.
	probeAbsent() bool
	// probeSlot is where the page's bytes are, its slot -1 once they went.
	probeSlot() fileSlot
	// probePublished reports a page whose bytes cannot change while it
	// holds the identity a checkpoint gave it.
	probePublished() bool
	// probeUnnamed reports one region's own state that no name reaches,
	// which no other region may reach either.
	probeUnnamed() bool
	// probeOther reports a binding of a region other than region that
	// reaches the page, and its page number. Caller holds the host lock.
	probeOther(region any) (uint64, bool)
	// probeName is what a finding calls the page.
	probeName() string
}

// probeBinding is a binding as the audit sees it.
type probeBinding interface {
	probeIndex() uint64
	probeRegion() any
}

func (f *frame) probeAbsent() bool   { return f == nil }
func (f *frame) probeSlot() fileSlot { return f.fileSlot }

// probePublished is a root's page: a page the pager holds under an identity is
// in an identity root and never in a region's layer.
func (f *frame) probePublished() bool { return f.layer == nil && f.slot >= 0 }

// probeUnnamed is a page of a region's layer no fork point lends.
func (f *frame) probeUnnamed() bool { return f.layer != nil && f.lent == nil }
func (f *frame) probeName() string  { return fmt.Sprintf("frame of slot %d", f.slot) }

func (f *frame) probeOther(region any) (uint64, bool) {
	for other := range f.aliases.all() {
		if other.region != region {
			return other.index, true
		}
	}
	return 0, false
}

func (b *binding) probeIndex() uint64 { return b.index }
func (b *binding) probeRegion() any   { return b.region }
