package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// Machine exclusively controls one VMM process lifetime: every lifecycle
// command for that process goes through it. It is what a migration drives, what
// the interval checkpoint captures and what a deliberate stop ends, and
// *vmmachine.Process is one. A handoff needs only part of it, which is
// vmmigrate.Runtime; this interface satisfies that.
//
// Prepare, Resume and Release are the phases of one capture. Prepare pauses the
// vCPUs, drains device completions, captures the VMM state and seals every
// memory region, returning each memory region's sealed checkpoint by the name of the
// volume it maps; sealing records the checkpoint without moving a byte, so
// Resume can restart the guest immediately while the checkpoint uploads those
// pages behind it. Release unseals and resumes a VM still paused because an
// earlier phase failed; a capture that got as far as its publication does not
// use it, because the publication owns the checkpoints and retires them itself.
// A migration uses the same Resume and Release, which are the same call either
// way.
type Machine interface {
	// MemoryRegions reports the memory regions by volume name.
	MemoryRegions() map[string]*vmmemory.MemoryRegion
	// Prepare pauses the process and returns its captured VMM state together
	// with the sealed checkpoint of every memory region, by volume name.
	Prepare(ctx context.Context) ([]byte, map[string]volume.DirtySource, error)
	// SealDisks pauses the process's vCPUs and seals the memory regions of its disks,
	// leaving its RAM as it is and capturing no VMM state, and returns those
	// memory regions' checkpoints by volume name. It is the pause of a disk
	// checkpoint: the process stays paused until Resume, and Release unseals
	// what it sealed and resumes it.
	SealDisks(ctx context.Context) (map[string]volume.DirtySource, error)
	// Stop pauses the vCPUs, drains device completions and returns the VMM
	// state with the process left paused. It seals nothing and waits for
	// nothing: the pages it leaves behind are what a destination fetches.
	Stop(ctx context.Context) ([]byte, error)
	// Resume restarts the vCPUs, with every memory region still sealed if a
	// capture sealed them.
	Resume(ctx context.Context) error
	// Release unseals every memory region and resumes a still-paused process.
	Release(ctx context.Context) error
	// Wait reports the end of the VMM process: the cause it was killed with, or
	// the status of a process that ended on its own. It returns only once the
	// process is gone, so it is what tells this host that the guest behind a
	// machine it runs no longer exists.
	Wait(ctx context.Context) error
	// Close ends the process.
	Close() error
}

// Machine is exactly what a handoff drives, plus the capture and the watch.
var _ vmmigrate.Runtime = Machine(nil)

// StartFunc builds and starts the VMM of a VM this host receives. It is
// vmmigrate.StartFunc returning a Machine, because a received VM is checkpointed
// on this host's interval like any other.
type StartFunc func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (Machine, error)

// registration is one registered VMM process and what this host runs for it: the
// interval loop that checkpoints it, and the watcher on the process itself.
// stop ends both and done closes when both have returned; they are nil for a VM
// this host only serves pages for. mu guards the pair, because a machine is
// stopped by whoever gives the VM up and two of them can arrive at once: a
// migration and the epoch timer, or the loop itself finding the same takeover.
//
// now asks that loop for a checkpoint out of the interval's turn, which is what
// the pager's dirty-budget pressure reaches it through; it is set with the loop
// and never replaced, so the pressure can read it under the machines lock.
//
// flushes are the guest's flushes waiting for a checkpoint of its disks, guarded
// by mu. A VM that leaves this host — migrated, stopped, given up — takes them
// with it unanswered: its VMM is paused or gone, and a flush it completed now
// would write into memory the VM's next host already owns.
type registration struct {
	runtime Machine
	mu      sync.Mutex
	stop    context.CancelFunc
	done    chan struct{}
	now     chan struct{}
	flushes []pendingFlush
	// migrating is set while a handover of this VM is in flight and is guarded
	// by the machines lock, not mu: it is what admits one of them at a time.
	migrating bool
}

