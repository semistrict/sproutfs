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
// VM whose memory regions this host cannot see. Like the loss window it is added up
// here rather than in the pager, which has memory regions and no idea of a VM.
//
// The only thing a memory region refuses this for is a cancelled context, since
// reading its pages waits on nothing but the memory region's own page-table work.
func (h *Host) PrivateBytes(ctx context.Context, vmID string) (uint64, error) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	if entry == nil {
		return 0, nil
	}
	return privateBytesOf(ctx, entry.runtime.MemoryRegions())
}

// privateBytesOf adds one VM's memory regions up.
func privateBytesOf(ctx context.Context, memoryRegions map[string]*vmmemory.MemoryRegion) (uint64, error) {
	var total uint64
	var errs error
	for _, memoryRegion := range memoryRegions {
		stats, err := memoryRegion.Stats(ctx)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		total += stats.PrivateBytes()
	}
	return total, errs
}
