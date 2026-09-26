package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// MigrationConfig turns on live migration for one host. Without an address
// this host neither migrates a VM away nor receives one: both halves need an
// address peers can fetch pages from.
type MigrationConfig struct {
	// Address is where this host serves migration pages: the address its page
	// server listens on, and the one a handoff tells the destination to dial.
	Address platform.Address
	// PageSize is the largest page this host serves, which is what the page
	// server's per-peer byte budgets are sized against. What a reply is counted
	// in is the page of the volume it answers for — a host's two pagers need
	// not agree — so this bounds the budgets and names nothing else. Zero
	// selects the largest page a volume may be published in.
	PageSize int
	// StartVM builds and starts the VMM of a VM this host receives. A host
	// without one can migrate its VMs away but cannot take any in. Every memory region
	// must use the same resource budget as Config.Resources; a mismatched
	// machine is closed before post-copy streaming or host registration.
	//
	// The backings it is handed are one per memory region, by volume name, and every
	// one of them must reach the machine that is started: with a Firecracker
	// supervisor that is vmmachine.Config.Backings, which attaches those memory regions
	// through the source host while their volumes stay their identity. A
	// supervisor that drops them starts a destination that faults from its own
	// checkpoint and never asks the host still holding its pages, which is a
	// post-copy in name only.
	StartVM StartFunc
	// DrainConcurrency bounds how many VMs a drain moves at once. Zero selects
	// four: a drain is planned work whose cost is one host's pages, and moving
	// every VM at once would put all of them on the network together.
	DrainConcurrency int
	// HoldTimeout is how long this host goes on serving the pages of one VM it
	// has handed over — a migration's destination or a fork's child — before it
	// gives those pages up on its own. Zero selects handoffIntervals
	// checkpoint intervals, which is the bound a deployment wants; a test that
	// stages an abandoned handover sets its own.
	HoldTimeout time.Duration
}

// ErrNotMigratable reports a host that was not configured for migration, or a
// VM it does not run.
var ErrNotMigratable = errors.New("host: this host cannot migrate that VM")

// ErrReceiving reports a receive of a VM this host is already receiving. The
// receive in flight is left alone: nothing of the refused one was started.
var ErrReceiving = fmt.Errorf("%w: that VM is already being received here", ErrNotMigratable)

// migratedHold is one VM this host has handed to another host and still serves
// the pages of: the VMM process whose pages those are, and the deadline that
// releases them when nothing ever reports the destination has them.
//
// It is the same word from the same orchestrator that ends a fork hold, and the
// same silence that leaves it open, so it has the same bound. What is left
// behind here is the source's own stopped process and its pages rather than a
// sealed parent, and releasing them costs the destination only the pages it had
// not fetched yet, which it reads from the checkpoint its record selects.
type migratedHold struct {
	runtime Machine
	// timer releases the pages when nothing reports the destination has
	// them, armed on the host's clock exactly as a fork hold's is.
	timer platform.Stopper
}

// handoffIntervals is how many checkpoint intervals a handover may be held for.
// A destination fetches the pages it inherited behind its own running guest, so
// the bound is a small multiple of the interval rather than a tight one: long
// enough that no healthy handover is cut short, short enough that a parent
// whose child is gone is durable again within it.
const handoffIntervals = 4

// HoldTimeout is how long this host serves one handover's pages for before it
// gives them up on its own. It is how long a handoff stays good: a destination
// that could not take it may be tried again, here or elsewhere, until then.
func (h *Host) HoldTimeout() time.Duration {
	if h.holdTimeout > 0 {
		return h.holdTimeout
	}
	interval := h.checkpointInterval
	if interval <= 0 {
		interval = DefaultCheckpointInterval
	}
	return handoffIntervals * interval
}

