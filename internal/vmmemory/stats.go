package vmmemory

import (
	"context"

	"github.com/semistrict/sproutfs/internal/vmmemory/internal/latency"
)

// LatencyBuckets is how many fixed log-scale buckets every latency histogram
// carries. Bucket 0 counts observations under one microsecond, and bucket i
// counts the half-open range from 2^(i-1) to 2^i microseconds, so the last
// bucket holds everything from about 4.2 seconds upwards. The scale is fixed
// rather than configurable so records written by different runs are comparable
// without carrying their boundaries.
const LatencyBuckets = latency.BucketCount

// Latency is one histogram's snapshot. Count is exactly the sum of Buckets, so
// a record can be checked against the counter it decomposes; TotalNS is the
// summed duration, which is what a mean needs and buckets cannot give.
type Latency = latency.Snapshot

// LatencyBucketUpperNS is the exclusive upper bound of bucket i in
// nanoseconds. The last bucket is unbounded; it reports the largest value the
// scale distinguishes, which is where that bucket begins.
func LatencyBucketUpperNS(i int) uint64 { return latency.BucketUpperNS(i) }

type Stats struct {
	Revocations, RevokeRuns, RevokedPages uint64
	// SpillWrites counts completed scratch writes, each of which may contain
	// several pages, and SpillWriteBytes what they carried. Spill is scratch and
	// never reaches the backing, so it is counted apart from the writes below.
	SpillWrites, SpillWriteBytes                           uint64
	ResidentPages, DirtyPages, LogicalPages                int
	PeakResidentPages, PeakDirtyPages                      int
	Faults, CopyOnWrites, Evictions, Spills, SpillRefaults uint64
	// WriteAheadPages counts the pages stores into fresh zero pages mapped
	// writable beyond the one each store faulted on. The pager cannot see a
	// store into one, so each is published like a stored page, and
	// WriteAheadZeroPages counts those whose checkpoint read still found nothing
	// but zeros: as near as the bytes can tell to a page the guest never stored
	// into, since a store of zeros looks the same.
	WriteAheadPages, WriteAheadZeroPages uint64
	// Loads counts backing reads; LoadedPages the pages they brought in.
	// IdentityHits counts pages mapped to an already resident stored identity
	// without any read. Mappings counts mapping commands; MappedPages the
	// clean pages they covered.
	Loads, LoadedPages, IdentityHits, Mappings, MappedPages uint64
	MappingRuns                                             uint64
	// CheckpointPages counts pages as a capture checkpoint takes them, including
	// those of a seal that failed partway and gave them back.
	CheckpointPages uint64
	// DirtyWaits counts the times a store waited for the dirty budget,
	// CheckpointRequests the checkpoints that wait asked for out of the
	// interval's turn, and DirtyStalls the stores no checkpoint could admit,
	// each of which stops a VM. A host that stalls is a host whose budget or
	// interval is too small for its guests.
	DirtyWaits, CheckpointRequests, DirtyStalls uint64
	// WindowWaits counts the times a store waited because its VM had held a
	// write no checkpoint covers for longer than the loss window, and
	// WindowStalls those where no checkpoint of that VM was ever going to be
	// taken, each of which stops a VM. A host that waits on the window is a host
	// whose publications are not keeping up with its guests; one that stalls on
	// it is running a VM it cannot make durable at all.
	WindowWaits, WindowStalls uint64
	// RefusedMappings counts the faults a client refused a mapping command for,
	// each of which is served again once the pager has revoked something. A
	// host that refuses is a host whose client's mapping budget is too small
	// for the mappings its guest's access pattern fragments into.
	RefusedMappings uint64
	// Protections counts the range write-protect commands seals issued;
	// ProtectedPages the pages those ranges covered.
	Protections, ProtectedPages uint64
	// UFFDReads includes empty reads; RemapEvents counts drained handshakes.
	UFFDReads, RemapEvents uint64
	// Read-only latency histograms of the fault path. FaultQueue is the delay
	// from the UFFD event read to the worker starting on that page, which is
	// scheduling and queueing and no work at all. Fault is one served attempt
	// from its start to its resolution, so its Count is exactly Faults. Mapping
	// is one mapping command's round trip to the VMM process, Revoke one
	// revocation's, Protect one seal range's; Resolve is the page-table
	// installation that completes a trapped access, and Load one backing read.
	// A fault's own duration contains the mapping, resolve and load spans it
	// caused, so the four do not sum to it. Seal is one region's whole seal,
	// which is what a capture's pause is made of and which contains that
	// region's Protect spans.
	FaultQueue, Fault, Mapping, Revoke, Protect, Resolve, Load, Seal Latency
}

func (h *Host) Stats(ctx context.Context) (Stats, error) {
	if err := context.Cause(ctx); err != nil {
		return Stats{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	stats := h.stats
	stats.ResidentPages = h.slots.Total() - h.slots.Free()
	stats.DirtyPages = h.dirty
	stats.LogicalPages = h.logical
	stats.UFFDReads = h.uffdReads.Load()
	stats.RemapEvents = h.remapEvents.Load()
	stats.FaultQueue = h.faultQueueLatency.Snapshot()
	stats.Fault = h.faultLatency.Snapshot()
	stats.Mapping = h.mappingLatency.Snapshot()
	stats.Revoke = h.revokeLatency.Snapshot()
	stats.Protect = h.protectLatency.Snapshot()
	stats.Resolve = h.resolveLatency.Snapshot()
	stats.Load = h.loadLatency.Snapshot()
	stats.Seal = h.sealLatency.Snapshot()
	return stats, h.err
}
