//go:build linux && (amd64 || arm64)

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

func (s *supervisor) Create(ctx context.Context, request hostapi.CreateRequest) (hostapi.CreateResult, error) {
	began := s.clock.Now()
	id := request.ID
	if request.From != nil && request.Template != "" {
		return hostapi.CreateResult{}, fmt.Errorf("%w: a create names a template or a checkpoint, not both",
			ErrRequest)
	}
	if request.Ephemeral%PMEMPageSize != 0 {
		return hostapi.CreateResult{}, fmt.Errorf("%w: an ephemeral disk is whole %d-byte pages, not %d bytes",
			ErrRequest, PMEMPageSize, request.Ephemeral)
	}
	if err := s.absent(id); err != nil {
		return hostapi.CreateResult{}, err
	}
	prepared := s.clock.Now()
	point, source, name, err := s.createPoint(ctx, request)
	if err != nil {
		return hostapi.CreateResult{}, err
	}
	templateSeconds := s.since(prepared)

	forked := s.clock.Now()
	// An ephemeral disk is given to the new VM here, zeroed: it is what the
	// fork adds beyond what it inherits, and its root is where it is first
	// recorded. One the checkpoint it inherits already has takes this size.
	var added []volume.VolumeSpec
	if request.Ephemeral != 0 {
		added = append(added, volume.VolumeSpec{Name: ephemeralVolume, Size: request.Ephemeral,
			PageSize: PMEMPageSize, Ephemeral: true})
	}
	vm, _, err := CreateFork(ctx, s.host.Volumes(), id, point, added...)
	if err != nil {
		return hostapi.CreateResult{}, fmt.Errorf("forking %s from %s: %w", id, source, err)
	}
	forkSeconds := s.since(forked)

	// A new VM is a fork of a published checkpoint: a template's, or another
	// VM's. A fork runs on the host that took it and nowhere else until it has
	// published a root index of its own: nothing can open it, so a host lost
	// in the meantime loses it, and nothing can seal it, so it cannot be forked
	// or migrated.
	// Create is not finished until that is no longer true — every VM it returns
	// is one the rest of the deployment can act on.
	//
	// It is published here, before the guest exists, because that is when it is
	// free: the VM's bytes are still exactly what it inherited, so the root seals
	// no pages and uploads none, and writes only its own index and the control
	// record's selection of it. Taking it after the boot instead would pause a
	// guest that has not yet done anything, to seal the little it had.
	//
	// It is also where the VM either resumes or takes the shape the request
	// asks for: its RAM, its disk and its processors. Another VM's checkpoint
	// with VMM state resumes, and its root names that state and that memory. A
	// template never ran, so there is no memory to lose by discarding it, and a
	// checkpoint without state has memory no registers describe: their root is
	// a cold boot's publication, the one moment the shape may change, and the
	// new VM boots its kernel over the disk it inherits. A shape asks for that
	// cold boot whatever the checkpoint holds.
	rooted := s.clock.Now()
	shape := ColdShape{Memory: vmmachine.RAMVolume, Root: rootVolume,
		MemoryBytes: request.Memory, RootBytes: request.Disk, VCPUs: request.VCPUs}
	state, err := s.host.CreateRoot(ctx, vm, point, shape)
	if err != nil {
		// A VM whose root never published is one nothing else can ever act on,
		// so it is given up rather than left behind as an unopenable record.
		return hostapi.CreateResult{}, errors.Join(
			fmt.Errorf("publishing the root checkpoint of %s", id), err,
			closing(ctx, vm))
	}
	rootSeconds := s.since(rooted)

	booted := s.clock.Now()
	m, err := s.boot(ctx, vm, state, name)
	if err != nil {
		return hostapi.CreateResult{}, err
	}
	return hostapi.CreateResult{VM: s.record(m), Template: templateSeconds, Fork: forkSeconds,
		Boot: s.since(booted), Root: rootSeconds, Total: s.since(began), Resumed: len(state) > 0}, nil
}