// Migrate moves one VM to another host: it stops the guest, releases the VM
// without publishing anything, and starts serving its pages to the destination.
// The returned handoff is what the deployment gives that host; this one keeps
// serving pages until ReleaseMigrated. The destination's own interval
// checkpoint is what makes the writes since this host's last checkpoint
// durable.
//
// A failure before the handoff leaves the VM running here.
func (h *Host) Migrate(ctx context.Context, vmID string, destination platform.Address) (vmmigrate.Handoff, error) {
	if h.pages == nil {
		return vmmigrate.Handoff{}, fmt.Errorf("%w: no migration endpoint is configured", ErrNotMigratable)
	}
	if destination == "" {
		return vmmigrate.Handoff{}, fmt.Errorf("%w: %s has no destination", ErrInvalidConfig, vmID)
	}
	entry, err := h.beginMigration(vmID)
	if err != nil {
		return vmmigrate.Handoff{}, err
	}
	vm := h.vm(vmID)
	if vm == nil {
		h.endMigration(entry)
		return vmmigrate.Handoff{}, fmt.Errorf("%w: %s is not open here", ErrNotMigratable, vmID)
	}
	status := vm.Status()
	if status.Sealed {
		// A memory region a fork point still has sealed cannot give its volume up, and
		// a migration that tried would abandon the checkpoint the child inherits.
		h.endMigration(entry)
		return vmmigrate.Handoff{}, fmt.Errorf("%w: %s", volume.ErrSealed, vmID)
	}
	if status.Root {
		// A fork reads its parent's sealed pages until it publishes a root index
		// of its own, and this handle is the only thing that ever could. Handing
		// it over releases it without publishing and retires nothing, so the
		// parent stays sealed for good and the child stays an identity no host can
		// open. The refusal comes before the guest is stopped for it.
		h.endMigration(entry)
		return vmmigrate.Handoff{}, fmt.Errorf("%w: %s has not published its own root index",
			volume.ErrForkPending, vmID)
	}
	if err := confirmHandoff(ctx, vm); err != nil {
		h.endMigration(entry)
		return vmmigrate.Handoff{}, err
	}
	// The checkpoint loop stops first: a checkpoint taken while the guest is being
	// stopped would seal memory regions the handoff is about to give up.
	entry.end()
	handoff, err := vmmigrate.Migrate(ctx, vm, entry.runtime, h.pages, vmmigrate.Options{})
	if err != nil {
		h.endMigration(entry)
		if errors.Is(err, vmmigrate.ErrStopped) {
			// The guest is stopped and some of its memory regions have given their
			// volumes up, so there is nothing here to run again: an interval
			// checkpoint would try to seal memory regions that no longer own what they
			// map, and the VMM process would go on running a guest no host can
			// publish. This host gives the VM up instead.
			if h.forget(vmID, entry) {
				h.discard(ctx, vmID, entry, stoppedMigrationMessage, err)
			}
			return vmmigrate.Handoff{}, err
		}
		// A failure before the handoff leaves the VM running here, so it goes on
		// being checkpointed on the interval.
		h.run(vmID, entry)
		return vmmigrate.Handoff{}, err
	}
	// The VM runs on the destination from here; this host only holds its pages,
	// under the deadline that gives them up when nothing reports the destination
	// has them.
	h.machines.mu.Lock()
	delete(h.machines.running, vmID)
	hold := &migratedHold{runtime: entry.runtime}
	hold.timer = h.clock.AfterFunc(h.HoldTimeout(), func() { h.expire(vmID) })
	h.machines.migrated[vmID] = hold
	h.machines.mu.Unlock()
	slog.InfoContext(ctx, "host: migrated a VM", "vm", vmID, "destination", destination,
		"pause_began", handoff.PausedAt)
	return handoff, nil
}

