package volume

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Checkpoint publishes every dirty page of every volume and selects the
// resulting index in the VM's control record, which is the moment those bytes
// survive the loss of this host. It is a no-op when nothing is dirty, and it
// does not stall writes: they continue into a new overlay generation while the
// publication runs.
func (vm *VM) Checkpoint(ctx context.Context) error {
	return vm.checkpoint(ctx, vm.isRoot())
}

// checkpoint publishes the overlay. force publishes even when nothing is dirty,
// which is what an explicit checkpoint of a fork does: its root index is what
// makes the fork a VM anyone can open, so there is always something to publish.
// A close does not force, so a fork that ends before it was ever checkpointed
// leaves no object at all.
func (vm *VM) checkpoint(ctx context.Context, force bool) error {
	if err := vm.pubMu.Lock(ctx); err != nil {
		return err
	}
	ckpt, err := vm.capture(nil, nil, force)
	if ckpt == nil || err != nil {
		vm.pubMu.Unlock()
		return err
	}
	return vm.complete(ctx, ckpt)
}

// DiscardMemory discards every byte of the named volume — the VM's memory — and
// publishes a checkpoint that names neither its pages nor any VMM state, at the
// sizes given. It is what a cold boot is made of: a VM whose memory is gone has
// nothing to restore, so the guest that opens this checkpoint boots its kernel
// from the volumes that are left, which are exactly what the last checkpoint
// published.
//
// The discard and the publication are one operation under the publication lock,
// because half of either is a VM that cannot be resumed and cannot be booted:
// memory without the state that was captured over it, or state without the
// memory it describes.
//
// shape is the shape the VM takes from here. A volume its sizes do not name
// keeps the size it has. The memory may take any size, up or down — it is being
// discarded anyway — and so may an ephemeral disk, which holds nothing. Every
// other volume may only grow, because the end of a filesystem is not this
// package's to cut. A grown volume's new pages read as zeroes. A shape with no
// processor count keeps the one the VM has.
//
// It is called on a VM nothing is running: a cold start opens the VM, discards
// its memory and only then boots it, and a create publishes its first
// checkpoint this way before the first boot. A publication that fails leaves the
// VM exactly as it was, at the checkpoint it was opened on and at the shape it
// had.
func (vm *VM) DiscardMemory(ctx context.Context, memory string, shape Shape) error {
	if err := vm.pubMu.Lock(ctx); err != nil {
		return err
	}
	if shape.VCPUs < 0 {
		vm.pubMu.Unlock()
		return ErrInvalidConfig
	}
	ckpt, undo, err := vm.discarding(memory, shape.Sizes)
	if ckpt == nil || err != nil {
		vm.pubMu.Unlock()
		return err
	}
	ckpt.vcpus = shape.VCPUs
	if err := vm.complete(ctx, ckpt); err != nil {
		undo()
		return err
	}
	return nil
}

// Shape is what a cold boot gives a VM from here: the sizes of the volumes it
// changes, by name, and how many processors the guest boots with, zero to keep
// the count the VM has.
type Shape struct {
	Sizes map[string]uint64
	VCPUs int
}

// VCPUs is how many processors a boot of this VM's selected checkpoint gives
// the guest, zero where none is recorded and the host's default applies.
func (vm *VM) VCPUs() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.baseIndex == nil {
		return 0
	}
	return vm.baseIndex.VCPUs()
}