// createPoint is the published checkpoint a create forks, and what the create
// names it by in its errors: another VM's checkpoint when the request names
// one, and the template's otherwise. Another VM's is pinned in its record
// without its epoch, because that VM need not run anywhere.
func (s *supervisor) createPoint(ctx context.Context, request hostapi.CreateRequest) (*volume.ForkPoint, string, string, error) {
	from := request.From
	if from == nil {
		template, name, err := s.templateNamed(ctx, control.TenantOf(request.ID), request.Template)
		if err != nil {
			return nil, "", "", err
		}
		return template.Point, "template " + name, name, nil
	}
	parent := control.Ref{VM: from.VM, Sequence: from.Checkpoint}
	point, err := s.host.Volumes().InheritPublished(ctx, request.ID, parent)
	if err != nil {
		return nil, "", "", fmt.Errorf("pinning the checkpoint of %s: %w", from.VM, err)
	}
	return point, "checkpoint " + point.Parent().String(), "", nil
}

func (s *supervisor) Open(ctx context.Context, id string, request hostapi.OpenRequest) (hostapi.OpenResult, error) {
	began := s.clock.Now()
	if err := s.absent(id); err != nil {
		return hostapi.OpenResult{}, err
	}
	if err := s.coldRequest(request); err != nil {
		return hostapi.OpenResult{}, err
	}
	vm, state, err := s.opening(ctx, id, request)
	if err != nil {
		return hostapi.OpenResult{}, err
	}
	restored := s.clock.Now()
	m, err := s.boot(ctx, vm, state, "")
	if err != nil {
		return hostapi.OpenResult{}, err
	}
	// A VM whose checkpoint had no VMM state booted whether or not it was asked
	// to, and a flow reading what it came back as has to know.
	return hostapi.OpenResult{VM: s.record(m), Restore: s.since(restored),
		Total: s.since(began), Cold: len(state) == 0}, nil
}

// coldRequest refuses an open this host will not act on whatever its VMs are
// doing: a shape for a warm start, which is a VM whose memory describes the
// shape it has, and a cold start on a host that cannot boot a kernel. Both are
// settled before the VM is opened, so nothing is ever discarded for a request
// that was never going to work.
func (s *supervisor) coldRequest(request hostapi.OpenRequest) error {
	if !request.Cold {
		if request.Memory != 0 || request.Disk != 0 || request.VCPUs != 0 {
			return fmt.Errorf("%w: a VM's memory, disk and processors can change only at a cold start, "+
				"which is the one moment nothing in memory describes its shape", ErrRequest)
		}
		return nil
	}
	if !s.config.Starter.Boots() {
		return fmt.Errorf("%w: this host's VMM starter cannot boot a kernel, so it cannot boot a VM cold",
			ErrRequest)
	}
	return nil
}

// opening opens the VM and reports the VMM state its guest starts from: the
// state the selected checkpoint holds for a warm start, and nothing at all for
// a cold one, whose checkpoint discards the memory and the state together.
func (s *supervisor) opening(ctx context.Context, id string,
	request hostapi.OpenRequest) (*volume.VM, []byte, error) {
	if request.Cold {
		vm, err := s.host.OpenCold(ctx, id, ColdShape{Memory: vmmachine.RAMVolume, Root: rootVolume,
			MemoryBytes: request.Memory, RootBytes: request.Disk, VCPUs: request.VCPUs})
		if err != nil {
			return nil, nil, fmt.Errorf("cold starting %s: %w", id, err)
		}
		return vm, nil, nil
	}
	vm, err := s.host.Volumes().Open(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", id, err)
	}
	// The VMM state of the checkpoint the control record selects is what this
	// guest resumes from; a checkpoint published without one is cold booted.
	state, err := s.host.Starting(ctx, vm, vmmachine.RAMVolume)
	if err != nil {
		return nil, nil, errors.Join(err, closing(ctx, vm))
	}
	return vm, state, nil
}