// machines is what this host runs: the VMM process of every VM its manager
// holds, the ones it has already handed to another host and is still serving
// pages for, and the fork holds it keeps for children running elsewhere.
// fenced names the VMs it has already given up, so two discoveries of one loss
// — the checkpoint loop and the epoch timer finding one takeover, or either of
// them and the watcher finding the VMM dead — close that VM once.
type machines struct {
	mu       sync.Mutex
	running  map[string]*registration
	migrated map[string]*migratedHold
	forked   map[string]*forkHold
	fenced   map[string]bool
	// stopping is the VMs this host is stopping because a pager ran out of a
	// bound, from the moment it forgets them until their processes are closed.
	// Their memory regions still hold pages until then, and a pager asking to
	// stop one of them again is told it will be.
	stopping map[*registration]string
}

// AddMachine registers the VMM process of a VM this host runs, which is what a
// drain migrates, what a page server serves from, and what the interval loop
// checkpoints. The supervisor keeps ownership: this only records which
// process belongs to which VM and starts that VM's checkpoint loop.
func (h *Host) AddMachine(vmID string, runtime Machine) error {
	if vmID == "" {
		return ErrInvalidConfig
	}
	if err := h.validateMachine(runtime); err != nil {
		return err
	}
	// A machine this host already runs is ended outside the lock: ending it
	// waits for goroutines that take this lock themselves when what they find
	// is a VM to give up.
	h.machines.mu.Lock()
	existing := h.machines.running[vmID]
	delete(h.machines.running, vmID)
	h.machines.mu.Unlock()
	if existing != nil {
		existing.end()
	}
	entry := &registration{runtime: runtime}
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	// This identity is being run again — received back, or created anew after a
	// deletion — so whatever fenced it before is history.
	delete(h.machines.fenced, vmID)
	h.run(vmID, entry)
	h.machines.running[vmID] = entry
	return nil
}

// run starts what this host runs for one registered VM: the watcher on its VMM
// process, and the interval checkpoint loop, which a host with no interval
// configured does not have. done closes once both have returned, so ending the
// machine waits for the pair; a machine with no loop has no out-of-turn
// checkpoint to ask for either, so now stays nil for it.
func (h *Host) run(vmID string, entry *registration) {
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	entry.mu.Lock()
	entry.stop, entry.done = cancel, done
	if h.checkpointInterval > 0 {
		entry.now = make(chan struct{}, 1)
	}
	entry.mu.Unlock()
	var running sync.WaitGroup
	running.Go(func() { h.awaitingExit(ctx, cancel, vmID, entry) })
	if h.checkpointInterval > 0 {
		running.Go(func() { h.checkpointing(ctx, vmID, entry) })
	}
	go func() { running.Wait(); close(done) }()
}

// awaitingExit gives up a VM whose VMM process has ended without this host
// asking it to: the host killed it because its memory session failed, the kernel
// killed it, or it crashed. Nothing else would notice — the checkpoint loop
// wakes only on its interval, and what it finds then is a socket that refuses
// the connection, which it logs and retries for as long as the host runs, while
// the supervisor goes on reporting a guest that no longer exists. ctx ends with
// the machine, so a process this host stops on purpose — a handoff, a removal, a
// deliberate stop, a shutdown — is not reported as a death.
//
// It runs on a goroutine the machine's own done waits for, so it stops the
// checkpoint loop rather than ending the machine, which would wait for this.
func (h *Host) awaitingExit(ctx context.Context, cancel context.CancelFunc, vmID string, entry *registration) {
	err := entry.runtime.Wait(ctx)
	if ctx.Err() != nil {
		return
	}
	cancel()
	// Whoever dropped the registration owns giving this VM up, and one takeover
	// and one death must still close it once between them.
	if !h.forget(vmID, entry) || !h.claimFence(vmID) {
		return
	}
	h.discard(ctx, vmID, entry, "host: gave up a VM whose VMM process ended", err)
}

