package vmmigrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// StartFunc builds and starts the VMM the destination resumes. It is given the
// VM this host opened, one backing per region keyed by the volume that region
// maps — each the source host that still holds those pages, with the
// destination's own volume behind it — and the VMM state the source captured.
// It returns a running process.
//
// Every backing must be the one its region attaches with, which for a
// Firecracker supervisor is vmmachine.Config.Backings. The volume stays the
// region's identity; only what the pager loads through changes. A start that
// drops these binds the destination to its own volumes, and then no fault ever
// reaches the source: the post-copy becomes a cold read of the checkpoint.
//
// The supervisor stays the caller's: this package never imports vmmachine.
type StartFunc func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (Runtime, error)

// inheritedBacking is what one region of a received VM faults through while the
// pages only its source had reach it. There are two, and they differ in nothing
// but how those pages get here: PeerBacking fetches them from the host that
// still holds them, and localBacking maps them, because that host is this one.
//
// Done means the same thing for both — the VM holds every page only its source
// had — which for a peer is the end of the fetch and for a local backing is
// true the moment the region attaches.
type inheritedBacking interface {
	vmmemory.Backing
	// Unpublished is what this region must have before the source may stop
	// serving, in the pages the handoff named. A backing that already holds
	// them names none.
	Unpublished() []PageRun
	// Unfetched is how many of them are still only on the source.
	Unfetched() int
	// Resident is the rest of what the source holds, streamed behind the
	// running guest as an optimization.
	Resident(ctx context.Context) ([]PageRun, error)
	// Concurrency is how many requests this region may have in flight at once.
	Concurrency() int
	Stats() PeerStats
	Close() error
}

// localBacking is what a fork's child attaches over when its parent runs on
// this host: the child's own volume, with the parent's sealed frames offered to
// the pager under the identity the instant gives them. Every page the child
// inherited is therefore present as a shared frame the moment the region
// attaches — no byte is copied, nothing is fetched, and the pages the parent
// holds that no checkpoint has are exactly as reachable as the published ones.
//
// The volume answers every read, as it does for any VM: the child's handle
// reads through the instant until it publishes its own root.
type localBacking struct {
	*volume.Volume
	// inherited is what the handoff named, kept for the account of it: these
	// pages are held rather than fetched, so nothing is outstanding.
	inherited []PageRun
}

// Unpublished names nothing to fetch: these pages are in this host's frames.
func (localBacking) Unpublished() []PageRun { return nil }

func (localBacking) Unfetched() int { return 0 }

// Resident is nothing to stream: there is no source to stream from.
func (localBacking) Resident(context.Context) ([]PageRun, error) { return nil, nil }

func (localBacking) Concurrency() int { return 1 }

// Stats reports the inherited pages as held: no page of them came off the wire,
// and none of them is outstanding.
func (b localBacking) Stats() PeerStats {
	held := int64(0)
	for _, run := range b.inherited {
		held += int64(run.Count)
	}
	return PeerStats{Fetched: held}
}

func (localBacking) Close() error { return nil }

// ReceiveStats reports what a destination has taken over.
type ReceiveStats struct {
	// PeerPages is what the source host served and VolumePages what this host
	// read from its own volumes and checkpoints, summed over every region.
	PeerPages, VolumePages int64
	// Streamed is how many of the source's resident pages the background stream
	// has faulted in, and Complete reports that it finished.
	Streamed int64
	Complete bool
	// Unpublished is how many pages the source held that no checkpoint has, and
	// Fetched how many of them this host now holds. The source may stop serving
	// only when the two are equal: every other page of it is in object storage
	// and this host can read it whenever it likes.
	Unpublished, Fetched int64
	// Requests is every request this VM's regions sent to the source and
	// Refusals how many of them it answered BUSY, which is a destination
	// queueing behind the source's per-peer budget rather than anything wrong.
	// A post-copy whose requests are mostly refusals is one sharing its source
	// with another destination region. Stalls is every read that went on
	// waiting for the source past a few seconds, which is a guest thread — or
	// this stream — stopped for that long.
	Requests, Refusals, Stalls int64
	// PausedAt is when the source stopped its guest and ResumedAt when this host
	// resumed it. Their difference is the migration's pause.
	PausedAt, ResumedAt time.Time
	// StreamError is why the stream stopped early, if it did. It costs the VM
	// nothing but speed: every page it did not fetch is read from the volume.
	StreamError error
}

// Received is a VM running on its destination while its pages arrive.
type Received struct {
	vm       *volume.VM
	runtime  Runtime
	backings map[string]inheritedBacking
	handoff  Handoff

	resumedAt   time.Time
	streamed    atomic.Int64
	unpublished int64
	// held closes when every page no checkpoint has is here or has failed to
	// arrive, which is what the source waits for; done closes when the bulk pass
	// behind it has finished too, which nobody waits for.
	held      chan struct{}
	heldErr   error
	done      chan struct{}
	cancel    context.CancelCauseFunc
	streamErr error
	closeOnce sync.Once
}

