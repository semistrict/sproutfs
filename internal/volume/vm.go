package volume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/ctxsync"
)

// VolumeSpec is one volume of a VM being created: its name and its size, which
// must be a whole number of pages.
type VolumeSpec struct {
	Name string
	Size uint64
}

// generation orders the writes one handle has applied. It is local and starts
// at zero for every handle, because nothing survives a handle but its published
// checkpoints: its only job is to say which overlay entries a checkpoint
// captured.
type generation uint64

// view is one consistent read view of a whole VM: the checkpoint its overlays
// sit on, the overlays themselves, and the reference the bytes they hold will
// be published under. It is replaced whole, so a reader never sees a new
// checkpoint under an old overlay.
type view struct {
	base     source
	overlays []*extentIndex
	owner    lineage
	err      error
}

// Status is a VM's local view of its own durability.
type Status struct {
	// Checkpoint is the selected checkpoint. Everything it published survives
	// the loss of this host; everything written since does not.
	Checkpoint control.Ref
	// Epoch is the writer token this handle holds in the control record. Every
	// sequence it publishes under belongs to it.
	Epoch uint64
	// DirtyBytes is an upper bound on the bytes the next checkpoint publishes,
	// which is also an upper bound on what this host's loss would cost.
	DirtyBytes uint64
	// HandedOff reports that Handoff released this VM to another host. It stays
	// set for the life of the handle, whose every operation reports
	// ErrHandedOff.
	HandedOff bool
	// Sealed reports that a fork point holds this VM's frames. Nothing may seal
	// them again — no capture, no fork — until that point is retired, which is
	// when the child it was taken for has published or pulled every page it
	// inherited.
	Sealed bool
	// Root reports a fork whose own root index has not been published yet. It
	// runs here and nowhere else: opening it anywhere reports ErrForkPending,
	// and a host lost before its first checkpoint loses it.
	Root bool
	// CheckpointError is the last publication failure, and nil once one
	// succeeds. A failed publication does not stop writes.
	CheckpointError error
	// Err is the terminal error of this handle: closed, handed off, or fenced
	// by a later writer.
	Err error
}

// VM is one virtual machine's volumes and its checkpoint publication. Methods
// are safe for concurrent use. Opening a VM again takes it over; the old handle
// fails as soon as it tries to write the control record.
type VM struct {
	manager *Manager
	id      string
	ordinal uint64 // assigned once by the manager when this handle is adopted
	names   []string
	volumes []*Volume
	byName  map[string]*Volume
	control *control.Handle

	current atomic.Pointer[view]

	// pubMu admits one publication at a time.
	pubMu *ctxsync.Mutex
	// sweeps counts the sweeps running behind publications that have released
	// pubMu, so that a close finishes them rather than cancelling them: a host
	// closing its VMs on the way out is exiting, not leaking.
	sweeps sync.WaitGroup

	mu        sync.Mutex
	base      source
	baseIndex *checkpoint.Index
	// owned is the index this handle itself published and that baseIndex now
	// is. The next successful publication reclaims its unreferenced objects. It
	// is nil until this handle has published anything, because a handle cannot
	// account for what the writer before it left behind.
	owned    *checkpoint.Index
	overlays []*extentIndex
	head     control.Ref
	// next is the sequence the bytes now in the overlays will be published under.
	// It advances when a checkpoint takes ownership of a sequence, not when that
	// checkpoint is installed, so the writes that continue during a publication
	// report a different lineage identity from the ones the checkpoint froze.
	next uint64
	// frozen is the generation of the publication in flight and frozenRef the
	// checkpoint it will publish, so the entries that publication captured go
	// on naming it while it runs and only the writes after it name the next.
	frozen    generation
	frozenRef control.Ref
	// applied is the generation of the last write this handle accepted.
	applied generation
	dirty   uint64
	closed  bool
	// poisoned is set when a later writer has taken the control record, after
	// which nothing this handle holds can ever be published.
	poisoned error
	// handedOff is set by Handoff, which releases the VM to another host
	// without publishing anything.
	handedOff bool
	published error
	// sealed reports that a fork point holds this VM's sealed frames. Nothing
	// may seal them again until it is retired.
	sealed bool
	// root reports a fork that has not published its own root index yet, point
	// the instant it reads through, and inherited the parent's unpublished
	// pages, which that root republishes as the fork's own. A fork on another
	// host has no point to read through: its pager pulled those pages, so they
	// are its own dirty state and inherited is empty.
	root      bool
	point     *ForkPoint
	inherited map[string][]uint64

	ctx    context.Context
	cancel context.CancelFunc
}

// Volume is one named byte-addressed volume of a VM. Its size is fixed for the
// VM's lifetime.
type Volume struct {
	vm      *VM
	name    string
	size    uint64
	ordinal int
}

// Name reports the volume's name, which is its identity within its VM.
func (v *Volume) Name() string { return v.name }

// Size reports the volume's size in bytes.
func (v *Volume) Size() uint64 { return v.size }

