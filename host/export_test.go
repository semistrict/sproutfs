package host

import (
	"context"

	"github.com/semistrict/sproutfs/platform"
)

// WithTimeline is a context carrying a start's timeline on clock, as the
// supervisor begins one for each create, open, fork and receive.
func WithTimeline(ctx context.Context, clock platform.Clock, vm, how string) context.Context {
	ctx, _ = startTimeline(ctx, clock, vm, how)
	return ctx
}

// TimelineSteps names the steps the timeline in ctx has recorded, in the order
// its log line gives them.
func TimelineSteps(ctx context.Context) []string {
	var names []string
	for _, s := range timelineOf(ctx).ordered() {
		names = append(names, s.name)
	}
	return names
}

// CacheDiskReport is the page cache's disk as /status reports it.
var CacheDiskReport = cacheDiskReport

// CacheFillReport is what the host's fills did as /status reports it.
var CacheFillReport = cacheFillReport

// HotTierReport is what the host's hot tier did as /status reports it.
var HotTierReport = hotTierReport

// RoomFor is the most bytes the entries of blocks blocks of one volume take
// on a journal.
var RoomFor = roomFor

// PostCopied tells durable flush that every page of a VM brought here has
// arrived, as a receive does once its post-copy ends.
func (h *Host) PostCopied(vmID string) { h.postCopied(vmID) }