// beginMigration admits one handover of a VM at a time and reports the
// registration it claimed. A migration stops the guest, gives every memory region's
// volume up and hands the pages to a page server: two callers that found one
// registration each did all of that to one VMM process, and the loser — whose
// memory regions had already given their volumes up — gave the VM up, closing the
// process whose pages the winner's destination was about to fault out of.
//
// The claim is under the machines lock, so the second caller is told rather than
// let into the pause. It is released by endMigration on every path that leaves
// the VM here, and travels with the registration out of running on the one that
// does not.
func (h *Host) beginMigration(vmID string) (*registration, error) {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	entry := h.machines.running[vmID]
	if entry == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotMigratable, vmID)
	}
	if entry.migrating {
		return nil, fmt.Errorf("%w: %s is already being handed over", ErrNotMigratable, vmID)
	}
	entry.migrating = true
	return entry, nil
}

// endMigration gives the claim back, for a handover that did not happen and left
// the VM running here.
func (h *Host) endMigration(entry *registration) {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	entry.migrating = false
}

// confirmHandoff re-reads a VM's control record and reports whether this host
// may still hand that VM's pages to another one. It is one GET, taken before
// the pause, and it is what separates a handoff from every other thing a host
// does with a stale handle.
//
// A handoff serves pages no checkpoint holds, so the store cannot refuse a
// handed-off page the way it refuses a fenced writer's publication: whatever
// this host hands over, the destination post-copies over the checkpoint the new
// writer published, and one VM's memory ends up made of two writers' pages.
// Local handle state is no evidence — a running VM learns of a takeover on a
// timer, and a host whose reads are failing while its pod network is fine never
// learns at all — so the record itself is read, and a read that fails refuses
// the handoff rather than letting it proceed on nothing.
func confirmHandoff(ctx context.Context, vm *volume.VM) error {
	if err := vm.Confirm(ctx); err != nil {
		return fmt.Errorf("confirming %s before handing it over: %w", vm.ID(), err)
	}
	return nil
}

