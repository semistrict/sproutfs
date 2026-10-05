package vmmemory

import (
	"context"
	"time"
)

// zirconHost is a pager's state under the zircon core, nil under the current
// one. The zircon core runs over the region layer and the identity roots of
// internal/zirconvm; see Core.
type zirconHost struct {
	host *Host
}

// zirconRegion is a memory region's state under the zircon core.
type zirconRegion struct {
	region *MemoryRegion
}

// The operations the zircon core does not serve yet. Each refuses, naming
// itself, and changes nothing.

func (z *zirconHost) attach(context.Context, MemoryRegionBacking, Mapping) (*MemoryRegion, error) {
	return nil, unsupported(CoreZircon, "attach a memory region")
}

func (z *zirconHost) dropIdle(context.Context) (int, error) {
	return 0, unsupported(CoreZircon, "drop idle pages")
}

func (z *zirconHost) stats(context.Context) (Stats, error) {
	return Stats{}, unsupported(CoreZircon, "report a pager's statistics")
}

func (z *zirconHost) sharing(context.Context) (SharingStats, error) {
	return SharingStats{}, unsupported(CoreZircon, "report a pager's sharing")
}

func (z *zirconHost) settlePrefetches(context.Context) error {
	return unsupported(CoreZircon, "settle prefetches")
}

func (z *zirconRegion) fault(context.Context, uint64, bool) error {
	return unsupported(CoreZircon, "serve a fault")
}

func (z *zirconRegion) populate(context.Context) error {
	return unsupported(CoreZircon, "populate a memory region")
}

func (z *zirconRegion) seal(context.Context) error {
	return unsupported(CoreZircon, "seal a memory region")
}

func (z *zirconRegion) unseal(context.Context) error {
	return unsupported(CoreZircon, "unseal a memory region")
}

func (z *zirconRegion) verify(context.Context) error {
	return unsupported(CoreZircon, "verify a memory region")
}

func (z *zirconRegion) detach(context.Context) error {
	return unsupported(CoreZircon, "detach a memory region")
}

func (z *zirconRegion) giveBackColdCopies(context.Context) (int, error) {
	return 0, unsupported(CoreZircon, "give cold copies back")
}

func (z *zirconRegion) settlePrefetches(context.Context) error {
	return unsupported(CoreZircon, "settle a memory region's prefetches")
}

func (z *zirconRegion) setUnpublishedAge(time.Duration) {}

func (z *zirconRegion) oldestUnpublished() time.Time { return time.Time{} }

func (z *zirconRegion) readResident(context.Context, uint64, []byte) (bool, bool, error) {
	return false, false, unsupported(CoreZircon, "read a resident page")
}

func (z *zirconRegion) resident() ([]uint64, error) {
	return nil, unsupported(CoreZircon, "list resident pages")
}

func (z *zirconRegion) handoff(context.Context) (time.Duration, error) {
	return 0, unsupported(CoreZircon, "hand a memory region off")
}

func (z *zirconRegion) unpublished() ([]uint64, error) {
	return nil, unsupported(CoreZircon, "list unpublished pages")
}

func (z *zirconRegion) stats(context.Context) (MemoryRegionStats, error) {
	return MemoryRegionStats{}, unsupported(CoreZircon, "report a memory region's statistics")
}