// discarding zeroes the whole memory volume and captures the checkpoint that
// publishes it under one hold of the VM's lock, so nothing is written into the
// volume between the discard and the checkpoint that says it holds nothing. It
// returns the way back: a publication that fails restores the overlay the
// discard replaced, which is what leaves the VM at the shape and the bytes it
// was opened on.
func (vm *VM) discarding(memory string, sizes map[string]uint64) (*Checkpoint, func(), error) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if err := vm.readyLocked(); err != nil {
		return nil, nil, err
	}
	held := vm.byName[memory]
	if held == nil {
		return nil, nil, ErrUnknownVolume
	}
	resized, err := vm.reshape(sizes, memory)
	if err != nil {
		return nil, nil, err
	}
	// The discard covers the pages the new shape still has: a page past the end
	// of a volume that shrank is one the publication trims out of the index
	// rather than one it republishes as zeroes, and a page a volume grew into
	// was never published and already reads as zeroes.
	span := held.size
	if next, found := resized[memory]; found {
		span = min(span, next)
	}
	was, dirty := vm.overlays[held.ordinal], vm.dirty
	if span > 0 {
		vm.applyLocked(held.ordinal, []change{{length: span}})
	}
	ckpt, err := vm.captureLocked(nil, nil, true)
	if ckpt == nil || err != nil {
		vm.overlays[held.ordinal], vm.dirty = was, dirty
		vm.publishLocked()
		return nil, nil, err
	}
	ckpt.dropState, ckpt.resized = true, resized
	return ckpt, func() {
		vm.mu.Lock()
		defer vm.mu.Unlock()
		defer vm.publishLocked()
		vm.overlays[held.ordinal], vm.dirty = was, dirty
	}, nil
}

// reshape reports the sizes this cold boot changes, having refused the ones it
// will not make: a volume this VM does not have, a size that is not a whole
// number of sectors, and a volume other than the memory or an ephemeral disk
// that would shrink. A size a volume already has is not a change and is left
// out.
func (vm *VM) reshape(sizes map[string]uint64, memory string) (map[string]uint64, error) {
	if len(sizes) == 0 {
		return nil, nil
	}
	changed := make(map[string]uint64, len(sizes))
	for name, size := range sizes {
		held := vm.byName[name]
		if held == nil {
			return nil, ErrUnknownVolume
		}
		if size == 0 || size%checkpoint.SectorSize != 0 {
			return nil, ErrInvalidConfig
		}
		// An ephemeral disk holds nothing at a cold boot, so it may shrink too.
		if size < held.size && name != memory && !held.ephemeral {
			return nil, fmt.Errorf("%w: %s is %d bytes and may not shrink to %d",
				ErrInvalidRange, name, held.size, size)
		}
		if size != held.size {
			changed[name] = size
		}
	}
	return changed, nil
}

// PrepareFunc pauses the guest, captures its VMM state and seals every memory region,
// returning the sealed pager state of each by volume name. Snapshot runs it
// under the VM's publication lock, so it is where two captures of one guest
// serialize.
type PrepareFunc func(context.Context) ([]byte, map[string]DirtySource, error)

// Prepared is a PrepareFunc for a caller with nothing to pause, which is what
// image building and tests have: the state and the sources are already in hand.
func Prepared(state []byte, sources map[string]DirtySource) PrepareFunc {
	return func(context.Context) ([]byte, map[string]DirtySource, error) { return state, sources, nil }
}

// Snapshot takes this VM's publication lock, has prepare seal the guest,
// checkpoints every volume at one write generation, attaches the VMM state and
// the sealed pager state prepare returned, starts the publication in the
// background and returns immediately. The returned checkpoint can be forked or
// read straight away; Wait reports when it became durable.
//
// The lock is held from before prepare until the publication ends, so a capture
// never seals a guest another capture has already sealed, and the publication
// in flight is the only one. It does not unseal: the publication owns every
// source prepare gave it and retires it when it lands, which is what makes the
// pages clean under the checkpoint that now holds them, or hands them back to
// the guest when it does not. A prepare that fails, or a checkpoint that cannot
// be taken after it, leaves the sources to the caller to release.
//
// Unlike Close and Handoff, Snapshot gives nothing up: the handle keeps the VM
// and goes on serving writes into a new overlay generation while the
// publication runs.
//
// terms says whether the checkpoint is kept and what a failed publication
// does; see Terms.
func (vm *VM) Snapshot(ctx context.Context, prepare PrepareFunc, terms Terms) (*Checkpoint, error) {
	return vm.snapshot(ctx, prepare, false, terms)
}