// Receive takes a VM over: it opens the VM the source released, or creates the
// child a fork handed over, starts the VMM from the captured state, and binds
// the pages no checkpoint holds to the backing that has them. It returns once
// those pages are here, which is what allows the source to release; the rest of
// the source's resident set keeps arriving behind the running guest until the
// caller closes the returned Received.
//
// Where the child of a fork lands changes only that backing. A child whose
// parent runs elsewhere streams the pages out of that host's page server. A
// child whose parent runs here attaches over the fork point itself: the pager
// shares the parent's sealed pages with it by identity, so every inherited
// page is present the moment the memory region attaches and nothing is fetched.
//
// A fork's child publishes its root index here, as soon as it holds every page
// its parent had — locally that is right after it attaches, and remotely it is
// the end of the post-copy. Until then nothing outside this host can open it,
// so a host lost in the meantime loses it and nothing can seal it; the
// publication is also what gives the child's own hold on the point back.
//
// A post-copy that cannot fetch those pages leaves a guest whose memory is part
// this host's and part missing, and nothing can publish it: the source has
// already given the VM up, and the pages it held exist nowhere else. Such a VM
// is discarded here rather than returned — the VMM process is closed, the handle
// released without publishing, and the supervisor told to forget it — so what a
// recovery opens is the checkpoint the control record already selects.
func (h *Host) Receive(ctx context.Context, handoff vmmigrate.Handoff) (*vmmigrate.Received, error) {
	// The fork point is this host's own for a child of a fork it took: such a child
	// is bound to the pages rather than to a peer, so it needs no page server
	// of its own and dials none.
	point := h.inherited(handoff.VMID)
	if h.migration.StartVM == nil || (h.pages == nil && point == nil) {
		return nil, fmt.Errorf("%w: this host cannot start a received VM", ErrNotMigratable)
	}
	if handoff.IsFork() && point == nil && handoff.Source == "" {
		// A child of a fork point this host was to take in over its own memory,
		// and the hold on that point is gone: it outlived its deadline, or the
		// parent was deleted or lost under it. There is nowhere left to read the
		// pages the parent held, so the child is refused rather than started over
		// a checkpoint that does not have them.
		return nil, fmt.Errorf("%w: the fork point %s inherits from %s is no longer held here",
			ErrNotMigratable, handoff.VMID, handoff.Parent)
	}
	ended, err := h.beginReceive(handoff.VMID)
	if err != nil {
		return nil, err
	}
	defer ended()
	// The handoff names every memory region and its size, so whether this host's pagers
	// could map them is known before the VM is opened and its VMM started.
	memoryRegions := make([]MemoryRegion, 0, len(handoff.MemoryRegions))
	for _, memoryRegion := range handoff.MemoryRegions {
		memoryRegions = append(memoryRegions, memoryRegionOf(memoryRegion.Name, memoryRegion.Ephemeral, memoryRegion.Size))
	}
	if err := h.AdmitMemoryRegions(memoryRegions); err != nil {
		return nil, fmt.Errorf("receiving %s: %w", handoff.VMID, err)
	}
	var started Machine
	start := func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (vmmigrate.Runtime, error) {
		runtime, err := h.migration.StartVM(ctx, vm, backings, state)
		if err != nil {
			return nil, err
		}
		if err := h.validateMachine(runtime); err != nil {
			if runtime != nil {
				err = errors.Join(err, runtime.Close())
			}
			return nil, err
		}
		started = runtime
		return runtime, nil
	}
	received, err := vmmigrate.Receive(ctx, h.volumes, handoff, h.dialPages, start,
		vmmigrate.Options{Point: point})
	if err != nil {
		return nil, err
	}
	if point != nil {
		// The child maps every page it inherited from here, which is what a
		// release of the hold kept for it states.
		h.took(handoff.VMID)
	}
	if err := h.AddMachine(handoff.VMID, started); err != nil {
		// The same VM in the same state as one whose stream never completed: a
		// guest this host started from the source's captured state and cannot
		// account for. It is given up the same way, rather than closed — which
		// would publish the half of it that arrived — and the supervisor is told.
		received.Close()
		h.discardReceived(ctx, handoff.VMID, started, received.VM(), err)
		return nil, fmt.Errorf("registering %s: %w", handoff.VMID, err)
	}
	if err := received.Done(ctx); err != nil {
		received.Close()
		h.discardReceived(ctx, handoff.VMID, started, received.VM(), err)
		return nil, fmt.Errorf("streaming %s from %s: %w", handoff.VMID, handoff.Source, err)
	}
	// The post-copy is the one part of a handover that runs behind a guest
	// already answering, so nothing else on either host says when it ended or
	// what it cost. A destination that queued behind its source's per-peer
	// budget says so here, in the refusals, and one whose reads stopped waiting
	// for seconds at a time in the stalls.
	post := received.Stats()
	slog.InfoContext(ctx, "host: post-copy complete",
		"vm", handoff.VMID, "source", handoff.Source, "fork", handoff.IsFork(),
		"unpublished_pages", post.Unpublished, "fetched_pages", post.Fetched,
		"peer_pages", post.PeerPages, "volume_pages", post.VolumePages,
		"requests", post.Requests, "refusals", post.Refusals, "stalls", post.Stalls,
		"fault_requests", post.Latency.Fault.Count,
		"fault_p50_seconds", seconds(post.Latency.Fault.QuantileUpperNS(0.5)),
		"fault_p99_seconds", seconds(post.Latency.Fault.QuantileUpperNS(0.99)),
		"fault_max_seconds", seconds(post.Latency.Fault.MaxNS),
		"fault_wait_p99_seconds", seconds(post.Latency.FaultWait.QuantileUpperNS(0.99)),
		"stream_requests", post.Latency.Stream.Count,
		"stream_p99_seconds", seconds(post.Latency.Stream.QuantileUpperNS(0.99)),
		"pause_seconds", post.ResumedAt.Sub(post.PausedAt).Seconds(),
		"seconds", h.clock.Since(post.ResumedAt).Seconds())
	if handoff.IsFork() {
		if err := h.rooted(ctx, received.VM(), started); err != nil {
			received.Close()
			h.discardReceived(ctx, handoff.VMID, started, received.VM(), err)
			return nil, fmt.Errorf("publishing the root index of %s: %w", handoff.VMID, err)
		}
	}
	return received, nil
}

