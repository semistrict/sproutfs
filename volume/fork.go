package volume

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
)

// ForkPoint is one pause of a VM and what a child of it starts from: the
// checkpoint the parent has published — pinned in the parent's control record,
// so nothing reclaims the checkpoints the child inherits — the pages the parent
// holds that no checkpoint has, and the VMM state captured with them.
//
// Making one publishes nothing. The parent keeps its handle, its volumes and
// its pages: the sealed pages stay the parent's and the child reads them by
// page identity — on this host through the point itself, on another host out
// of the parent's page server. The child's first checkpoint publishes those
// pages as its own, and so does the parent's next, which is why one interval's
// dirty set is uploaded twice when both sides live that long.
//
// Any number of children may start from one point, which is what makes one
// pause of the parent enough for a fan-out of forks: they share the one pin the
// point took. Each of them is one hold on the point, and the seal ends when the
// last hold is retired. A point nothing was forked from is retired by whoever
// took it.
//
// A ForkPoint is immutable once it is returned and safe for concurrent use.
type ForkPoint struct {
	// ref is the checkpoint the child inherits and index its table. checkpoint
	// is the parent's seal over that checkpoint, nil for a point rebuilt
	// on another host, where the pages the parent holds unpublished reach the
	// child through its pager rather than through this.
	checkpoint *Checkpoint
	ref        control.Ref
	index      *checkpoint.Index
	// unpublished names, per volume, the pages no checkpoint of the parent
	// holds. They are exactly what the child must be given.
	unpublished map[string][]uint64
	// control is the parent's own handle, which took the pin on ref for this
	// point. A point rebuilt on another host has none: the pin was written on
	// the host that took the point, by the parent's own writer.
	control *control.Handle

	// mu guards the holders of this point: one per child, taken by Hold. The
	// seal ends when the last of them has retired it, and a point no child ever
	// took is retired by whoever made it.
	mu      sync.Mutex
	holders int
	retired bool
	err     error
}

// Parent is the published checkpoint the child inherits, which is pinned in the
// parent's control record.
func (f *ForkPoint) Parent() control.Ref { return f.ref }

// State is the VMM state captured at the fork point, nil for a point over a
// published checkpoint of a VM that was not running. The bytes belong to the
// point and must not be modified.
func (f *ForkPoint) State() []byte {
	if f.checkpoint == nil {
		return nil
	}
	return f.checkpoint.state
}

// Volumes reports the volumes a child of this point has, in ascending name
// order.
func (f *ForkPoint) Volumes() []string { return slices.Clone(f.index.Volumes()) }

// Size reports one volume's size, zero for a volume this point has no volume of.
func (f *ForkPoint) Size(volume string) uint64 { return f.index.Size(volume) }

// PageSize reports the page one volume of this point is published in, which is
// the unit its page numbers and ReadPage are counted in. Zero is a volume this
// point does not describe. A VM's volumes need not agree about it, so anything
// serving these pages reads it per volume rather than per point.
func (f *ForkPoint) PageSize(volume string) uint64 { return f.index.Geometry(volume).PageSize }

// Pages reports the pages of one volume that no checkpoint of the parent holds,
// in ascending order. They exist only in the parent's pages and overlay, so a
// child on another host must fetch every one of them before this point may be
// retired; a child on this host reads them through the point itself.
func (f *ForkPoint) Pages(volume string) []uint64 { return slices.Clone(f.unpublished[volume]) }

// UnpublishedAge is how long the parent has held the oldest of those pages,
// zero where it holds none of that volume's. A child on another host is dated
// from it, so it inherits the parent's loss window along with the pages it is
// measured over rather than starting a window of its own — a fork every few
// minutes would otherwise carry writes forward for ever without any of them
// ever becoming durable.
//
// A point rebuilt on another host reports nothing: it holds no seal of its own,
// and the age of what the parent still has is the parent's to know.
func (f *ForkPoint) UnpublishedAge(volume string) time.Duration {
	if f.checkpoint == nil {
		return 0
	}
	source := f.checkpoint.sources[volume]
	if source == nil {
		return 0
	}
	return source.UnpublishedAge()
}

