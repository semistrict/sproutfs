package host

import (
	"context"
	"errors"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// PrivateBytes is how much host memory the named VM holds that its volumes do
// not: the pages its guest has written since its last checkpoint, whether they
// are resident, spilled or held by a checkpoint that has not landed. It is the
// part of that VM's memory this host could share with nothing, so it is what an
// operator reads beside the sharing the pager retains.
//
// Zero is a VM holding nothing of its own, a VM this host does not run, and a
// VM whose regions this host cannot see. Like the loss window it is added up
// here rather than in the pager, which has regions and no idea of a VM.
//
// The only thing a region refuses this for is a cancelled context, since
// reading its pages waits on nothing but the region's own page-table work.
func (h *Host) PrivateBytes(ctx context.Context, vmID string) (uint64, error) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	if entry == nil {
		return 0, nil
	}
	return privateBytesOf(ctx, entry.runtime.Regions())
}

// privateBytesOf adds one VM's regions up.
func privateBytesOf(ctx context.Context, regions map[string]*vmmemory.Region) (uint64, error) {
	var total uint64
	var errs error
	for _, region := range regions {
		stats, err := region.Stats(ctx)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		total += stats.PrivateBytes()
	}
	return total, errs
}