// beginReceive admits one receive of a VM at a time, and reports what ends it.
// A handoff is retried when a receive of it fails, and a receive whose caller
// hung up can still be running here: its open, its start and its post-copy do
// not stop because nobody is waiting for the answer. A second receive of the
// same VM beside it would open the VM again and fence the first, and whichever
// registered its machine last would replace the other's. The second is told
// instead, and fails like any other attempt.
func (h *Host) beginReceive(vmID string) (func(), error) {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	if h.machines.receiving[vmID] {
		return nil, fmt.Errorf("%w: %s", ErrReceiving, vmID)
	}
	h.machines.receiving[vmID] = true
	return func() {
		h.machines.mu.Lock()
		defer h.machines.mu.Unlock()
		delete(h.machines.receiving, vmID)
	}, nil
}

// rooted publishes the root index of a fork's child, which is what makes it a
// VM the rest of the deployment can act on: until it lands nothing can open the
// child, so a host lost in the meantime loses it, and nothing can seal it, so it
// can be neither forked nor migrated.
//
// It happens when the child holds every page its parent had and not before: a
// root published over a guest whose memory is part missing would be an identity
// nothing could ever delete. It is a capture like any other — the child's own
// pages are sealed and published with the ones it inherited — and it ends with
// the index selected, because a child whose root is still in flight is still one
// nothing else can open.
func (h *Host) rooted(ctx context.Context, vm *volume.VM, runtime Machine) error {
	if vm == nil || !vm.Status().Root {
		// The child's interval loop got there first, which is the same root by
		// another route.
		return nil
	}
	ckpt, err := Capture(ctx, vm, runtime, h.clock, volume.Terms{})
	if err != nil {
		return err
	}
	return ckpt.Wait(ctx)
}

// discardReceived gives up a VM this host took over and could not complete the
// post-copy of. It runs on the receiving call's own goroutine, which is not one
// of the machine's, so the checkpoint loop is stopped and waited for first: a
// capture already in flight would otherwise publish the torn image this is
// discarding.
//
// Nothing is done to this host's own page server: the pages of a VM being
// received are the source's, and this host serves none of them.
//
// The handle is then handed off rather than closed, because a close publishes
// whatever is dirty, and dirty here means the half of the guest that did arrive.
// A fork's child is the exception both ways: a handoff of one is refused,
// because this handle is the only thing that could publish its root index, and
// a close of one publishes nothing either — a fork that ends before its root was
// ever taken leaves no object behind — while also retiring its hold on the
// parent's fork point, which is what gives the parent its sealed pages back.
func (h *Host) discardReceived(ctx context.Context, vmID string, runtime Machine, vm *volume.VM, cause error) {
	ctx, cancel := cleanup(ctx)
	defer cancel()
	h.RemoveMachine(vmID)
	errs := []error{runtime.Close()}
	if vm != nil && vm.Status().Root {
		errs = append(errs, vm.Close(ctx))
	} else if vm != nil {
		errs = append(errs, vm.Handoff(ctx))
	}
	slog.ErrorContext(ctx, "host: discarded a received VM whose post-copy never completed",
		"vm", vmID, "cause", cause, "error", errors.Join(errs...))
	if h.closeMachine != nil {
		h.closeMachine(vmID)
	}
}

// ReleaseMigrated stops serving one migrated VM's pages and closes the process
// that held them. It is what the destination's completion report allows, and
// not before: until the destination reports Done, this host holds pages of that
// VM that no checkpoint has, and releasing them would lose the guest's writes
// since this host's last checkpoint. Once every migrated VM is released,
// Status().Serving is empty and this host may exit. A VM this host never
// migrated away is not touched.
// It is refused while the page server still holds pages of that VM that no
// checkpoint has and the destination has not fetched. What the control plane's
// table says about the migration is not evidence of that: the page server
// answered the fetches, so it is the one thing that knows, and a release it
// refuses leaves the VM exactly as it was and still serving. A child of a fork
// this host takes in itself fetches nothing, and this host is the one thing that
// knows whether it has been taken in: until it has, its release is refused the
// same way.
func (h *Host) ReleaseMigrated(vmID string) error { return h.release(vmID, false) }