// Fork is a migration handoff from a parent that keeps running. The parent
// pauses only for its VMM state capture and the seal, and publishes nothing to
// be forked: the child inherits the checkpoint the parent's control record
// already selects, and the pages written since it come out of the parent's
// sealed pages.
//
// It is one path wherever the children land. This host builds one handoff per
// child and holds the point for each of them; the control plane gives every
// handoff to its destination's Receive — this host included, for a child with
// no destination named — and tells this one to release when the child has every
// page it inherited. A child taken in here is served nothing: it attaches over
// the fork point itself, so its inherited pages never reach the wire.
func (s *supervisor) Fork(ctx context.Context, parent string, request hostapi.ForkRequest) (hostapi.ForkResult, error) {
	began := s.clock.Now()
	if _, err := s.running(parent); err != nil {
		return hostapi.ForkResult{}, err
	}
	captured := s.clock.Now()
	handoffs, err := s.host.Fork(ctx, parent, request.IDs, platform.Address(request.Destination))
	if err != nil {
		return hostapi.ForkResult{}, fmt.Errorf("forking %s: %w", parent, err)
	}
	wire := make([]hostapi.Handoff, 0, len(handoffs))
	for _, handed := range handoffs {
		wire = append(wire, apiHandoff(handed))
	}
	return hostapi.ForkResult{Handoffs: wire, Capture: s.since(captured),
		Total: s.since(began)}, nil
}

func (s *supervisor) Capture(ctx context.Context, id string, request hostapi.CaptureRequest) (hostapi.CaptureResult, error) {
	m, err := s.running(id)
	if err != nil {
		return hostapi.CaptureResult{}, err
	}
	if request.Into != "" {
		if request.Keep {
			return hostapi.CaptureResult{}, fmt.Errorf("%w: a capture into a new VM publishes that VM's root, "+
				"which its record selects, so there is nothing to keep", ErrRequest)
		}
		// The new VM never boots here, so this host keeps nothing of it.
		if err := s.absent(request.Into); err != nil {
			return hostapi.CaptureResult{}, err
		}
		began := s.clock.Now()
		root, err := s.host.CaptureInto(ctx, id, request.Into)
		if err != nil {
			return hostapi.CaptureResult{}, err
		}
		return hostapi.CaptureResult{VM: request.Into, Checkpoint: root.Sequence, Publish: s.since(began)}, nil
	}
	paused := s.clock.Now()
	ckpt, err := Capture(ctx, m.vm, m.process, s.clock, volume.Terms{Keep: request.Keep})
	if err != nil {
		return hostapi.CaptureResult{}, fmt.Errorf("capturing %s: %w", id, err)
	}
	pause := s.since(paused)
	published := s.clock.Now()
	if err := ckpt.Wait(ctx); err != nil {
		return hostapi.CaptureResult{}, fmt.Errorf("publishing the checkpoint of %s: %w", id, err)
	}
	return hostapi.CaptureResult{VM: id, Checkpoint: ckpt.Ref().Sequence, Pause: pause,
		Publish: s.since(published)}, nil
}