// ReadPage fills dst, exactly one page of that volume, with the bytes the fork
// point froze. It is what the parent's page server serves a child on another
// host from, and it neither contacts the network nor touches the parent's live
// state.
func (f *ForkPoint) ReadPage(ctx context.Context, volume string, page uint64, dst []byte) error {
	if f.checkpoint == nil {
		return ErrUnknownVolume
	}
	geometry := f.checkpoint.geometry[volume]
	if geometry.PageSize == 0 {
		return ErrUnknownVolume
	}
	if uint64(len(dst)) != geometry.PageSize {
		return ErrInvalidRange
	}
	return f.checkpoint.read(ctx, volume, page*geometry.PageSize, dst)
}

// Hold takes one hold on this point, for a holder that will Retire it: a child
// created from it here, or one this host serves the pages of while it starts on
// another host. Every child of one fork point is one hold, which is what lets one
// pause of the parent start any number of them.
func (f *ForkPoint) Hold() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retired {
		// The seal this point named has ended and the parent is storing into
		// those pages again, so there is nothing here to keep: a child taken
		// from this point now would inherit a mixture of the pause and whatever
		// its parent has done since. Every caller takes its holds before any of
		// them is given up; this is what makes that the point's own rule.
		return fmt.Errorf("%w: a fork point retired by its last holder", ErrRetired)
	}
	f.holders++
	return nil
}

// Pin writes the pin on the checkpoint this point inherits, which is what keeps
// it whole for every child started from it. It is the parent's conditional
// write, so a pin on a parent that has been fenced is refused rather than
// taken, and it comes before any child exists: a child that existed while what
// it inherits was unpinned could have it reclaimed under it.
//
// The point itself takes it when it is made, so this is the same pin again:
// pinning twice is one pin, and every child of one fork point shares it. Nothing
// gives it back — the pin outlives this point, the child and the host that took
// it, and only a collector releases one. A point rebuilt on another host pins
// nothing: the parent's host wrote the pin before handing the child over, where
// its own writer could.
func (f *ForkPoint) Pin(ctx context.Context) error {
	if f.control == nil {
		return nil
	}
	_, err := f.control.Pin(ctx, f.ref.Sequence)
	return err
}

// Retire gives one hold up, and ends the seal on the parent once the last of
// them has: every sealed page goes back to the parent's guest as ordinary dirty
// state, because nothing was published under this point, so the parent's next
// checkpoint takes those pages again. The parent may seal once more from there.
//
// The pin stays. A point no child was ever taken from leaves the parent pinning
// a checkpoint nothing inherited, which costs that checkpoint's objects until a
// collector establishes that nothing reads them — the price of never taking
// objects out from under a fork that reads them.
//
// It is idempotent past the last hold, and a no-op for a point that sealed
// nothing.
func (f *ForkPoint) Retire(ctx context.Context) error {
	f.mu.Lock()
	if f.holders > 0 {
		f.holders--
	}
	if f.holders > 0 || f.retired {
		err := f.err
		f.mu.Unlock()
		return err
	}
	f.retired = true
	f.mu.Unlock()
	var errs []error
	if f.checkpoint != nil {
		errs = append(errs, f.checkpoint.retire(ctx, false))
		f.checkpoint.owner.unseal()
	}
	err := errors.Join(errs...)
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
	return err
}

// ForkPoint pauses this VM through prepare, resumes it as soon as its memory is
// sealed, and returns the point a child starts from. It publishes nothing:
// the checkpoint the child inherits is the one this VM's control record already
// selects, and everything written since it is in the pages prepare sealed.
//
// The pin comes before the point is returned and is this handle's own
// conditional write, because a child that exists while what it inherits is
// unpinned could have it reclaimed under it. It is one pin for the point,
// shared by every child of it and never given back.
//
// It runs under the publication lock, so a fork and an interval checkpoint of
// one guest serialize there rather than racing to seal the same memory regions. The
// seal stays until the point is retired: nothing may capture this VM in the
// meantime, which Status reports as Sealed.
func (vm *VM) ForkPoint(ctx context.Context, prepare PrepareFunc) (*ForkPoint, error) {
	if prepare == nil {
		return nil, ErrInvalidConfig
	}
	if err := vm.pubMu.Lock(ctx); err != nil {
		return nil, err
	}
	defer vm.pubMu.Unlock()
	if err := vm.beginFork(); err != nil {
		return nil, err
	}
	state, sources, err := prepare(ctx)
	if err != nil {
		vm.unseal()
		return nil, err
	}
	point, err := vm.forkPoint(state, sources)
	if err != nil {
		vm.unseal()
		return nil, err
	}
	if _, err := vm.control.Pin(ctx, point.ref.Sequence); err != nil {
		// Nothing was published, so the sealed pages go straight back to the
		// guest and this VM can be forked again once whatever fenced it is
		// dealt with.
		return nil, errors.Join(vm.observe(err), point.Retire(context.WithoutCancel(ctx)))
	}
	return point, nil
}

