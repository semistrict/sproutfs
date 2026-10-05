package vmmemory

import "fmt"

// What the probe build's audit (probe_on.go) sees of a page and of a binding,
// whichever core made them: a resident page and a binding of the current
// core, or a frame and a binding of the zircon core. An ordinary build checks
// nothing, but the two cores still say the same things to it.

// probePage is a page as the audit sees it.
type probePage interface {
	// probeAbsent reports no page at all: a nil page of either core.
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

func (pg *resident) probeAbsent() bool    { return pg == nil }
func (pg *resident) probeSlot() fileSlot  { return pg.fileSlot }
func (pg *resident) probePublished() bool { return pg.published() }
func (pg *resident) probeUnnamed() bool   { return pg.private && pg.key == (pageKey{}) }
func (pg *resident) probeName() string    { return fmt.Sprintf("%+v", pg.key.id) }

func (pg *resident) probeOther(region any) (uint64, bool) {
	for other := range pg.aliases.all() {
		if other.memoryRegion != region {
			return other.index, true
		}
	}
	return 0, false
}

func (b *binding) probeIndex() uint64 { return b.index }
func (b *binding) probeRegion() any   { return b.memoryRegion }

func (f *zframe) probeAbsent() bool   { return f == nil }
func (f *zframe) probeSlot() fileSlot { return f.fileSlot }

// probePublished is a root's page: a page the zircon core holds under an
// identity is in an identity root and never in a region's layer.
func (f *zframe) probePublished() bool { return f.layer == nil && f.slot >= 0 }

// probeUnnamed is a page of a region's layer no fork point lends.
func (f *zframe) probeUnnamed() bool { return f.layer != nil && f.lent == nil }
func (f *zframe) probeName() string  { return fmt.Sprintf("frame of slot %d", f.slot) }

func (f *zframe) probeOther(region any) (uint64, bool) {
	for other := range f.aliases.all() {
		if other.region != region {
			return other.index, true
		}
	}
	return 0, false
}

func (b *zbinding) probeIndex() uint64 { return b.index }
func (b *zbinding) probeRegion() any   { return b.region }