// VM is the destination's handle on the migrated VM.
func (r *Received) VM() *volume.VM { return r.vm }

// Runtime is the process this host started from the captured state.
func (r *Received) Runtime() Runtime { return r.runtime }

// Done reports when the source's pages have been streamed in, which is when the
// source may release its frames and the host that held them may shut down. It
// does not return until every page the source held that no checkpoint has is on
// this host or has failed to arrive: those pages exist nowhere else, so the
// source cannot stop serving while one of them is only there.
//
// A failure to fetch one of them is returned, and is the only error that says
// anything about the VM: ErrUnpublishedLost means this guest's memory is part
// this host's and part missing, which costs exactly what losing the source host
// costs, and there is nothing to publish. A failure on a page the checkpoint
// does hold costs the VM speed rather than correctness, because this host reads
// it from object storage instead.
//
// Every other error leaves a VM that is running and correct. The caller's own
// context is one of them: the stream runs on a context of this package's own,
// so a caller that gives up — an HTTP client that disconnected, a drain whose
// deadline passed — stops waiting and nothing else, and Done may be called
// again. ErrClosed is another: this Received's stream was stopped, so the pages
// it had not reached are still on the source, which is a reason not to release
// that host rather than a reason to discard this guest. Nothing but
// ErrUnpublishedLost is grounds for giving the VM up.
//
// The rest of the source's resident set is an optimization the source need not
// wait for, so Done returns while that stream is still running.
func (r *Received) Done(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-r.held:
		return r.heldErr
	}
}

// Streamed reports when the background stream has finished: the pages no
// checkpoint holds and the rest of the source's resident set both. Nothing in a
// migration has to wait for it — Done is what the source waits on — but a caller
// that wants the whole resident set here before it measures or shuts down can.
func (r *Received) Streamed(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-r.done:
		return r.streamErr
	}
}

func (r *Received) Stats() ReceiveStats {
	stats := ReceiveStats{Streamed: r.streamed.Load(), Unpublished: r.unpublished,
		PausedAt: r.handoff.PausedAt, ResumedAt: r.resumedAt}
	select {
	case <-r.done:
		stats.Complete, stats.StreamError = true, r.streamErr
	default:
	}
	for _, backing := range r.backings {
		peer := backing.Stats()
		stats.PeerPages += peer.PeerPages
		stats.VolumePages += peer.VolumePages
		stats.Requests += peer.Requests
		stats.Refusals += peer.Refusals
		stats.Stalls += peer.Stalls
		// Only the pages the source actually served are fetched. A page read from
		// this host's own volume is the checkpoint's, not the source's.
		stats.Fetched += peer.Fetched
	}
	return stats
}

// Close stops the background stream and drops the connections to the source. The
// VM keeps running: everything it has not fetched is in its own checkpoint.
func (r *Received) Close() {
	r.closeOnce.Do(func() {
		r.cancel(ErrClosed)
		<-r.done
		for _, backing := range r.backings {
			_ = backing.Close()
		}
	})
}

// Receive runs the destination's half of a live migration: it opens the VM,
// which takes the control record's epoch over, binds every region to the source
// host that still holds its pages, starts the VMM from the captured state, and
// streams the source's resident set in behind the running guest.
//
// The open reads two objects — the control record and the checkpoint index it
// selects — and no page, which is why the destination starts within a round
// trip of the source's release. The checkpoint it opens is the one the source
// published while the guest was stopped, so nothing the guest wrote is behind
// it.
func Receive(ctx context.Context, manager *volume.Manager, handoff Handoff, dial Dialer, start StartFunc, opts Options) (*Received, error) {
	if manager == nil || start == nil || dial == nil || handoff.VMID == "" || len(handoff.Regions) == 0 {
		return nil, fmt.Errorf("%w: a receive needs a manager, a handoff, a dialer and a start", ErrInvalid)
	}
	vm, err := open(ctx, manager, handoff, opts.Point)
	if err != nil {
		return nil, err
	}
	received, err := attach(ctx, vm, handoff, dial, start, opts.Point, platform.ClockOr(opts.Clock))
	if err != nil {
		// Nothing ran, so nothing is dirty and the close publishes nothing; the
		// VM goes back to whoever opens it next.
		if closeErr := vm.Close(context.WithoutCancel(ctx)); closeErr != nil {
			slog.WarnContext(ctx, "vmmigrate: releasing the VM of an abandoned receive",
				"vm", handoff.VMID, "error", closeErr)
		}
		return nil, err
	}
	return received, nil
}