// end stops everything this host runs for a VM and waits for the capture its
// loop may have in flight, publication and all, so nothing checkpoints a VM this
// host has given up and nothing still holds its memory regions sealed when it returns.
// It is idempotent and safe to call from two callers at once, which is what a
// migration racing the epoch timer does. It must not be called from either of
// those goroutines: it waits for both to return.
func (m *registration) end() {
	m.mu.Lock()
	stop, done := m.stop, m.done
	m.stop, m.done = nil, nil
	m.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-done
}

// discard gives up everything this host holds of a VM it can no longer run:
// another writer has taken its control record, or its VMM process has ended and
// there is no guest left to run. cause is what ended it, which with message is
// the only account of it anything gets.
//
// The fork points taken on this VM go first: retiring them stops the children
// this host serves that point's pages to and gives the memory regions back before the
// process that maps them is closed. Then the page server drops the VM, the VMM
// process is stopped, the VM handle is released — a fenced handle publishes
// nothing on close — and the supervisor that owns them both is told.
//
// Its caller has already dropped the registration and claimed the close, so this
// runs once for a VM however this host lost it.
func (h *Host) discard(ctx context.Context, vmID string, entry *registration, message string, cause error) {
	// The closes outlive a cancellation that was this VM's own, under a deadline
	// of their own: a store having a bad minute must not hold this open for the
	// life of the process.
	ctx, cancel := cleanup(ctx)
	defer cancel()
	errs := h.retireForks(vmID)
	if h.pages != nil {
		h.pages.Discard(vmID)
	}
	if entry != nil {
		errs = append(errs, entry.runtime.Close())
	}
	if vm := h.vm(vmID); vm != nil {
		errs = append(errs, vm.Close(ctx))
	}
	slog.WarnContext(ctx, message, "vm", vmID, "cause", cause, "error", errors.Join(errs...))
	if h.closeMachine != nil {
		h.closeMachine(vmID)
	}
}

// retireForks retires every fork point taken on one VM. It comes before
// anything closes the process whose memory those points are: a child reads
// that point out of it, and retiring the point stops this host offering
// them, so the child's next fault for one fails and says so rather than reading
// the checkpoint's older bytes as though they were the point's. It also gives
// the VM's own pages back, which is what lets a last capture of it happen at
// all.
func (h *Host) retireForks(vmID string) []error {
	children := h.forkedFrom(vmID)
	errs := make([]error, 0, len(children)+3)
	for _, child := range children {
		errs = append(errs, h.Abandon(child))
	}
	return errs
}

// stopped ends one VM deliberately, checkpointing before it closes anything.
// The pages the stall is about live in the VMM's memory regions, which closing the
// process detaches, and only a capture publishes them; the handle's own close
// publishes the overlay alone. A VMM the failed fault has already killed can no
// longer be paused for one, and then the stop keeps nothing the kill would have
// kept either — but it says so, which the kill does not.
//
// Everything after that capture is the give-up every other loss of a VM goes
// through, and for the same reasons: this host is as done with the VM as a
// fenced or a dead one leaves it. Its caller has claimed the close, so a stall
// and a death that find the same VM close it once between them.
func (h *Host) stopped(vmID string, entry *registration, cause error) {
	entry.end()
	ctx := context.WithoutCancel(h.ctx)
	// The fork points taken on this VM go first: a VM one of them still holds
	// sealed cannot be captured at all, so a stop that left them would publish
	// nothing of what it could still have kept.
	errs := h.retireForks(vmID)
	if vm := h.vm(vmID); vm != nil {
		if checkpoint, err := CaptureDisks(ctx, vm, entry.runtime, h.clock); err != nil {
			errs = append(errs, err)
		} else {
			errs = append(errs, checkpoint.Wait(ctx))
		}
	}
	if err := errors.Join(errs...); err != nil {
		slog.ErrorContext(ctx, "host: the last checkpoint of a stalled VM failed",
			"vm", vmID, "error", err)
	}
	h.discard(ctx, vmID, entry, stalledMessage, cause)
	h.machines.mu.Lock()
	delete(h.machines.stopping, entry)
	h.machines.mu.Unlock()
}