// SnapshotDisks is Snapshot of a VM's disks alone, which is the checkpoint a
// host's interval takes: prepare pauses the guest and seals the memory regions of its
// disks, and captures no VMM state. The checkpoint names none either — not
// even its parent's, which is what a checkpoint that captured none otherwise
// goes on naming — because the registers of an earlier pause over the disks of
// this one are a guest that never existed. Its memory volume keeps the pages
// the checkpoint before it published, and a VM opened at it has no state to
// restore those with: it is cold booted, which discards them.
//
// A prepare that captured state anyway is refused, leaving the sources to the
// caller to release as any failed prepare does.
//
// terms says whether the checkpoint is kept and what a failed publication
// does; see Terms.
func (vm *VM) SnapshotDisks(ctx context.Context, prepare PrepareFunc, terms Terms) (*Checkpoint, error) {
	if prepare == nil {
		return nil, ErrInvalidConfig
	}
	return vm.snapshot(ctx, func(ctx context.Context) ([]byte, map[string]DirtySource, error) {
		state, sources, err := prepare(ctx)
		if err == nil && state != nil {
			err = fmt.Errorf("%w: a checkpoint of the disks captured %d bytes of VMM state",
				ErrInvalidConfig, len(state))
		}
		return nil, sources, err
	}, true, terms)
}

// Terms is what a capture asks of the checkpoint it publishes beyond its pages.
//
// Keep marks the checkpoint kept, in the same control-record write that
// selects it (control.Handle.SelectKept). Reclamation then spares it and
// everything its index names, and a new VM can be created from it after this
// one has moved past it. Retry decides what a failed publication does; nil
// gives its pages back at once.
type Terms struct {
	Keep  bool
	Retry Retry
}

// Retry decides whether a checkpoint whose publication failed keeps its pages
// sealed and publishes again. It is asked after each failed attempt, and may
// wait before it answers; attempt counts the failures so far. Yes publishes the
// same checkpoint again under the same reference: the same pages, the same
// parent and the same compaction, so every object it writes is byte-identical to
// what the failed attempt may have left, and the store carries on past them.
// No gives the pages back to the guest, and the next checkpoint seals them
// again. A publication a later writer fenced is never retried.
//
// Keeping the seal is what lets a checkpoint be taken again without a pause.
// A pager holding a guest's stores until a checkpoint lands holds the vCPUs that
// made them, and a pause needs every vCPU: a checkpoint that gave its pages back
// could not be taken again while those stores wait.
type Retry func(ctx context.Context, attempt int, err error) bool

// snapshot is Snapshot and SnapshotDisks: dropState is a checkpoint that names
// no VMM state at all.
func (vm *VM) snapshot(ctx context.Context, prepare PrepareFunc, dropState bool, terms Terms) (*Checkpoint, error) {
	if prepare == nil {
		return nil, ErrInvalidConfig
	}
	if err := vm.pubMu.Lock(ctx); err != nil {
		return nil, err
	}
	// Whether this VM can be sealed at all is settled before its guest is touched:
	// a caller told no here has paused nothing and unseals nothing, which is what
	// keeps a capture from abandoning the checkpoint a fork point holds.
	if err := vm.sealable(); err != nil {
		vm.pubMu.Unlock()
		return nil, err
	}
	state, sources, err := prepare(ctx)
	if err != nil {
		vm.pubMu.Unlock()
		return nil, err
	}
	if sim.Bug(ctx, "volume-drop-captured-state") {
		// The volume's bytes are captured and the VMM state the same pause
		// took is not: the checkpoint reads back as a guest that never ran.
		state = nil
	}
	// The guest is running again, and the pages the seal froze cannot change,
	// so this is where every source drops the pages the guest never really
	// stored into — before anything enumerates them.
	unchanged, err := settle(ctx, sources)
	if err != nil {
		vm.pubMu.Unlock()
		return nil, err
	}
	ckpt, err := vm.capture(state, sources, true)
	if ckpt == nil || err != nil {
		vm.pubMu.Unlock()
		return nil, err
	}
	ckpt.unchanged, ckpt.dropState, ckpt.retry, ckpt.keep = unchanged, dropState, terms.Retry, terms.Keep
	go func() {
		if err := vm.complete(vm.ctx, ckpt); err != nil {
			report(vm.ctx, "volume: snapshot publication failed", vm.id, err)
		}
	}()
	return ckpt, nil
}