// Share offers the parent's sealed pages to this host under the identity this
// point gives them, so a child started here maps them instead of reading
// them. It is what a child taken in on the parent's own host attaches over:
// every page it inherited is present the moment its memory region attaches, and no
// byte is copied and nothing is fetched.
//
// A point over a published checkpoint alone — one rebuilt on a host that never
// held the parent — has no pages to offer, and a child of it reads what it
// inherited out of object storage and its parent's page server instead.
//
// It is idempotent: every child of one fork point offers the same pages under
// the same names.
func (f *ForkPoint) Share(ctx context.Context) error {
	if f.checkpoint == nil {
		return nil
	}
	for name, source := range f.checkpoint.sources {
		if err := source.Share(ctx, f.checkpoint.ref, name); err != nil {
			return err
		}
	}
	return nil
}

// beginFork takes this VM's seal for a fork about to be taken. Only one is
// outstanding at a time, because only one checkpoint of a memory region is.
func (vm *VM) beginFork() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if err := vm.readyLocked(); err != nil {
		return err
	}
	if vm.baseIndex == nil || vm.root {
		// A child whose own root index is not published yet has no checkpoint
		// of its own to pin, and its parent's is pinned in another record.
		return ErrForkPending
	}
	if vm.sealed {
		return ErrSealed
	}
	vm.sealed = true
	return nil
}

// unseal gives the seal back, which a failed fork does and a retired point does.
func (vm *VM) unseal() {
	vm.mu.Lock()
	vm.sealed = false
	vm.mu.Unlock()
}

// forkPoint takes the pause itself: one consistent view of every volume at the
// generation the seal froze, over the checkpoint this handle sits on.
func (vm *VM) forkPoint(state []byte, sources map[string]DirtySource) (*ForkPoint, error) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if err := vm.readyLocked(); err != nil {
		return nil, err
	}
	if vm.baseIndex == nil || vm.root {
		return nil, ErrForkPending
	}
	for name := range sources {
		if vm.byName[name] == nil {
			return nil, ErrUnknownVolume
		}
	}
	// The point takes a sequence of its own and never publishes under it, so the
	// identity it gives the parent's unpublished pages is shared by every child
	// of this point and taken by nothing else, ever.
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
		sizes:       make(map[string]uint64, len(vm.names)),
		geometry:    vm.geometries(),
		position:    vm.applied,
		state:       state,
		hasState:    state != nil,
		done:        make(chan struct{}),
		swept:       make(chan struct{}),
	}
	// The seal is this point's from here: it lasts as long as the children
	// taken from the point rather than as long as an upload, wherever those
	// children run, so nothing waiting for the pager's dirty budget waits on it.
	for _, source := range sources {
		source.Hold()
	}
	ckpt.base = newSealedSource(vm.base, ckpt.ref, sources, ckpt.geometry)
	point := &ForkPoint{checkpoint: ckpt, ref: vm.baseIndex.Ref(), index: vm.baseIndex,
		unpublished: make(map[string][]uint64, len(vm.names)), control: vm.control}
	for ordinal, name := range vm.names {
		ckpt.overlays[name] = vm.overlays[ordinal]
		ckpt.sizes[name] = vm.volumes[ordinal].size
		if pages := changedPages(ckpt.overlays[name], sources[name], ckpt.geometry[name]); len(pages) > 0 {
			point.unpublished[name] = pages
		}
	}
	vm.next++
	return point, nil
}

// Inherit rebuilds, on a host that never held the parent, the point a child
// starts from: the parent's published checkpoint, which the parent pinned
// before the handoff. The pages the parent holds that no checkpoint has are not
// in it — the child's pager pulls them out of the parent's page server, and the
// child's first checkpoint publishes them.
func (m *Manager) Inherit(ctx context.Context, parent control.Ref) (*ForkPoint, error) {
	if parent.VM == "" || parent.Sequence == 0 {
		return nil, ErrInvalidConfig
	}
	index, err := m.config.Store.Open(ctx, parent)
	if err != nil {
		return nil, err
	}
	return &ForkPoint{ref: parent, index: index}, nil
}

