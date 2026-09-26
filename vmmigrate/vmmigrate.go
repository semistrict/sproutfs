// Package vmmigrate moves a running VM between hosts with a pause of the VMM
// state capture and nothing else. It is post-copy only: nothing is uploaded
// inside the pause.
//
// The source stops the guest and releases the VM without publishing; the
// destination opens it, which advances the epoch and reads the index of the
// checkpoint the control record already selects, and resumes from the captured
// VMM state. Everything the guest wrote since the source's last checkpoint
// exists only in the source's pages, and the destination faults it out of them
// over the same network the hosts already share. PageSource serves those pages,
// PeerBacking fetches them, and both give up as soon as the source says it no
// longer serves that VM. A page the source served out of its own dirty state is
// dirty on the destination too, so the destination's next interval checkpoint
// is what makes it durable.
//
// Migrate runs the source's half — stop, hand the VM over, serve the pages —
// and Receive runs the destination's — open the VM, start the VMM from the
// captured state, and stream the source's resident set in behind the running
// guest.
package vmmigrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

var (
	// ErrInvalid reports a missing VM, process, page source or memory region.
	ErrInvalid = errors.New("vmmigrate: invalid argument")
	// ErrStopped reports a migration that stopped the guest and could not hand
	// the VM over. This process cannot resume it: its memory regions have already given
	// their volumes up. The VM is reopened, here or anywhere else, from the
	// checkpoint its control record selects.
	ErrStopped = errors.New("vmmigrate: the guest was stopped and cannot resume")
	// ErrClosed reports a page source or a post-copy stream that has stopped.
	ErrClosed = errors.New("vmmigrate: closed")
	// ErrOutstanding reports a release refused because the destination has not
	// fetched every page this host holds that no checkpoint of the VM has.
	// Those pages exist nowhere else: releasing them would lose the guest's
	// writes since this host's last checkpoint. The source knows which of them
	// it has answered for, so this is the evidence, rather than whatever a
	// control plane's table believes about the migration.
	ErrOutstanding = errors.New("vmmigrate: unpublished pages are still outstanding")
	// ErrStale reports a handoff whose VM's control record no longer selects the
	// checkpoint the source handed over. A migration publishes nothing, so the
	// record is openable by anybody between the source's release and this open,
	// and a writer that got in has published over it. Post-copying the source's
	// pages on top would make one VM's memory out of two writers' pages, so the
	// handoff is refused instead.
	ErrStale = errors.New("vmmigrate: the VM's record has moved past this handoff")
)

// Runtime is one VMM process a migration drives, and is satisfied by
// *vmmachine.Process. MemoryRegions names every memory region by the volume it maps,
// which are the names the destination opens the same volumes under.
type Runtime interface {
	// MemoryRegions reports the memory regions by volume name.
	MemoryRegions() map[string]*vmmemory.MemoryRegion
	// Stop pauses the vCPUs, drains device completions and returns the VMM
	// state with the process left paused. It seals nothing and waits for
	// nothing: the pages it leaves behind are what the destination fetches.
	Stop(context.Context) ([]byte, error)
	// Resume restarts the vCPUs.
	Resume(context.Context) error
	// Release unseals every memory region and resumes a process an earlier phase left
	// paused, which is how an abandoned migration gives the VM back.
	Release(context.Context) error
	// Close ends the process.
	Close() error
}

// MemoryRegionInfo names one memory region of a migrated VM and the size of the volume it
// maps, so the destination can bind the same layout.
type MemoryRegionInfo struct {
	Name string
	Size uint64
	// Ephemeral marks an ephemeral disk, one no checkpoint holds. Every page of
	// it the source holds is in Unpublished. A fork's child is handed none of
	// them: it gets the disk zeroed.
	Ephemeral bool `json:",omitempty"`
	// Unpublished names the pages of this memory region that no checkpoint of the VM has:
	// the guest's writes since the source's last checkpoint. They exist only in
	// the source's pages, so the destination must not read them from its own
	// volume — which reports them as the checkpoint's bytes or as holes — and must
	// fetch every one of them before the source may stop serving. The guest was
	// stopped when this was taken, so it is final.
	//
	// The set is bounded by the source's dirty budget, which is what makes it
	// plain data the control plane can carry.
	Unpublished []PageRun
	// UnpublishedAge is how long the source had held the oldest of those pages
	// when it gave the VM up, zero where it held none. The destination dates the
	// pages it receives from it, on its own clock, so the VM's loss window
	// carries across the handoff instead of restarting: a VM handed from host to
	// host would otherwise never reach a bound at all.
	UnpublishedAge time.Duration `json:",omitempty"`
}

