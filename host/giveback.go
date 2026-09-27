package host

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"

	"github.com/semistrict/sproutfs/vmmemory"
)

// The bounds of one VM's give-back in one interval. It compares at most
// giveBackPages copies and giveBackBytes of them, whichever is fewer: 512 pages
// at 2 MiB and 4096 at 4 KiB. Each page costs a write-protect, a comparison and,
// where the page goes back, one mapping command, all with the guest running, so
// the bound is what keeps that work a trickle beside the guest rather than a
// stall. A copy is compared once unless it goes back, so what is left over is
// taken up by the next interval.
const (
	giveBackPages = 4096
	giveBackBytes = 1 << 30
)

// givingBack gives back, once an interval, the RAM copies of one VM whose
// bytes its guest never changed: see vmmemory.MemoryRegion.GiveBack. RAM is
// never checkpointed on the interval, so nothing else would. A disk's copies
// are settled by the checkpoint the interval takes of it instead. The cold
// copies of either are given back sooner by their sessions, so what this finds
// is the copies those left: see vmmemory.MemoryRegion.GiveBackColdCopies.
//
// It runs beside the checkpoint loop rather than inside it, because a
// checkpoint that fails, or waits for its publication, is no reason to keep a
// guest's copies. It pauses nothing.
func (h *Host) givingBack(ctx context.Context, vmID string, entry *registration) {
	for {
		if err := h.clock.Sleep(ctx, jittered(h.entropy, h.checkpointInterval)); err != nil {
			return
		}
		regions := entry.runtime.MemoryRegions()
		for _, name := range slices.Sorted(maps.Keys(regions)) {
			memory := regions[name]
			if memory.Kind() != vmmemory.Ram {
				continue
			}
			limit := int(min(giveBackPages, giveBackBytes/memory.PageSize()))
			given, err := memory.GiveBack(ctx, limit)
			switch {
			case err == nil:
				if given > 0 {
					slog.DebugContext(ctx, "host: gave back unchanged RAM copies", "vm", vmID, "volume", name, "pages", given)
				}
			case ctx.Err() != nil:
				return
			case errors.Is(err, vmmemory.ErrClosed), errors.Is(err, vmmemory.ErrHandedOff):
				// The VM is leaving this host, and its loop with it.
			default:
				slog.WarnContext(ctx, "host: giving back a VM's unchanged RAM copies failed",
					"vm", vmID, "volume", name, "error", err)
			}
		}
	}
}
