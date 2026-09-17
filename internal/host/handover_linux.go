//go:build linux && (amd64 || arm64)

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	hostapi "github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/api/orch"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

func (s *supervisor) Migrate(ctx context.Context, id string, destination platform.Address) (hostapi.MigrateResult, error) {
	if _, err := s.running(id); err != nil {
		return hostapi.MigrateResult{}, err
	}
	stopped := s.clock.Now()
	handoff, err := s.host.Migrate(ctx, id, destination)
	if err != nil {
		return hostapi.MigrateResult{}, fmt.Errorf("migrating %s to %s: %w", id, destination, err)
	}
	// The VM runs on the destination from here. This host holds only its
	// frames, which its page server serves until the destination has them all.
	s.forget(id)
	return hostapi.MigrateResult{Handoff: apiHandoff(handoff), Stop: s.since(stopped)}, nil
}

func (s *supervisor) Receive(ctx context.Context, wire hostapi.Handoff) (hostapi.ReceiveResult, error) {
	handoff := handoffOf(wire)
	// Receive returns only once every page no checkpoint has is here: those
	// pages exist nowhere else. A receive that could not get them has already
	// given the VM up, and one refused before it started never recorded a
	// machine that outlived it, so either way nothing of it is left here.
	received, err := s.host.Receive(ctx, handoff)
	if err != nil {
		s.forget(handoff.VMID)
		return hostapi.ReceiveResult{}, fmt.Errorf("receiving %s from %s: %w", handoff.VMID, handoff.Source, err)
	}
	stats := received.Stats()
	// The source may release its frames now. The rest of its resident set keeps
	// arriving behind the running guest as long as it is still serving; closing
	// here would send every page of it to object storage instead.
	go func() {
		streamCtx := context.WithoutCancel(ctx)
		if err := received.Streamed(streamCtx); err != nil {
			slog.WarnContext(streamCtx, "host: the migration's bulk stream stopped early",
				"vm", handoff.VMID, "source", handoff.Source, "error", err)
		}
		received.Close()
	}()
	s.mu.Lock()
	m := s.machines[handoff.VMID]
	s.mu.Unlock()
	if m == nil {
		return hostapi.ReceiveResult{}, fmt.Errorf("%w: %s", ErrNotRunning, handoff.VMID)
	}
	return hostapi.ReceiveResult{VM: s.record(m),
		Pause:     hostapi.Of(stats.ResumedAt.Sub(stats.PausedAt)),
		Stream:    s.since(stats.ResumedAt),
		PeerPages: stats.PeerPages, VolumePages: stats.VolumePages,
		Fetched: stats.Fetched, Unpublished: stats.Unpublished}, nil
}

// startReceived builds the VMM of a VM this host takes over. Every region
// attaches through the backing the migration supplies — the source host that
// still holds its pages, with this host's own volume behind it — so a fault
// reaches the source rather than reading a checkpoint that does not have the
// guest's last writes.
func (s *supervisor) startReceived(ctx context.Context, vm *volume.VM,
	backings map[string]vmmemory.Backing, state []byte) (Machine, error) {
	process, err := vmmachine.Start(ctx, s.machineConfig(vm, state, backings))
	if err != nil {
		return nil, err
	}
	if err := process.Release(ctx); err != nil {
		return nil, errors.Join(err, process.Close())
	}
	if err := s.remember(&machine{vm: vm, process: process}); err != nil {
		return nil, errors.Join(err, process.Close())
	}
	return process, nil
}

func (s *supervisor) Released(ctx context.Context, id string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return s.host.ReleaseMigrated(id)
}

func (s *supervisor) Abandoned(ctx context.Context, id string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	slog.InfoContext(ctx, "host: a handover was given up by the control plane", "vm", id)
	return s.host.Abandon(id)
}

// Drain moves every VM this host runs and returns only when nothing is left to
// hand over. It is what the deployment's preStop hook calls: once the page
// server is serving nothing, every page this host held is either on another
// host or in object storage, and exiting costs nothing.
//
// The orchestrator chooses each destination and drives both halves of the
// migration, so a drain needs to know nothing about the other hosts.
func (s *supervisor) Drain(ctx context.Context) (hostapi.DrainResult, error) {
	began := s.clock.Now()
	// The whole drain is bounded here, so the wait for the handed-over pages to
	// arrive answers to the same deadline the hand-overs did.
	ctx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()
	result := hostapi.DrainResult{Host: s.config.PodName, Moved: []string{}, Remaining: []string{}}
	moved, remaining, err := s.host.DrainVia(ctx,
		DrainBudget{PerVM: drainVMTimeout, Concurrency: drainConcurrency}, s.handOver)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	result.Moved = append(result.Moved, moved...)
	result.Remaining = append(result.Remaining, remaining...)
	// Serving is what this host still holds pages for. A host that exits while
	// it is not empty loses the writes those pages carry.
	for len(s.host.Status().Serving) > 0 {
		if err := s.clock.Sleep(ctx, drainPoll); err != nil {
			errs = append(errs, err)
			break
		}
	}
	result.Remaining = append(result.Remaining, s.host.Status().Serving...)
	result.Seconds = s.since(began)
	slog.InfoContext(ctx, "host: drained", "moved", result.Moved, "remaining", result.Remaining,
		"seconds", float64(result.Seconds))
	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// handOver asks the orchestrator to move one VM off this host, and tells it
// what happened either way. The orchestrator chooses the destination and drives
// both halves of the migration, so a drain needs to know nothing about the
// other hosts.
func (s *supervisor) handOver(ctx context.Context, id string) error {
	s.report(ctx, orch.DrainReport{Host: s.config.PodName, VM: id, Phase: orch.DrainStarted})
	_, err := s.orchestrator.Migrate(ctx, id, "")
	finished := orch.DrainReport{Host: s.config.PodName, VM: id, Phase: orch.DrainFinished}
	if err != nil {
		slog.ErrorContext(ctx, "host: draining a VM failed", "vm", id, "error", err)
		finished.Error = err.Error()
	}
	s.report(ctx, finished)
	return err
}

// report tells the orchestrator what this drain is doing with one VM. A report
// that does not arrive costs the drain nothing — the orchestrator drives the
// migration itself and learns the outcome that way — so it is logged rather
// than stopping a host that is trying to leave cleanly.
//
// It carries a deadline of its own, off the drain's: the report that matters
// most is the one about a VM whose own handover just ran out of time, and a
// context that is already done would carry none of them.
func (s *supervisor) report(ctx context.Context, report orch.DrainReport) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainReportTimeout)
	defer cancel()
	if err := s.orchestrator.ReportDrain(ctx, report); err != nil {
		slog.WarnContext(ctx, "host: reporting a drain failed",
			"vm", report.VM, "phase", report.Phase, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Deleting and shutdown
// ---------------------------------------------------------------------------