// MaxWriteBytes reports the largest payload one Write or WriteBatch may carry,
// which is Config.MaxWriteBytes. A caller that batches its own writes, such as
// the pager's writeback, sizes its batches against this.
func (v *Volume) MaxWriteBytes() int { return v.vm.manager.config.MaxWriteBytes }

// newVM builds a handle over the control record it holds. base is what the
// overlays sit on and index the checkpoint behind it, which for a fork is its
// parent's: a fork's first checkpoint is its own root index, published over
// that parent. owned is that same index when this handle published it — a
// create's root — and nil when the writer before it did.
func newVM(m *Manager, id string, handle *control.Handle, base source, index, owned *checkpoint.Index,
	specs []VolumeSpec, point *ForkPoint) *VM {
	record := handle.Record()
	vm := &VM{
		manager: m, id: id, control: handle,
		byName: make(map[string]*Volume, len(specs)),
		pubMu:  ctxsync.NewMutex(),
		base:   base, baseIndex: index, owned: owned,
		head: control.Ref{VM: id, Sequence: record.Selected},
		next: nextSequence(handle.Epoch(), record.Selected),
	}
	if point != nil {
		// The root is the sequence the record already selects, and this fork
		// publishes it rather than inheriting one that exists.
		vm.root, vm.point, vm.inherited = true, point, point.inheritedPages()
		vm.next = record.Selected
	}
	for ordinal, spec := range specs {
		volume := &Volume{vm: vm, name: spec.Name, size: spec.Size, ordinal: ordinal}
		vm.names = append(vm.names, spec.Name)
		vm.volumes = append(vm.volumes, volume)
		vm.byName[spec.Name] = volume
	}
	vm.overlays = make([]*extentIndex, len(specs))
	vm.publishLocked()
	return vm
}

// ID reports the identity the orchestrator allocated for this VM.
func (vm *VM) ID() string { return vm.id }

// Epoch reports the writer token this handle holds in the control record.
func (vm *VM) Epoch() uint64 { return vm.control.Epoch() }

// Volume returns one volume by name, nil for a name this VM does not have.
func (vm *VM) Volume(name string) *Volume { return vm.byName[name] }

// Volumes returns every volume, in ascending name order.
func (vm *VM) Volumes() []*Volume { return slices.Clone(vm.volumes) }

// Status reports this VM's durability without contacting the network.
func (vm *VM) Status() Status {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return Status{
		Checkpoint:      vm.head,
		Epoch:           vm.control.Epoch(),
		DirtyBytes:      vm.dirty,
		HandedOff:       vm.handedOff,
		Sealed:          vm.sealed,
		Root:            vm.root,
		CheckpointError: vm.published,
		Err:             vm.readyLocked(),
	}
}

// Close publishes a final checkpoint when anything is dirty, stops the
// publication worker, waits for any publication in flight, and releases the
// handle. A failed final checkpoint is logged and does not block the release:
// the bytes it could not publish are lost, which is what losing a host costs
// anyway. A canceled Close can be retried with a fresh context.
//
// Callers quiesce writes first: a write accepted after the final checkpoint has
// captured its checkpoint is acknowledged and then dropped.
func (vm *VM) Close(ctx context.Context) error {
	vm.mu.Lock()
	usable := vm.readyLocked() == nil
	vm.mu.Unlock()
	if usable {
		// A close does not publish a fork's root on its own: a fork that ends
		// before it was ever checkpointed is one nothing inherited, and it
		// leaves no object behind.
		if err := vm.checkpoint(ctx, false); err != nil && !errors.Is(err, ErrClosed) {
			slog.Warn("volume: final checkpoint on close failed", "vm", vm.id, "error", err)
		}
	}
	vm.mu.Lock()
	vm.closed = true
	point := vm.point
	// A fork that is closed still holding its own root has published nothing
	// and never will: this handle was the only thing that could. A handed-off
	// one is the exception — the host it went to publishes that root.
	abandoned := vm.root && !vm.handedOff
	vm.point, vm.inherited = nil, nil
	vm.publishLocked()
	vm.mu.Unlock()
	err := vm.stop(ctx)
	// The parent of a fork that never published gets its sealed frames back
	// here: nothing inherited them in the end, so its next checkpoint takes
	// them again. It does not get its pin back — nothing ever does — so a fork
	// abandoned before its root costs the parent the checkpoint it was taken
	// at until a collector finds that no lineage reads it.
	if point != nil {
		err = errors.Join(err, point.Retire(context.WithoutCancel(ctx)))
	}
	// The record goes with it. A fork that never published is an identity
	// nothing could use again while its record stood: creating it reports that
	// it exists and opening it reports a fork still pending, so leaving the
	// record behind costs the deployment an identity and an object for ever. It
	// is the same unwinding Fork does for a child it could not attach at all,
	// and it is idempotent: removing a record that is gone is not an error. The
	// pin on the parent does not go with it, because nothing gives a pin back.
	if abandoned {
		err = errors.Join(err, vm.manager.config.Control.Delete(context.WithoutCancel(ctx), vm.id))
	}
	return err
}