// machineFor finds the VM whose VMM maps one of the pager's memory regions, which is
// how dirty-budget pressure on a memory region reaches the loop that can relieve it.
func (h *Host) machineFor(memoryRegion *vmmemory.MemoryRegion) (string, *registration) {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	for vmID, entry := range h.machines.running {
		for _, mapped := range entry.runtime.MemoryRegions() {
			if mapped == memoryRegion {
				return vmID, entry
			}
		}
	}
	return "", nil
}

// forget drops one registration, reporting whether this caller is the one that
// dropped it: two stalled stores of the same VM must not stop it twice.
func (h *Host) forget(vmID string, entry *registration) bool {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	if h.machines.running[vmID] != entry {
		return false
	}
	delete(h.machines.running, vmID)
	return true
}

// validateMachine checks the actual mapped memory regions, rather than trusting an
// additional budget declaration from the process adapter.
func (h *Host) validateMachine(runtime vmmigrate.Runtime) error {
	if runtime == nil || h.resources == nil {
		return ErrInvalidConfig
	}
	memoryRegions := runtime.MemoryRegions()
	if len(memoryRegions) == 0 {
		return fmt.Errorf("%w: machine has no memory regions", ErrInvalidConfig)
	}
	for name, memoryRegion := range memoryRegions {
		if memoryRegion.Resources() != h.resources {
			return fmt.Errorf("%w: memory region %s does not use the host resource budget", ErrInvalidConfig, name)
		}
	}
	return nil
}

// RemoveMachine forgets a VM this host no longer runs and stops its checkpoint
// loop. It does not close the process or release any pages.
func (h *Host) RemoveMachine(vmID string) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	delete(h.machines.running, vmID)
	h.machines.mu.Unlock()
	if entry != nil {
		entry.end()
	}
}

// Delete gives up a VM this host runs for good: its checkpoint loop stops, its
// VMM process is closed, its handle is released and its control record is
// removed, after which nothing can open it. The checkpoint objects go with the
// record, except the ones the record pinned — the fork points this VM was forked
// at, whose checkpoints a descendant may still read — and those are a collector's.
//
// A VM this host does not run is only its control record here, which is removed
// all the same: a delete is the deployment saying the VM is over.
//
// The fork points taken on this VM go first, for the reason a discard retires
// them first: a child running elsewhere reads that point's pages out of the
// memory the process about to be closed maps. Retiring the point stops this
// host offering them, so the child's next fault for one fails and says so
// rather than reading the checkpoint's older bytes as though they were the
// point's.
//
// A VM something still holds sealed after that is refused, exactly as a
// migration of one is, and left running: the holder is reading the pages this
// would detach, and it is not one of this host's own holds — a child created
// here whose root index has not published yet holds the point through its own
// handle, and a capture in flight holds it through the publication. Deleting it
// anyway would take the point out from under whoever has it.
func (h *Host) Delete(ctx context.Context, vmID string) error {
	errs := h.retireForks(vmID)
	if vm := h.vm(vmID); vm != nil && vm.Status().Sealed {
		return errors.Join(append(errs, fmt.Errorf("%w: %s", volume.ErrSealed, vmID))...)
	}
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	delete(h.machines.running, vmID)
	h.machines.mu.Unlock()
	if entry != nil {
		entry.end()
	}
	if h.pages != nil {
		h.pages.Discard(vmID)
	}
	if entry != nil {
		errs = append(errs, entry.runtime.Close())
	}
	if vm := h.vm(vmID); vm != nil {
		errs = append(errs, vm.Close(ctx))
	}
	return errors.Join(append(errs, h.volumes.Delete(ctx, vmID))...)
}

