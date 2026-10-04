//go:build linux && (amd64 || arm64)

package host

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/semistrict/sproutfs/vmmachine"
)

// startVMM starts one VM's VMM and records the start on the timeline ctx
// carries: the whole start, its phases in order, and what each memory
// region's attach cost.
func startVMM(ctx context.Context, config vmmachine.Config) (*vmmachine.Process, error) {
	t := timelineOf(ctx)
	if t == nil {
		return vmmachine.Start(ctx, config)
	}
	began := t.clock.Now()
	process, err := vmmachine.Start(ctx, config)
	t.add("vmm start", began, t.clock.Since(began))
	if err != nil {
		return nil, err
	}
	phases := process.StartPhases()
	at := began
	for _, phase := range []struct {
		name string
		ns   int64
	}{{"vmm process", phases.ProcessNS}, {"vmm state load", phases.StateLoadNS},
		{"vmm sessions", phases.SessionsNS}, {"vmm ready", phases.ReadyNS}} {
		took := time.Duration(phase.ns)
		t.add(phase.name, at, took)
		at = at.Add(took)
	}
	for _, region := range slices.Sorted(maps.Keys(phases.Attachments)) {
		attach := phases.Attachments[region]
		t.attached(region, time.Duration(attach.DurationNS), attach.Populate)
	}
	return process, nil
}