// stop gives the VM up for good: it ends the publication worker and waits for a
// publication another call already had in flight. The caller has already made
// the handle terminal, so nothing new can start.
//
// A canceled wait is inconclusive and keeps the handle registered so the call
// can be repeated with a live context.
func (vm *VM) stop(ctx context.Context) error {
	if err := vm.pubMu.Lock(ctx); err != nil {
		return err
	}
	vm.pubMu.Unlock()
	// The sweep behind the last publication runs on the handle's own context,
	// which cancelling first would cut short. It is a bounded run of deletes
	// over objects nothing reads, so it is waited for under the caller's
	// deadline and only then is the handle's context ended.
	swept := make(chan struct{})
	go func() {
		vm.sweeps.Wait()
		close(swept)
	}()
	select {
	case <-ctx.Done():
		vm.cancel()
		return context.Cause(ctx)
	case <-swept:
	}
	vm.cancel()
	vm.manager.release(vm)
	return nil
}

// readyLocked reports the terminal error of this handle, nil while it can still
// be used. A handed-off handle keeps saying so even after it is closed, because
// that is the fact its owner needs: the VM moved on rather than stopped.
func (vm *VM) readyLocked() error {
	if vm.handedOff {
		return ErrHandedOff
	}
	if vm.closed {
		return ErrClosed
	}
	return vm.poisoned
}

// publishLocked replaces the immutable read view after any change to the
// overlays, the checkpoint or the handle's terminal state.
func (vm *VM) publishLocked() {
	vm.current.Store(&view{
		base:     vm.base,
		overlays: slices.Clone(vm.overlays),
		owner:    lineage{frozen: vm.frozen, frozenRef: vm.frozenRef, next: control.Ref{VM: vm.id, Sequence: vm.next}},
		err:      vm.readyLocked(),
	})
}

// apply installs one write into the overlays under a new generation. It is the
// whole write path: nothing is appended anywhere, and nothing waits.
func (vm *VM) apply(ordinal int, changes []change) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	defer vm.publishLocked()
	if err := vm.readyLocked(); err != nil {
		return err
	}
	vm.applyLocked(ordinal, changes)
	return nil
}

// applyLocked is the body of apply, for a caller that already holds the VM's
// lock because the write and what it does next are one step.
func (vm *VM) applyLocked(ordinal int, changes []change) {
	vm.applied++
	overlay := vm.overlays[ordinal]
	for _, item := range changes {
		overlay = replaceExtent(overlay, extent{start: item.offset, end: item.offset + item.length,
			data: item.data, generation: vm.applied})
		vm.dirty += sectorSpan(item.offset, item.length)
	}
	vm.overlays[ordinal] = overlay
}

// takePoint gives up the fork point this handle read through, which it does
// once its root index is published: nothing reads the parent's sealed frames
// any more. It returns nil for a handle that was not a fork, or whose point has
// already been given up.
func (vm *VM) takePoint() *ForkPoint {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.root || vm.point == nil {
		return nil
	}
	point := vm.point
	vm.point = nil
	return point
}

// fail marks the handle terminal after a later writer has taken the control
// record, and returns that terminal error. Nothing this handle holds will ever
// be published.
func (vm *VM) fail(cause error) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	defer vm.publishLocked()
	if vm.poisoned == nil {
		vm.poisoned = errors.Join(ErrNeedsRecovery, cause)
	}
	return vm.poisoned
}

// Confirm re-reads this VM's control record and reports whether this handle
// still holds its epoch. It is how a writer that is publishing nothing learns
// it has been taken over: a running guest writes into frames, so a VM between
// checkpoints — or one a fork point has sealed, which is not checkpointed at
// all — would otherwise find out only when it next tried to publish, if it ever
// did.
//
// A handle the record has moved past is failed here, exactly as a refused
// publication fails it: nothing it holds can ever be published, and every later
// call reports ErrNeedsRecovery. Every other failure is returned unchanged and
// changes nothing, because a record that cannot be read is not evidence of
// anything.
func (vm *VM) Confirm(ctx context.Context) error {
	vm.mu.Lock()
	err := vm.readyLocked()
	vm.mu.Unlock()
	if err != nil {
		return err
	}
	epoch := vm.control.Epoch()
	record, err := vm.manager.config.Control.Read(ctx, vm.id)
	if err != nil {
		return err
	}
	if record.Epoch != epoch {
		return vm.fail(fmt.Errorf("%w: %s is at epoch %d, this handle holds %d",
			control.ErrFenced, vm.id, record.Epoch, epoch))
	}
	return nil
}

// observe turns a control-record failure that removes this handle's authority
// into a terminal error, and passes everything else through unchanged.
func (vm *VM) observe(err error) error {
	if err == nil || !errors.Is(err, control.ErrFenced) {
		return err
	}
	return vm.fail(err)
}

// start registers the handle with its manager. Nothing runs behind a handle:
// every publication belongs to a caller, and the only background one is the
// snapshot a capture leaves uploading under the publication lock.
func (vm *VM) start(ctx context.Context) error {
	vm.ctx, vm.cancel = context.WithCancel(context.WithoutCancel(ctx))
	if err := vm.manager.adopt(vm); err != nil {
		vm.cancel()
		return err
	}
	return nil
}