// InheritPublished is the point a new VM starts from one published checkpoint
// of a VM nothing need be running: a stopped VM, whose last checkpoint is its
// whole state. A zero sequence names the checkpoint the parent's record
// selects.
//
// Nobody holds the parent's epoch, so the pin is written without it
// (control.Client.Pin). It comes before the index is read, because a child
// that exists while what it inherits is unpinned could have it reclaimed under
// it. Only the published checkpoint the record selects, or one a pin already
// keeps, can be pinned this way; any other is refused with
// control.ErrNotPublished. A parent that turns out to be running goes on
// running, and its writer carries the pin on.
//
// child is the VM the point is for. A child of another tenant is refused before
// anything is pinned, because no page crosses between tenants.
func (m *Manager) InheritPublished(ctx context.Context, child string, parent control.Ref) (*ForkPoint, error) {
	if !validID(parent.VM) || !validID(child) {
		return nil, ErrInvalidConfig
	}
	if err := sameTenant(child, parent.VM); err != nil {
		return nil, err
	}
	pinned, err := m.config.Control.Pin(ctx, parent.VM, parent.Sequence)
	if err != nil {
		return nil, err
	}
	return m.Inherit(ctx, pinned)
}

// Fork creates the VM a fork point starts: a control record selecting a root
// index over the parent's pinned checkpoint, and a handle that reads through
// the point until it publishes that root itself.
//
// The pin on the parent comes first, because a child that exists while what it
// inherits is unpinned could have it reclaimed under it. It is the same pin the
// point already took, so this costs a read of the parent's record and no write;
// a fork that fails after it leaves it behind, which costs the parent's
// checkpoint nothing but eagerness — the pin says a fork may read through that
// checkpoint, and one that never started reads nothing.
//
// Nothing is uploaded here. The child's first checkpoint is its root index: it
// publishes the pages it inherited as its own, and only then is the child a VM
// any host can open. A fork that ends before that leaves no object behind.
func (m *Manager) Fork(ctx context.Context, id string, point *ForkPoint) (*VM, error) {
	if !validID(id) || point == nil || point.index == nil {
		return nil, ErrInvalidConfig
	}
	if err := sameTenant(id, point.ref.VM); err != nil {
		return nil, err
	}
	if err := m.usable(); err != nil {
		return nil, err
	}
	if err := point.Pin(ctx); err != nil {
		return nil, err
	}
	// The child's first checkpoint is published under an epoch of its own, so a
	// child named after a VM that lived before it inherits none of its
	// sequences, its page identities or its object keys.
	handle, err := m.config.Control.Create(ctx, id, rootSequence(m.config.Control.NewEpoch()), false)
	if errors.Is(err, control.ErrExists) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	if err := point.Hold(); err != nil {
		return nil, err
	}
	vm, err := m.attach(ctx, id, handle, point.index, nil, point)
	if err != nil {
		// The record is written before the handle exists, and a child's record
		// selects a root only that child's first checkpoint publishes: one left
		// behind is an identity nothing can use again — creating it reports that
		// it exists and opening it reports a fork still pending. Nothing else
		// can be holding it, because this call is what created it.
		undo := context.WithoutCancel(ctx)
		return nil, errors.Join(err, m.config.Control.Delete(undo, id), point.Retire(undo))
	}
	return vm, nil
}

// inheritedPages is what a child's root checkpoint republishes beyond its own
// dirty state: the parent's unpublished pages, which it reads through the
// point. A child on another host has none here — its pager pulled them, so they
// are its own dirty state and its checkpoint reports them like any other.
func (f *ForkPoint) inheritedPages() map[string][]uint64 {
	if f == nil || f.checkpoint == nil {
		return nil
	}
	return maps.Clone(f.unpublished)
}

// sameTenant refuses a child of a tenant other than its parent's. A fork
// shares the parent's pages by their identity, in the store and in a host's
// memory, so a fork across tenants is the one way a page could cross between
// them.
func sameTenant(child, parent string) error {
	if control.TenantOf(child) != control.TenantOf(parent) {
		return fmt.Errorf("%w: %s cannot inherit from %s", ErrOtherTenant, child, parent)
	}
	return nil
}