// settle has every volume's seal drop the pages whose bytes the guest never
// changed, and reports how many pages went. The volumes settle at the same time
// as each other, because each one's pages are compared independently of every
// other's and the upload is what waits for all of them; the count is summed in
// name order, so it does not depend on which of them finishes first.
func settle(ctx context.Context, sources map[string]DirtySource) (int, error) {
	names := slices.Sorted(maps.Keys(sources))
	counts := make([]int, len(names))
	failures := make([]error, len(names))
	var wait sync.WaitGroup
	for i, name := range names {
		wait.Add(1)
		go func() {
			defer wait.Done()
			counts[i], failures[i] = sources[name].Settle(ctx)
		}()
	}
	wait.Wait()
	unchanged := 0
	for _, count := range counts {
		unchanged += count
	}
	return unchanged, errors.Join(failures...)
}

// isRoot reports a fork that has not published its own root index yet.
func (vm *VM) isRoot() bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.root
}

// sealable reports whether this VM's pages may be sealed now. A fork point
// holds them until the child it was taken for has them, and one seal of a
// memory region is outstanding at a time.
func (vm *VM) sealable() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if err := vm.readyLocked(); err != nil {
		return err
	}
	if vm.sealed {
		return ErrSealed
	}
	return nil
}

// capture takes one consistent checkpoint of every volume. Writes and captures
// both hold the VM's lock, so the overlays captured here hold exactly the
// writes at or below the captured generation.
func (vm *VM) capture(state []byte, sources map[string]DirtySource, force bool) (*Checkpoint, error) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.captureLocked(state, sources, force)
}

// captureLocked is the body of capture, for a caller that already holds the
// VM's lock because what it did to the overlays and the checkpoint that
// publishes it are one step.
func (vm *VM) captureLocked(state []byte, sources map[string]DirtySource, force bool) (*Checkpoint, error) {
	if err := vm.readyLocked(); err != nil {
		return nil, err
	}
	if vm.baseIndex == nil {
		return nil, ErrCorrupt
	}
	for name := range sources {
		held := vm.byName[name]
		if held == nil {
			return nil, ErrUnknownVolume
		}
		// A pager seals nothing of an ephemeral disk, and a caller that offers
		// its pages anyway is refused before they could reach a checkpoint.
		if held.ephemeral {
			return nil, fmt.Errorf("%w: sealed pages of %s", ErrEphemeral, name)
		}
	}
	if !force && vm.dirty == 0 {
		return nil, nil
	}
	// A sequence must belong to this handle's epoch, or a fenced predecessor
	// and this handle could write different contents under one reference.
	if !control.ValidSequence(vm.control.Epoch(), vm.next) {
		return nil, ErrCapacity
	}
	ckpt := &Checkpoint{
		owner:       vm,
		ref:         control.Ref{VM: vm.id, Sequence: vm.next},
		parent:      vm.base,
		parentIndex: vm.baseIndex,
		overlays:    make(map[string]*extentIndex, len(vm.names)),
		sources:     sources,
		inherited:   vm.inherited,
		sizes:       make(map[string]uint64, len(vm.names)),
		geometry:    vm.geometries(),
		position:    vm.applied,
		state:       state,
		hasState:    state != nil,
		done:        make(chan struct{}),
		swept:       make(chan struct{}),
	}
	ckpt.base = newSealedSource(vm.base, ckpt.ref, sources, ckpt.geometry)
	for ordinal, name := range vm.names {
		ckpt.overlays[name] = vm.overlays[ordinal]
		ckpt.sizes[name] = vm.volumes[ordinal].size
	}
	// The checkpoint owns this sequence from now on. The entries it captured go on
	// naming it; writes accepted after it belong to the next sequence, so no
	// two contents ever report the same identity.
	vm.frozen, vm.frozenRef = ckpt.position, ckpt.ref
	vm.next++
	vm.publishLocked()
	return ckpt, nil
}

