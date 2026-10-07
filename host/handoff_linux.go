//go:build linux && (amd64 || arm64)

package host

import (
	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/internal/latency"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/bounded"
	"github.com/semistrict/sproutfs/vmmigrate"
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
			Ephemeral: memoryRegion.Ephemeral, Unpublished: runs, UnpublishedAge: memoryRegion.UnpublishedAge})
	}
	return hostapi.Handoff{VMID: handoff.VMID, State: handoff.State, Checkpoint: handoff.Checkpoint,
		Parent: handoff.Parent, ParentCheckpoint: handoff.ParentCheckpoint,
		Source: string(handoff.Source), PageSize: handoff.PageSize,
		MemoryRegions: memoryRegions, PausedAt: handoff.PausedAt, Pull: handoff.Pull,
		CheckpointInterval: handoff.CheckpointInterval}
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
			Ephemeral: memoryRegion.Ephemeral, Unpublished: runs, UnpublishedAge: memoryRegion.UnpublishedAge})
	}
	return vmmigrate.Handoff{VMID: handoff.VMID, State: handoff.State, Checkpoint: handoff.Checkpoint,
		Parent: handoff.Parent, ParentCheckpoint: handoff.ParentCheckpoint,
		Source: platform.Address(handoff.Source), PageSize: handoff.PageSize,
		MemoryRegions: memoryRegions, PausedAt: handoff.PausedAt, Pull: handoff.Pull,
		CheckpointInterval: handoff.CheckpointInterval}
}

// apiCheckpoints is the wire form of what the interval checkpoints did.
func apiCheckpoints(c CheckpointActivity) hostapi.Checkpoints {
	return hostapi.Checkpoints{Attempts: c.Attempts, Published: c.Published,
		CaptureFailed: c.CaptureFailed, PublishFailed: c.PublishFailed, Fenced: c.Fenced,
		UploadedBytes: c.UploadedBytes, Pause: hostapi.LatencyOf(c.Pause), Upload: hostapi.LatencyOf(c.Upload)}
}

// apiJournal is the wire form of what durable flush did.
func apiJournal(j JournalActivity) hostapi.Journal {
	return hostapi.Journal{DurableFlush: j.DurableFlush, Served: j.Served,
		Flushes: hostapi.Outcomes{Succeeded: j.Flushes.Succeeded, Failed: j.Flushes.Failed},
		Flush:   hostapi.LatencyOf(j.Flush), Capture: hostapi.LatencyOf(j.Capture),
		RingBytes: j.RingBytes, LiveBytes: j.LiveBytes, Position: j.Position}
}

// apiLifecycle is the wire form of what this host did with its VMs.
func apiLifecycle(a Activity) hostapi.Lifecycle {
	outcomes := func(o Outcomes) hostapi.Outcomes {
		return hostapi.Outcomes{Succeeded: o.Succeeded, Failed: o.Failed}
	}
	return hostapi.Lifecycle{Migrations: outcomes(a.Migrations), Forks: outcomes(a.Forks),
		Receives: outcomes(a.Receives), MigrationPause: hostapi.LatencyOf(a.MigrationPause),
		ForkPause: hostapi.LatencyOf(a.ForkPause), Deaths: a.Deaths, Fenced: a.Fenced, Stopped: a.Stopped}
}

// apiStore is the wire form of what one bucket has served this host: what its
// meter counted of the calls, and what its bounds did to their attempts.
func apiStore(metered *platform.MeteredObjectStore, recoveries bounded.Recoveries) hostapi.Store {
	traffic, took := metered.Traffic(), metered.Latency()
	count := func(c platform.ObjectCount, l latency.Snapshot, r bounded.Recovery) hostapi.StoreCount {
		return hostapi.StoreCount{Calls: c.Calls, Failures: c.Failures, Bytes: c.Bytes, Latency: hostapi.LatencyOf(l),
			FirstByteTimeouts: r.FirstByteTimeouts, StallTimeouts: r.StallTimeouts, Retries: r.Retries}
	}
	return hostapi.Store{Head: count(traffic.Head, took.Head, recoveries.Head),
		Get:    count(traffic.Get, took.Get, recoveries.Get),
		Put:    count(traffic.Put, took.Put, recoveries.Put),
		Delete: count(traffic.Delete, took.Delete, recoveries.Delete),
		List:   count(traffic.List, took.List, recoveries.List)}
}