// Handoff is what the source gives the deployment to start the VM elsewhere. It
// is plain data: the control plane carries it to the destination host.
//
// A fork's handoff is the same thing from a parent that keeps running: VMID is
// the child the destination creates rather than a VM the source released, and
// Parent names the checkpoint it inherits. Everything else — the state, the
// layout, the unpublished runs and the address they are served from — means
// exactly what it does for a migration.
type Handoff struct {
	// VMID is the VM the source released, and State the captured VMM state the
	// destination restores. The checkpoint the destination opens is whatever the
	// VM's control record selects, which is the source's last interval checkpoint;
	// the writes since it come from the source's pages.
	VMID  string
	State []byte
	// Checkpoint is the sequence the source's control record selected when it
	// gave the VM up, and is what says the destination is taking over the same
	// VM the source released. Nothing is published by a migration, so that record
	// is openable by anybody in between; a destination that opens a record
	// selecting anything else refuses the handoff rather than post-copying the
	// source's pages over another writer's checkpoint. It is zero for a fork,
	// whose child has no record until the destination creates it.
	Checkpoint uint64 `json:",omitempty"`
	// Parent and ParentCheckpoint make this handoff a fork: the VM the child
	// inherits, and the sequence of the last checkpoint that VM published, which
	// it pinned in its own control record before the handoff. They are empty for
	// a migration, where the VM that moves is the VM that already existed.
	Parent           string `json:",omitempty"`
	ParentCheckpoint uint64 `json:",omitempty"`
	// Source is where this VM's pages are still served from, and PageSize the
	// page they are served in.
	Source   platform.Address
	PageSize int
	// MemoryRegions is the memory layout, in ascending name order.
	MemoryRegions []MemoryRegionInfo
	// PausedAt is when the guest stopped, which with the destination's resume
	// bounds the pause the migration cost.
	PausedAt time.Time
}

// IsFork reports a handoff whose VM is a child of a parent that keeps running,
// rather than a VM its source gave up.
func (h Handoff) IsFork() bool { return h.Parent != "" }

// Options bounds a migration.
type Options struct {
	// Source overrides the address the destination fetches pages from, for a
	// deployment whose page server is reached through another name than the one
	// it listens on. It defaults to the page source's own address.
	Source platform.Address
	// Clock is what the handoff's pause and the destination's resume are
	// stamped with, and what a destination's retry against a busy source waits
	// on. Nil is the wall clock.
	Clock platform.Clock
	// Point is what a Receive of a fork's child binds to when that child's
	// parent runs on this host: the point the parent was sealed at. The child
	// is created from it and its memory regions attach over a local backing of it, so
	// every page it inherited is present the moment the memory region attaches and
	// nothing is fetched. It is nil for every other receive — a migration, or a
	// child whose parent is elsewhere — which rebuilds the point from the
	// checkpoint the parent pinned and pulls those pages out of its page server.
	Point *volume.ForkPoint
}

// Migrate runs the source's half of a live migration.
//
// It stops the guest, gives the memory regions' volumes up while keeping their pages,
// releases the VM without publishing, and registers the memory regions with the page
// source so the destination can fault from them. The returned handoff is what
// the destination needs and nothing more.
//
// Nothing is uploaded here. The pages the guest wrote since this host's last
// interval checkpoint are in the pages the memory regions keep, and the destination
// pulls them out of those pages and publishes them in its own next checkpoint.
// The exposure is the source dying during the post-copy, which loses those
// writes exactly as any host loss does.
//
// A failure before the handoff leaves the VM running on this host: nothing was
// released. A failure after the memory regions gave their volumes up cannot resume the
// guest — it reports ErrStopped.
func Migrate(ctx context.Context, vm *volume.VM, process Runtime, source *PageSource, opts Options) (Handoff, error) {
	if vm == nil || process == nil || source == nil {
		return Handoff{}, ErrInvalid
	}
	memoryRegions := process.MemoryRegions()
	if len(memoryRegions) == 0 {
		return Handoff{}, fmt.Errorf("%w: %s maps no memory region", ErrInvalid, vm.ID())
	}
	names := make([]string, 0, len(memoryRegions))
	for name := range memoryRegions {
		names = append(names, name)
	}
	slices.Sort(names)
	layout := make([]MemoryRegionInfo, 0, len(names))
	for _, name := range names {
		v := vm.Volume(name)
		if v == nil {
			return Handoff{}, fmt.Errorf("%w: memory region %q maps no volume of %s", ErrInvalid, name, vm.ID())
		}
		layout = append(layout, MemoryRegionInfo{Name: name, Size: v.Size(), Ephemeral: v.Ephemeral()})
	}
	address := opts.Source
	if address == "" {
		address = source.Address()
	}

	// The checkpoint the record selects is read before the guest stops, because
	// from here nothing this handle holds can advance it: the handoff publishes
	// nothing. It is what the destination checks the record it opens against.
	selected := vm.Status().Checkpoint.Sequence

	clock := platform.ClockOr(opts.Clock)
	paused := clock.Now()
	state, err := process.Stop(ctx)
	if err != nil {
		return Handoff{}, resume(ctx, process, err)
	}
	// Giving the volumes up is local and comes first: from here the memory regions
	// serve their pages without touching a VM that is about to be another
	// host's. A memory region a checkpoint still has sealed keeps its volume and
	// reports that here, which is a migration that has not happened.
	for index, name := range names {
		age, err := memoryRegions[name].Handoff(ctx)
		if err != nil {
			if index == 0 {
				return Handoff{}, resume(ctx, process, err)
			}
			return Handoff{}, errors.Join(ErrStopped, err)
		}
		// The loss window travels with the pages: the destination dates what it
		// receives from this age rather than from its own arrival, so a VM
		// cannot outrun the bound by being handed on.
		layout[index].UnpublishedAge = age
		// The guest is stopped and this memory region's volume is given up, so its
		// unpublished set can no longer change: it is exactly what the
		// destination must fetch out of these pages. A memory region that cannot
		// list it stops the migration here rather than handing over a layout
		// that names no pages to fetch, which the destination would honour by
		// rewinding the guest to the last checkpoint.
		unpublished, err := memoryRegions[name].Unpublished()
		if err != nil {
			return Handoff{}, errors.Join(ErrStopped, err)
		}
		layout[index].Unpublished = runsOf(unpublished)
	}
	if err := vm.Handoff(ctx); err != nil {
		return Handoff{}, errors.Join(ErrStopped, err)
	}
	source.Serve(vm.ID(), MemoryRegionPages(memoryRegions))
	return Handoff{VMID: vm.ID(), State: state, Checkpoint: selected,
		Source: address, PageSize: source.PageSize(), MemoryRegions: layout,
		PausedAt: paused}, nil
}