// complete publishes a captured checkpoint, installs it, gives the guest its
// pages back, releases everything waiting on it, and only then reclaims what
// it replaced. The caller holds the publication lock, which complete releases:
// the sweep is a run of object-store deletes over objects nothing reads any
// more, and neither the guest nor the next capture of it waits for those.
func (vm *VM) complete(ctx context.Context, ckpt *Checkpoint) error {
	// Every object-store call this publication makes counts into the checkpoint's
	// own meter as well as the store's totals, so one checkpoint's traffic is
	// separable from that of the checkpoints running beside it.
	ctx = platform.WithObjectMeter(ctx, &ckpt.meter)
	index, record, err := vm.publish(ctx, ckpt)
	for attempt := 1; err != nil && ckpt.retrying(ctx, attempt, err); attempt++ {
		index, record, err = vm.publish(ctx, ckpt)
	}
	var replaced *checkpoint.Index
	if err == nil {
		replaced = vm.install(ckpt, index)
	} else {
		vm.release(ckpt)
	}
	// A fence removes this handle's authority for good, so everything waiting on
	// the checkpoint is told that rather than only the conditional write that
	// failed.
	err = vm.observe(err)
	// Retiring comes after the install, because what makes a sealed page clean
	// is the identity this VM now gives it, which is the checkpoint just
	// selected — and before anything else this publication does, because a
	// guest whose seal still stands copies every store it makes. A retire that
	// fails does not unpublish anything: the bytes are durable, and the pager
	// reports why its pages are still sealed.
	if retired := ckpt.retire(context.WithoutCancel(ctx), err == nil); retired != nil {
		err = errors.Join(err, retired)
	}
	if err == nil {
		// A fork that has published its root owns every page it inherited, so
		// the parent's sealed pages go back to its guest. The pin on the
		// parent's checkpoint is not given back with them: this index may still
		// name the parent's checkpoints, and an index of a VM forked from this one
		// may name them even when this one does not.
		if point := vm.takePoint(); point != nil {
			err = point.Retire(context.WithoutCancel(ctx))
		}
	}
	ckpt.finish(err)
	vm.record(err)
	// The sweep is counted before the lock goes, so a close that takes the
	// lock next waits for it.
	vm.sweeps.Add(1)
	vm.pubMu.Unlock()
	vm.reclaim(ctx, replaced, index, record)
	vm.sweeps.Done()
	close(ckpt.swept)
	return err
}