// open takes the destination's handle on the VM a handoff names: the VM itself
// for a migration, and for a fork the child, created over the checkpoint its
// parent pinned. A fork's child reads that checkpoint from object storage and
// the pages written since it from the parent, and its own first checkpoint is
// the root index that makes it a VM anyone can open.
//
// point is the instant itself, for a child whose parent runs on this host: it
// carries the parent's sealed frames, which is what makes the pages written
// since that checkpoint reachable without the network. A child whose parent is
// elsewhere rebuilds the point from the published checkpoint alone.
func open(ctx context.Context, manager *volume.Manager, handoff Handoff, point *volume.ForkPoint) (*volume.VM, error) {
	if !handoff.IsFork() {
		vm, err := manager.Open(ctx, handoff.VMID)
		if err != nil {
			return nil, err
		}
		// A migration publishes nothing, so the record was openable by anybody
		// between the source's release and this open. One that selects a
		// different checkpoint has had another writer in it, and the frames this
		// handoff offers are the wrong writer's: post-copying them over that
		// checkpoint would make one VM's memory out of two writers' pages, and
		// neither side would ever say so. The handle opened here is released
		// without publishing, so what the record selects is what that writer
		// published.
		if selected := vm.Status().Checkpoint.Sequence; selected != handoff.Checkpoint {
			err := fmt.Errorf("%w: %s selects checkpoint %d, the source handed over %d",
				ErrStale, handoff.VMID, selected, handoff.Checkpoint)
			return nil, errors.Join(err, vm.Handoff(context.WithoutCancel(ctx)))
		}
		return vm, nil
	}
	if point == nil {
		rebuilt, err := manager.Inherit(ctx, control.Ref{VM: handoff.Parent, Sequence: handoff.ParentCheckpoint})
		if err != nil {
			return nil, err
		}
		point = rebuilt
	}
	return manager.Fork(ctx, handoff.VMID, point)
}

