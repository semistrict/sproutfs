package host

import (
	"context"

	hostapi "github.com/semistrict/sproutfs/api/host"
)

// Kept lists one VM's kept checkpoints, read out of its control record, so any
// host answers for any VM whether or not anything runs it. A kept checkpoint a
// fork pinned is reported forked, because that is the one a release refuses.
func (h *Host) Kept(ctx context.Context, vmID string) (hostapi.KeptResult, error) {
	record, err := h.control.Read(ctx, vmID)
	if err != nil {
		return hostapi.KeptResult{}, err
	}
	result := hostapi.KeptResult{VM: vmID, Kept: make([]hostapi.Kept, 0, len(record.Kept))}
	for _, kept := range record.Kept {
		result.Kept = append(result.Kept, hostapi.Kept{Checkpoint: kept.Sequence, Time: kept.Time,
			State: kept.State, Forked: record.IsPinned(kept.Sequence)})
	}
	return result, nil
}