// Stop ends a VM this host runs and leaves the VM behind: a last checkpoint,
// and then the VMM process is closed, the pages go back to the pager and the
// handle is released. Its control record and its objects stay where they are,
// so any host can open it again at exactly the bytes this published — which is
// the whole difference between a stop and losing the host, where the writes
// since the last checkpoint go with it.
//
// The checkpoint is of the disks alone, as the interval's is, unless suspend
// asks for the guest's memory and VMM state as well: a VM stopped plainly is
// cold booted over its disks when it starts again, and a suspended one resumes
// where it was. Uploading a guest's memory is the one thing a stop does not do
// unless asked, because it is the part that is expensive and that most
// software never expected to survive.
//
// It is refused for a VM something still holds sealed, as a delete is. A fork
// point holds the pages of the VMM process this would close, and a child
// elsewhere reads the pages no checkpoint holds out of them; closing the
// process under it would take the point away mid-fault. Nothing is retired
// here to get past that, which is where this parts company with a delete: a
// delete ends the VM for good and stops the children reading it on the way out,
// and a stop is a VM that is coming back.
//
// The checkpoint comes first and nothing is given up until it has landed. A
// publication the store refused leaves the VM exactly as it was — running,
// registered, checkpointing on its interval — because the alternative is a stop
// that reported a failure and lost the guest's last writes anyway.
// The checkpoint it published is what it reports, because that is the pause
// the VM comes back at and nothing else records it: the handle that knew is
// released by the time this answers.
func (h *Host) Stop(ctx context.Context, vmID string, suspend bool) (control.Ref, error) {
	vm := h.vm(vmID)
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	if vm == nil || entry == nil {
		return control.Ref{}, fmt.Errorf("%w: %s", ErrNotRunning, vmID)
	}
	if vm.Status().Sealed {
		return control.Ref{}, fmt.Errorf("%w: %s", volume.ErrSealed, vmID)
	}
	// The checkpoint loop stops first, as a migration's does, and the checkpoint
	// it had in flight lands with it. The pause this publishes is the one the
	// VM comes back at and this is the only account of it — the handle that knew
	// is released by the time this answers — so a loop left running through the
	// publication takes one more checkpoint behind it and makes the answer name
	// a pause that is already superseded.
	//
	// A stop that does not happen leaves the VM exactly as it was, which
	// includes the loop: the guest is running, its writes are in this host's
	// pages, and what a checkpoint that did not land costs is only how far a
	// host loss would rewind it.
	entry.end()
	capture := CaptureDisks
	if suspend {
		capture = Capture
	}
	checkpoint, err := capture(ctx, vm, entry.runtime, h.clock)
	if err != nil {
		h.run(vmID, entry)
		return control.Ref{}, fmt.Errorf("the last checkpoint of %s: %w", vmID, err)
	}
	if err := checkpoint.Wait(ctx); err != nil {
		h.run(vmID, entry)
		return control.Ref{}, fmt.Errorf("publishing the last checkpoint of %s: %w", vmID, err)
	}
	// Whoever drops the registration owns giving the VM up, so a stop that
	// raced a takeover or a death leaves the closing to whichever got there.
	if !h.forget(vmID, entry) {
		return checkpoint.Ref(), nil
	}
	if h.pages != nil {
		h.pages.Discard(vmID)
	}
	errs := []error{entry.runtime.Close()}
	if held := h.vm(vmID); held != nil {
		errs = append(errs, held.Close(ctx))
	}
	if err := errors.Join(errs...); err != nil {
		return control.Ref{}, err
	}
	slog.InfoContext(ctx, "host: stopped a VM", "vm", vmID,
		"checkpoint", checkpoint.Ref().Sequence, "suspended", suspend)
	return checkpoint.Ref(), nil
}

// Machines reports the VMs this host runs, in ascending identity order.
func (h *Host) Machines() []string {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	return slices.Sorted(maps.Keys(h.machines.running))
}

// vm finds this host's open handle on one VM.
func (h *Host) vm(vmID string) *volume.VM {
	for _, vm := range h.volumes.VMs() {
		if vm.ID() == vmID {
			return vm
		}
	}
	return nil
}