func attach(ctx context.Context, vm *volume.VM, handoff Handoff, dial Dialer, start StartFunc,
	point *volume.ForkPoint, clock platform.Clock) (*Received, error) {
	// The instant's frames are offered to this host's pager before any region
	// attaches, which is what makes every page the child inherited present
	// rather than fetched. It is the local backing's whole attach.
	if point != nil {
		if err := point.Share(ctx); err != nil {
			return nil, fmt.Errorf("offering the instant %s inherits: %w", handoff.VMID, err)
		}
	}
	backings := make(map[string]inheritedBacking, len(handoff.Regions))
	supplied := make(map[string]vmmemory.Backing, len(handoff.Regions))
	for _, region := range handoff.Regions {
		v := vm.Volume(region.Name)
		if v == nil || (v.Size() != region.Size && !sim.Bug(ctx, "migration-accept-wrong-size")) {
			return nil, fmt.Errorf("%w: %s has no volume %s of %d bytes",
				ErrInvalid, handoff.VMID, region.Name, region.Size)
		}
		var backing inheritedBacking
		if point != nil {
			backing = localBacking{Volume: v, inherited: region.Unpublished}
		} else {
			peer, err := NewPeerBacking(PeerConfig{Volume: v, Peer: handoff.Source, VM: handoff.VMID,
				PageSize: handoff.PageSize, Unpublished: region.Unpublished, Dial: dial, Clock: clock})
			if err != nil {
				return nil, err
			}
			backing = peer
		}
		backings[region.Name], supplied[region.Name] = backing, backing
	}
	drop := func() {
		for _, backing := range backings {
			_ = backing.Close()
		}
	}
	runtime, err := start(ctx, vm, supplied, handoff.State)
	if err != nil {
		drop()
		return nil, err
	}
	// A machine that does not map every region the source had is not this VM,
	// whatever it started from: the region it lacks would fault from nowhere. It
	// is closed rather than run.
	regions := runtime.Regions()
	for _, region := range handoff.Regions {
		if regions[region.Name] == nil && !sim.Bug(ctx, "migration-accept-missing-region") {
			if closeErr := runtime.Close(); closeErr != nil {
				slog.WarnContext(ctx, "vmmigrate: closing a machine that did not map every region",
					"vm", handoff.VMID, "error", closeErr)
			}
			drop()
			return nil, fmt.Errorf("%w: the started machine has no region %s", ErrInvalid, region.Name)
		}
	}
	streamCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	r := &Received{vm: vm, runtime: runtime, backings: backings, handoff: handoff,
		resumedAt: clock.Now(), held: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	// The handoff's set is final: the guest was stopped when it was taken. It is
	// counted from the handoff rather than from the backings, so that the pages
	// only the source had mean the same number whether they were fetched from
	// it or held here all along.
	for _, region := range handoff.Regions {
		for _, run := range region.Unpublished {
			r.unpublished += int64(run.Count)
		}
	}
	go r.stream(streamCtx, regions)
	return r, nil
}

// stream faults the source's pages in behind the running guest. It goes through
// the pager's own load path rather than writing pages into the region: that is
// what keeps a frame shared by lineage with every other VM on this host that
// inherited the same checkpoint, what makes a page the guest faults on first
// arrive exactly once, and what puts the pages no checkpoint has into this
// host's own dirty set, so its next interval checkpoint publishes them.
//
// The pages the source holds that no checkpoint has come first and are fetched
// to completion: they exist nowhere else, so the source cannot stop serving
// until they are here. Everything else follows as an optimization, and a page of
// it that will not arrive is simply read from object storage later.
func (r *Received) stream(ctx context.Context, regions map[string]*vmmemory.Region) {
	defer close(r.done)
	func() {
		defer close(r.held)
		r.heldErr = r.streamHeld(ctx, regions)
	}()
	r.streamErr = errors.Join(r.heldErr, r.streamResident(ctx, regions))
}

// streamHeld fetches the pages no checkpoint has. They are what Done waits for,
// so they come before any published page: a source waiting to shut down is not
// held up behind bytes it has already put in object storage. Every region is
// bound, because attach refused a machine that lacked one.
func (r *Received) streamHeld(ctx context.Context, regions map[string]*vmmemory.Region) error {
	var errs []error
	for _, info := range r.handoff.Regions {
		backing := r.backings[info.Name]
		if err := r.fetch(ctx, regions[info.Name], backing, info.Name, backing.Unpublished()); err != nil {
			errs = append(errs, err)
			continue
		}
		if left := backing.Unfetched(); left != 0 {
			errs = append(errs, fmt.Errorf("%w: %s still holds %d pages of %s",
				ErrUnpublishedLost, r.handoff.Source, left, info.Name))
		}
	}
	return errors.Join(errs...)
}

// streamResident fetches the rest of what the source holds, behind the running
// guest. Every page of it is in object storage as well, so one that will not
// arrive costs the VM speed rather than correctness.
func (r *Received) streamResident(ctx context.Context, regions map[string]*vmmemory.Region) error {
	var errs []error
	for _, info := range r.handoff.Regions {
		backing := r.backings[info.Name]
		resident, err := backing.Resident(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// The published remainder is what this pass streams, so no page is
		// faulted, or counted, twice.
		if err := r.fetch(ctx, regions[info.Name], backing, info.Name, subtractRuns(resident, backing.Unpublished())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// subtractRuns reports the pages of runs that remove does not cover. The
// unpublished set is bounded by the source's dirty budget, so holding it as a
// set costs a bounded amount however large the region is.
func subtractRuns(runs, remove []PageRun) []PageRun {
	if len(remove) == 0 {
		return runs
	}
	excluded := make(map[uint64]struct{})
	for _, run := range remove {
		for page := run.First; page < run.First+uint64(run.Count); page++ {
			excluded[page] = struct{}{}
		}
	}
	var result []PageRun
	for _, run := range runs {
		for page := run.First; page < run.First+uint64(run.Count); page++ {
			if _, found := excluded[page]; found {
				continue
			}
			if n := len(result); n > 0 && result[n-1].First+uint64(result[n-1].Count) == page {
				result[n-1].Count++
				continue
			}
			result = append(result, PageRun{First: page, Count: 1})
		}
	}
	return result
}

// fetch faults in one region's runs, as many at once as that region's backing
// holds connections for: one page at a time would pay the round trip to the
// source for every page and leave the other connections idle. The first page
// that will not load stops this region — the failure is the source's, so the
// pages behind it would fail the same way — and the fault that took it is what
// this reports.
func (r *Received) fetch(ctx context.Context, region *vmmemory.Region, backing inheritedBacking, name string, runs []PageRun) error {
	if len(runs) == 0 {
		return nil
	}
	fetchCtx, stop := context.WithCancel(ctx)
	defer stop()
	pages := make(chan uint64)
	var once sync.Once
	var failure error
	var wg sync.WaitGroup
	for range max(1, backing.Concurrency()) {
		wg.Go(func() {
			for page := range pages {
				if err := region.Fault(fetchCtx, page, false); err != nil {
					if fetchCtx.Err() == nil {
						slog.WarnContext(ctx, "vmmigrate: streaming a page from the source failed",
							"vm", r.handoff.VMID, "volume", name, "page", page, "error", err)
					}
					once.Do(func() { failure = err; stop() })
					continue
				}
				r.streamed.Add(1)
			}
		})
	}
feed:
	for _, run := range runs {
		for page := run.First; page < run.First+uint64(run.Count); page++ {
			select {
			case pages <- page:
			case <-fetchCtx.Done():
				break feed
			}
		}
	}
	close(pages)
	wg.Wait()
	if failure != nil {
		return failure
	}
	// The context this pass was given, rather than a fault, is what stopped it.
	return ctx.Err()
}
