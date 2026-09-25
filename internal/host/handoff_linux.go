//go:build linux && (amd64 || arm64)

package host

import (
	hostapi "github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
)

// This is the whole of the translation between what a host runs and what its
// API says. It carries the build tag the supervisor does, because the
// supervisor is its only caller and nothing the other builds compile has a
// handoff to translate. The API packages are wire types and nothing else — a client that
// reads a handoff must not link a pager to do it — so a handoff crosses here,
// in the one place that has both halves.

// apiHandoff is the wire form of a handoff the control plane carries to the
// destination's Receive.
func apiHandoff(handoff vmmigrate.Handoff) hostapi.Handoff {
	memoryRegions := make([]hostapi.HandoffMemoryRegion, 0, len(handoff.MemoryRegions))
	for _, memoryRegion := range handoff.MemoryRegions {
		runs := make([]hostapi.HandoffPageRun, 0, len(memoryRegion.Unpublished))
		for _, run := range memoryRegion.Unpublished {
			runs = append(runs, hostapi.HandoffPageRun{First: run.First, Count: run.Count})
		}
		memoryRegions = append(memoryRegions, hostapi.HandoffMemoryRegion{Name: memoryRegion.Name, Size: memoryRegion.Size,
			Unpublished: runs, UnpublishedAge: memoryRegion.UnpublishedAge})
	}
	return hostapi.Handoff{VMID: handoff.VMID, State: handoff.State, Checkpoint: handoff.Checkpoint,
		Parent: handoff.Parent, ParentCheckpoint: handoff.ParentCheckpoint,
		Source: string(handoff.Source), PageSize: handoff.PageSize,
		MemoryRegions: memoryRegions, PausedAt: handoff.PausedAt}
}

// handoffOf is the wire form read back, which is what a destination takes a VM
// over from.
func handoffOf(handoff hostapi.Handoff) vmmigrate.Handoff {
	memoryRegions := make([]vmmigrate.MemoryRegionInfo, 0, len(handoff.MemoryRegions))
	for _, memoryRegion := range handoff.MemoryRegions {
		runs := make([]vmmigrate.PageRun, 0, len(memoryRegion.Unpublished))
		for _, run := range memoryRegion.Unpublished {
			runs = append(runs, vmmigrate.PageRun{First: run.First, Count: run.Count})
		}
		memoryRegions = append(memoryRegions, vmmigrate.MemoryRegionInfo{Name: memoryRegion.Name, Size: memoryRegion.Size,
			Unpublished: runs, UnpublishedAge: memoryRegion.UnpublishedAge})
	}
	return vmmigrate.Handoff{VMID: handoff.VMID, State: handoff.State, Checkpoint: handoff.Checkpoint,
		Parent: handoff.Parent, ParentCheckpoint: handoff.ParentCheckpoint,
		Source: platform.Address(handoff.Source), PageSize: handoff.PageSize,
		MemoryRegions: memoryRegions, PausedAt: handoff.PausedAt}
}

// apiStore is the wire form of what this host's object store has served.
func apiStore(traffic platform.ObjectTraffic) hostapi.Store {
	count := func(c platform.ObjectCount) hostapi.StoreCount {
		return hostapi.StoreCount{Calls: c.Calls, Failures: c.Failures, Bytes: c.Bytes}
	}
	return hostapi.Store{Head: count(traffic.Head), Get: count(traffic.Get), Put: count(traffic.Put),
		Delete: count(traffic.Delete), List: count(traffic.List)}
}