// publish names every page the checkpoint changed and then selects the index
// the publication produced. Page contents come from the checkpoint's own
// immutable view — the overlay for what was written through this package, and
// the sealed pages for what a pager holds — never from the VM's live state, so
// writes accepted after the checkpoint cannot reach it. It returns the control
// record the selection produced, whose pins and kept checkpoints are what
// reclamation must spare.
func (vm *VM) publish(ctx context.Context, ckpt *Checkpoint) (*checkpoint.Index, control.Record, error) {
	publication := vm.manager.config.Store.Begin(ckpt.parentIndex, ckpt.ref)
	// The checkpoints this handle knows are pinned or kept are what compaction
	// must leave alone; the selection below reports any a fork pinned while
	// this publication ran, and reclamation spares those. They are read once: a
	// retry of this publication compacts exactly as its first attempt did, so it
	// writes the same bytes under the same reference.
	if ckpt.protected == nil {
		ckpt.protected = append([]uint64{}, vm.control.Record().Protected()...)
	}
	publication.Protect(ckpt.protected)
	// A fork's child may have ephemeral disks its parent's checkpoint does not,
	// or at another size: its root is where they are first recorded.
	for _, name := range vm.names {
		held, size := vm.byName[name], ckpt.sizes[name]
		switch {
		case !slices.Contains(ckpt.parentIndex.Volumes(), name):
			publication.Add(name, checkpoint.VolumeSpec{Size: size, PageSize: held.geometry.PageSize,
				Ephemeral: held.ephemeral})
		case ckpt.parentIndex.Size(name) != size:
			publication.SetSize(name, size)
		}
	}
	// The shape comes before the pages: a volume that shrank drops the pages
	// past its new end rather than republishing them, and one that grew has
	// somewhere for its new pages to be.
	for _, name := range slices.Sorted(maps.Keys(ckpt.resized)) {
		publication.SetSize(name, ckpt.resized[name])
	}
	for _, name := range vm.names {
		for _, number := range publishedPages(ckpt, name) {
			publication.Dirty(name, number)
		}
	}
	switch {
	case ckpt.hasState:
		publication.SetState(ckpt.state)
	case ckpt.dropState:
		publication.DropState()
	}
	if ckpt.vcpus != 0 {
		publication.SetVCPUs(ckpt.vcpus)
	}
	index, err := publication.Commit(ctx, checkpointSource{checkpoint: ckpt})
	if err != nil {
		return nil, control.Record{}, err
	}
	record, err := vm.selecting(ctx, ckpt, index)
	if err != nil {
		return nil, control.Record{}, err
	}
	return index, record, nil
}

// selecting selects a published checkpoint in the control record, and keeps it
// in the same write when its capture asked for that, so no sweep can see it
// selected and replaced without seeing it kept.
func (vm *VM) selecting(ctx context.Context, ckpt *Checkpoint, index *checkpoint.Index) (control.Record, error) {
	if ckpt.keep {
		return vm.control.SelectKept(ctx, ckpt.ref.Sequence, index.HasState())
	}
	return vm.control.Select(ctx, ckpt.ref.Sequence)
}

// publishedPages reports every page one volume of a checkpoint publishes: what
// its own overlay and pager seal hold, and, for a fork's root index, the
// pages it inherited from the point it was forked at, which it reads through
// that point and publishes as its own.
func publishedPages(ckpt *Checkpoint, name string) []uint64 {
	return mergePages(changedPages(ckpt.overlays[name], ckpt.sources[name], ckpt.geometry[name]),
		ckpt.inherited[name])
}

// mergePages unions two ascending page lists.
func mergePages(a, b []uint64) []uint64 {
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		return b
	}
	pages := make(map[uint64]bool, len(a)+len(b))
	for _, page := range a {
		pages[page] = true
	}
	for _, page := range b {
		pages[page] = true
	}
	return slices.Sorted(maps.Keys(pages))
}

// changedPages reports every page one volume of a checkpoint republishes, in
// ascending order: the pages its overlay covers and the pages the pager sealed.
// A pager page is a store page, so a sealed page number is a page number of
// this volume, in this volume's own page size.
func changedPages(overlay *extentIndex, source DirtySource, geometry checkpoint.Geometry) []uint64 {
	written := dirtyPages(overlay, geometry)
	if source == nil {
		return written
	}
	sealed := source.DirtyPages()
	if len(written) == 0 {
		return sealed
	}
	pages := make(map[uint64]bool, len(written)+len(sealed))
	for _, page := range written {
		pages[page] = true
	}
	for _, page := range sealed {
		pages[page] = true
	}
	return slices.Sorted(maps.Keys(pages))
}

