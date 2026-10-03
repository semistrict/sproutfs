package vmmigrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// MemoryRegionPages presents the memory regions of a migrated VM as what the peer
// server serves, by volume name. The memory regions keep their pages after their
// volumes were handed off, which is exactly what this serves.
func MemoryRegionPages(memoryRegions map[string]*vmmemory.MemoryRegion) map[string]peer.Pages {
	pages := make(map[string]peer.Pages, len(memoryRegions))
	for name, memoryRegion := range memoryRegions {
		pages[name] = regionPages{memoryRegion}
	}
	return pages
}

// regionPages is one memory region as the peer server reads it. A page past the
// region's end is the server's ErrPastEnd, which is how it knows every higher
// page of a request is past the end too.
type regionPages struct{ *vmmemory.MemoryRegion }

func (r regionPages) ReadResident(ctx context.Context, page uint64, dst []byte) (bool, bool, error) {
	held, unpublished, err := r.MemoryRegion.ReadResident(ctx, page, dst)
	if errors.Is(err, vmmemory.ErrRange) {
		return false, false, fmt.Errorf("%w: %w", peer.ErrPastEnd, err)
	}
	return held, unpublished, err
}

// forkPages presents one volume of a fork point as what its parent's peer
// server serves. Only the pages no checkpoint of the parent holds are served:
// everything else is in object storage, where the child reads it from, and
// serving it would only copy what both sides already share by identity.
type forkPages struct {
	point  *volume.ForkPoint
	volume string
	held   map[uint64]bool
}

// ForkPages presents a fork point as what the parent's peer server serves the
// child, by volume name.
func ForkPages(point *volume.ForkPoint) map[string]peer.Pages {
	names := point.Volumes()
	pages := make(map[string]peer.Pages, len(names))
	for _, name := range names {
		held := make(map[uint64]bool)
		for _, page := range point.Pages(name) {
			held[page] = true
		}
		pages[name] = forkPages{point: point, volume: name, held: held}
	}
	return pages
}

func (f forkPages) Resident() ([]uint64, error) { return f.point.Pages(f.volume), nil }

func (f forkPages) PageSize() uint64 { return f.point.PageSize(f.volume) }

// Unpublished is everything a fork point serves: the pages it names are exactly
// the ones no checkpoint of the parent holds, which is why the child has to
// fetch them and why nothing else is offered.
func (f forkPages) Unpublished() ([]uint64, error) { return f.point.Pages(f.volume), nil }

func (f forkPages) ReadResident(ctx context.Context, page uint64, dst []byte) (bool, bool, error) {
	if !f.held[page] {
		return false, false, nil
	}
	if err := f.point.ReadPage(ctx, f.volume, page, dst); err != nil {
		return false, false, err
	}
	// Every page a fork point serves is one no checkpoint holds, so the child's
	// pager keeps it privately until its own first checkpoint publishes it.
	return true, true, nil
}