// Fork runs the parent's half of a fork whose child runs on another host.
//
// The pause already happened: point is the fork point host.Seal took, and the
// parent has been running again since. Nothing is stopped, nothing gives its
// volume up and nothing is published — the parent keeps its handle, its memory regions
// and its pages. This only registers the pages the child must fetch and
// describes them, which is the whole difference between a fork and a migration
// on the wire.
//
// The parent serves those pages under the child's identity until the child
// reports every one of them received, exactly as a migration's source does: a
// PageSource.Release taken before that is refused with ErrOutstanding, because
// the source is the one thing that knows which pages it has actually answered
// for. Giving the child up rather than handing it over — a hold that outlived
// its deadline, a fan-out that failed, a parent this host lost — goes through
// Discard instead, which refuses nothing: those pages are going either way, and
// a refusal would only leave the parent sealed and the child half-released.
//
// A child taken in on this same host is served nothing: it attaches over the
// point itself, so no page of it ever reaches the wire. Such a fork passes no
// page source, and the handoff it returns names no address to fetch from.
func Fork(ctx context.Context, child string, point *volume.ForkPoint, source *PageSource, opts Options) (Handoff, error) {
	if child == "" || point == nil {
		return Handoff{}, fmt.Errorf("%w: a fork handoff needs a child and a point", ErrInvalid)
	}
	if err := context.Cause(ctx); err != nil {
		return Handoff{}, err
	}
	parent := point.Parent()
	names := point.Volumes()
	layout := make([]MemoryRegionInfo, 0, len(names))
	for _, name := range names {
		layout = append(layout, MemoryRegionInfo{Name: name, Size: point.Size(name),
			Ephemeral:   point.Ephemeral(name),
			Unpublished: runsOf(point.Pages(name)), UnpublishedAge: point.UnpublishedAge(name)})
	}
	handoff := Handoff{VMID: child, State: point.State(),
		Parent: parent.VM, ParentCheckpoint: parent.Sequence, MemoryRegions: layout,
		PausedAt: platform.ClockOr(opts.Clock).Now()}
	if source == nil {
		return handoff, nil
	}
	handoff.Source, handoff.PageSize = source.Address(), source.PageSize()
	if opts.Source != "" {
		handoff.Source = opts.Source
	}
	source.Serve(child, ForkPages(point))
	return handoff, nil
}

// runsOf groups ascending page numbers into runs, which is how a handoff and a
// page listing both carry a set of pages.
func runsOf(pages []uint64) []PageRun {
	var runs []PageRun
	for _, page := range pages {
		if n := len(runs); n > 0 && runs[n-1].First+uint64(runs[n-1].Count) == page {
			runs[n-1].Count++
			continue
		}
		runs = append(runs, PageRun{First: page, Count: 1})
	}
	return runs
}

// resume gives an abandoned migration's VM back: it unseals every memory region and
// restarts a guest an earlier phase paused. The migration's own failure is what
// the caller needs, so the release's failure is joined to it rather than
// replacing it.
func resume(ctx context.Context, process Runtime, cause error) error {
	if sim.Bug(ctx, "migration-skip-resume") {
		// The source is left sealed or paused by an abandoned migration, so
		// its guest never runs again and its pages answer nothing.
		return cause
	}
	if err := process.Release(context.WithoutCancel(ctx)); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// ProbeVolumeFallback marks a destination memory region giving up on the source host
// and reading the rest of its pages from its own volume. It is correct only
// because every page no checkpoint holds is refused rather than substituted,
// which is exactly what a run that never falls back never checks.
const ProbeVolumeFallback = "vmmigrate/volume-fallback"