// boot starts one VM's VMM and registers it, which is what begins its interval
// checkpoint loop. A machine that cannot be registered is closed rather than
// left running unaccounted for.
func (s *supervisor) boot(ctx context.Context, vm *volume.VM, state []byte, template string) (*machine, error) {
	// A VM's memory regions are its volumes, and the pager's logical cap is what says
	// whether it can map them all. Asking here is what makes a create, an open
	// or a fork that could never run a refusal rather than a VMM that is
	// started and then killed part way through attaching.
	memoryRegions := make([]MemoryRegion, 0, len(vm.Volumes()))
	for _, v := range vm.Volumes() {
		memoryRegions = append(memoryRegions, memoryRegionOf(v.Name(), v.Ephemeral(), v.Size()))
	}
	if err := s.host.AdmitMemoryRegions(memoryRegions); err != nil {
		return nil, errors.Join(fmt.Errorf("starting the VMM of %s", vm.ID()), err,
			closing(ctx, vm))
	}
	process, err := vmmachine.Start(ctx, s.machineConfig(vm, state, nil))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("starting the VMM of %s", vm.ID()), err,
			closing(ctx, vm))
	}
	// A restore starts paused: the vCPUs run again once its memory regions are its own.
	if len(state) > 0 {
		if err := process.Release(ctx); err != nil {
			return nil, errors.Join(fmt.Errorf("resuming %s", vm.ID()), err, process.Close(),
				closing(ctx, vm))
		}
	}
	m := &machine{vm: vm, process: process, template: template}
	if err := s.remember(m); err != nil {
		return nil, errors.Join(err, process.Close(), closing(ctx, vm))
	}
	if err := s.host.AddMachine(vm.ID(), process); err != nil {
		s.forget(vm.ID())
		return nil, errors.Join(fmt.Errorf("registering %s", vm.ID()), err, process.Close(),
			closing(ctx, vm))
	}
	slog.InfoContext(ctx, "host: a VM is running", "vm", vm.ID(), "pid", process.PID(),
		"restored", len(state) > 0)
	return m, nil
}

// machineConfig is one VM's VMM configuration: its RAM memory region, its PMEM
// root and the VMM state a restore replays. backings is what a migration's
// destination attaches its memory regions through. Everything about the process
// itself is the Starter's.
func (s *supervisor) machineConfig(vm *volume.VM, state []byte, backings map[string]vmmemory.Backing) vmmachine.Config {
	// The root is the device the guest boots from, and every ephemeral disk the
	// VM has is a PMEM device after it, in name order.
	pmem := []vmmachine.Pmem{{ID: rootVolume, Root: true}}
	for _, v := range vm.Volumes() {
		if v.Ephemeral() {
			pmem = append(pmem, vmmachine.Pmem{ID: v.Name()})
		}
	}
	return vmmachine.Config{
		Starter: s.config.Starter, Scratch: s.scratch, Pagers: s.pagers, VM: vm,
		// A VM that records a processor count boots with it, on any host: the
		// count is in its checkpoint, not in this host's configuration.
		VCPUs:        vm.VCPUs(),
		Pmem:         pmem,
		Connection:   s.connection,
		RestoreState: state,
		Backings:     backings,
	}
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

// Delete ends one VM for good. A VM this host does not run is only a control
// record and its objects as far as this host is concerned, and those are in the
// bucket rather than here, so it is deleted all the same: a VM whose host is
// gone would otherwise be undeletable, because there is no host left to ask.
//
// The host owns the order this comes apart in: the fork points taken on this
// VM are retired before the process that holds their pages is closed, a VM
// anything still holds sealed is refused, and the handle outlives the pager
// attachments that map it.
func (s *supervisor) Delete(ctx context.Context, id string) error {
	s.forget(id)
	return s.host.Delete(ctx, id)
}

// Stop ends one VM this host runs and leaves the VM behind. The host owns what
// that costs — the last checkpoint, the order the process and the handle come
// apart in, and the refusal of a VM something still holds sealed — and this
// only stops reporting a guest that no longer exists here, once the host has
// said the stop happened. A refused stop leaves the VM running and reported.
func (s *supervisor) Stop(ctx context.Context, id string, request hostapi.StopRequest) (hostapi.StopResult, error) {
	began := s.clock.Now()
	checkpoint, err := s.host.Stop(ctx, id, request)
	if err != nil {
		return hostapi.StopResult{}, err
	}
	s.forget(id)
	return hostapi.StopResult{VM: id, Checkpoint: checkpoint.Sequence,
		Seconds: s.since(began)}, nil
}