// install adopts the published checkpoint and drops the overlay entries it
// incorporated. Entries above the checkpoint's generation are the writes that
// continued during the publication and stay in the overlay. It returns the
// index this handle published before this one, which is what the checkpoint
// just selected has replaced.
func (vm *VM) install(ckpt *Checkpoint, index *checkpoint.Index) *checkpoint.Index {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	defer vm.publishLocked()
	replaced := vm.owned
	vm.owned = index
	vm.base = indexSource{store: vm.manager.config.Store, index: index}
	vm.baseIndex = index
	vm.head = ckpt.ref
	if vm.frozenRef == ckpt.ref {
		vm.frozen, vm.frozenRef = 0, control.Ref{}
	}
	// A cold boot's shape becomes the VM's here and nowhere else: the index
	// that carries it is durable and selected, so a volume this handle now
	// reports the new size of reads through a checkpoint that has it.
	for name, size := range ckpt.resized {
		vm.byName[name].size = size
	}
	if vm.root {
		// The root index exists now, and with it every page this fork
		// inherited: it reads its own objects from here and the point it was
		// forked at is nothing to it. Which sequence it landed under does not
		// enter it — a root publication that failed burnt its own, and the
		// selection this one made is what the record now says the fork is.
		vm.root, vm.inherited = false, nil
	}
	dirty := uint64(0)
	for ordinal := range vm.overlays {
		vm.overlays[ordinal] = vm.overlays[ordinal].retain(ckpt.position)
		dirty += dirtySectorCount(vm.overlays[ordinal]) * checkpoint.SectorSize
	}
	vm.dirty = dirty
	return replaced
}

// reclaim deletes the checkpoints the new index no longer reads from: the one
// this handle published before it, and every checkpoint that one named which this one
// does not. A sequence a fork was taken at is pinned in the control record the
// selection returned and is left whole — its index object, its parts, and every checkpoint
// that index names — because forks this VM cannot see read through it. A kept
// sequence is left whole the same way, because a fork may yet be taken of it.
//
// Only the checkpoints this handle published are reclaimed. The one it opened
// on was published by a writer whose publications this handle cannot account
// for, so its unreferenced objects are left behind; a collector reclaims them.
// Deletion failures are reported and not retried: everything they miss is
// unreferenced, and the next publication has its own checkpoint to reclaim.
//
// It runs with the publication lock released, so the deletes hold up neither
// the guest nor the next capture of it. Two sweeps of one VM never contend:
// each one's dead set comes from the index it replaced, and the one after it
// starts from the index this one made current.
func (vm *VM) reclaim(ctx context.Context, replaced, current *checkpoint.Index, record control.Record) {
	if replaced == nil {
		return
	}
	protected := record.Protected()
	if sim.Bug(ctx, "volume-reclaim-kept") {
		// The sweep spares what forks pinned and forgets what is kept, so a
		// kept checkpoint no fork was taken from goes with the next sweep.
		protected = record.Pinned
	}
	if err := vm.manager.config.Store.Reclaim(ctx, replaced, current, protected); err != nil {
		report(ctx, "volume: reclaiming the replaced checkpoint's objects failed", vm.id, err)
	}
}

// release abandons the checkpoint of a failed publication. Its sequence stays
// burnt, always: the parts that did land are under that reference, and the next
// checkpoint seals whatever the guest has dirtied since — a different set of
// pages, which republished under the same reference is one reference naming two
// contents. The create-if-absent rule every object is written with reports that
// as a conflict on every interval from then on, so a VM whose dirty set a guest
// moves without this package seeing it would never publish again.
//
// The idempotent retry that makes a lost reply harmless is the one inside a
// single Commit, which republishes byte-identical objects. Nothing across
// publications is byte-identical, so nothing across them may share a reference.
//
// Burning a sequence costs nothing worth accounting for: the counter is 32 bits
// within one writer epoch, which at a checkpoint a minute is thousands of years.
func (vm *VM) release(ckpt *Checkpoint) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	defer vm.publishLocked()
	if vm.frozenRef == ckpt.ref {
		vm.frozen, vm.frozenRef = 0, control.Ref{}
	}
}

// record keeps the last publication outcome for Status.
func (vm *VM) record(err error) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.published = err
}