// Abandon is ReleaseMigrated for a VM this host is giving up rather than
// handing over: a fork hold that outlived its deadline, a fan-out that failed,
// a VM a later writer fenced this host out of, a parent being deleted, a host
// that is exiting. The pages go either way, and refusing would only leave the
// parent sealed and the VM half-released.
func (h *Host) Abandon(vmID string) error { return h.release(vmID, true) }

func (h *Host) release(vmID string, abandoning bool) error {
	// What can refuse goes first: nothing below may be undone for a release
	// that does not happen. A child this host takes in itself is refused here
	// until it has been, and one elsewhere by the page server.
	if !abandoning {
		if err := h.untaken(vmID); err != nil {
			return fmt.Errorf("releasing %s: %w", vmID, err)
		}
	}
	if h.pages != nil {
		if abandoning {
			h.pages.Discard(vmID)
		} else if err := h.pages.Release(vmID); err != nil {
			return fmt.Errorf("releasing %s: %w", vmID, err)
		}
	}
	h.machines.mu.Lock()
	migrated := h.machines.migrated[vmID]
	delete(h.machines.migrated, vmID)
	hold := h.machines.forked[vmID]
	delete(h.machines.forked, vmID)
	h.machines.mu.Unlock()
	// The deadline this release beat has nothing left to do.
	if migrated != nil {
		migrated.timer.Stop()
	}
	if hold != nil {
		// The deadline this release beat has nothing left to do. A fork's child
		// has every page it inherited, so the parent takes its sealed pages
		// back and is checkpointed again; its VMM process is untouched, because
		// the parent never stopped.
		hold.timer.Stop()
		return hold.point.Retire(context.Background())
	}
	// Its checkpoint loop stopped when it was migrated away; this only releases
	// what that handoff left behind.
	if migrated == nil {
		return nil
	}
	return migrated.runtime.Close()
}

// expire releases a handover whose deadline has passed: a fork hold nothing
// said the child had the pages of, or a migration nothing said the destination
// had them. This host stops offering those pages and gives up what it held for
// them.
//
// For a fork that is the parent's pages: a parent that stays sealed is one
// nothing can checkpoint, fence or migrate, and whose dirty set only grows,
// which costs far more than a child that has to fall back to the checkpoint it
// was forked from. For a migration it is the source's own stopped VMM process,
// which nothing will ever run again, and the pages it maps.
func (h *Host) expire(vmID string) {
	h.machines.mu.Lock()
	held := h.machines.forked[vmID] != nil || h.machines.migrated[vmID] != nil
	h.machines.mu.Unlock()
	if !held {
		return
	}
	slog.WarnContext(h.ctx, "host: a handover outlived its deadline and was released",
		"vm", vmID, "deadline", h.HoldTimeout())
	if err := h.Abandon(vmID); err != nil {
		slog.ErrorContext(h.ctx, "host: releasing an expired handover failed",
			"vm", vmID, "error", err)
	}
}

// stopHolds disarms every deadline this host is still keeping, which is what a
// host that is shutting down does before its VM handles go: the release those
// deadlines would drive has nothing left to give back.
func (h *Host) stopHolds() {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	for _, hold := range h.machines.forked {
		hold.timer.Stop()
	}
	for _, hold := range h.machines.migrated {
		hold.timer.Stop()
	}
}

// dialPages reaches the page server named by a handoff. The cluster network is
// trusted, so the address is the whole of what a destination needs.
func (h *Host) dialPages(ctx context.Context, peer platform.Address) (platform.Conn, error) {
	return h.network.Dial(ctx, "", peer)
}

// seconds is a histogram's nanoseconds as the seconds a log line reports.
func seconds(ns uint64) float64 { return time.Duration(ns).Seconds() }
