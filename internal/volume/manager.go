package volume

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
)

// rootSequence is the sequence a VM's first checkpoint is published under: the
// first counter of the epoch its creating handle drew. It is knowable before
// the control record exists, which is what lets that checkpoint be published
// first, and the epoch is drawn rather than fixed so that two VMs created under
// one identity never publish under the same sequence.
func rootSequence(epoch uint64) uint64 { return control.Sequence(epoch, 1) }

// nextSequence is the first sequence a handle at this epoch may publish under.
// Sequences are epoch-major, so a handle never reuses one a fenced predecessor
// could still be uploading objects for.
func nextSequence(epoch, selected uint64) uint64 {
	if control.EpochOf(selected) == epoch {
		return selected + 1
	}
	return control.Sequence(epoch, 1)
}

// Create publishes the root of a new VM as its first checkpoint and then
// writes the control record that selects it. Every volume starts at its
// declared size and reads as zeroes. A VM whose control record already exists
// is opened instead, which is how a create interrupted after its record is
// finished by repeating it.
//
// The identity must be unused: a create is refused with ErrIdentityUsed when
// anything is already stored under it and no record accounts for it. What is
// there is either the checkpoints a deleted VM left pinned, which a fork still
// reads through, or a create interrupted before its record — and in both cases
// a VM published here would write into keys that are not its own. Identities
// are never reused, so this is a mistake rather than a state to recover from:
// the orchestrator allocates a fresh one.
//
// The epoch the first checkpoint is published under is drawn rather than fixed,
// so that even an identity handed out again cannot collide with what the VM
// before it published, or be served its pages out of a page cache.
func (m *Manager) Create(ctx context.Context, id string, volumes []VolumeSpec) (*VM, error) {
	return m.create(ctx, id, volumes, true)
}

// CreateIfAbsent publishes the root of a new VM and the control record that
// selects it, exactly as Create does, and refuses with ErrExists when the
// deployment already records that identity rather than opening it.
//
// It is for a caller that does not allocate the identity and cannot choose
// another: every host names one guest image's template by the image's own
// bytes, so two of them starting together race here, and the one that loses
// must read the winner's record rather than open it — an open takes the epoch,
// and the epoch it would take is the one the winner is importing under.
//
// Objects under the identity that no control record accounts for do not refuse
// it, which is the other half of naming a VM by something other than a fresh
// allocation. They are what a create that ended between its root and its record
// left: nothing can have forked a VM whose record never existed, so nothing
// reads them, and the epoch this create draws is its own, so no key it
// publishes under is one they hold. Create refuses them because an identity it
// is given is one that was allocated and will not be met again, and there a
// leftover is a mistake rather than a state to go on from.
func (m *Manager) CreateIfAbsent(ctx context.Context, id string, volumes []VolumeSpec) (*VM, error) {
	return m.create(ctx, id, volumes, false)
}

// create is both of them: takeOver says what an identity the deployment already
// records means — the VM is opened, which is how a create interrupted after its
// record is finished by repeating it, or the create is refused with ErrExists.
func (m *Manager) create(ctx context.Context, id string, volumes []VolumeSpec, takeOver bool) (*VM, error) {
	if !validID(id) || len(volumes) == 0 {
		return nil, ErrInvalidConfig
	}
	if err := m.usable(); err != nil {
		return nil, err
	}
	specs := slices.Clone(volumes)
	slices.SortFunc(specs, func(a, b VolumeSpec) int { return strings.Compare(a.Name, b.Name) })
	sizes := make(map[string]uint64, len(specs))
	for index, spec := range specs {
		if !validID(spec.Name) || spec.Size == 0 || spec.Size%checkpoint.SectorSize != 0 {
			return nil, ErrInvalidConfig
		}
		if index > 0 && specs[index-1].Name == spec.Name {
			return nil, ErrInvalidConfig
		}
		sizes[spec.Name] = spec.Size
	}
	// A record is what says an identity is a VM's, so it is read before
	// anything is written: one that is there is opened, and one that is not
	// leaves the objects under that identity — if there are any — accounted for
	// by nothing.
	switch _, err := m.config.Control.Read(ctx, id); {
	case err == nil:
		if !takeOver {
			return nil, fmt.Errorf("%w: %s", ErrExists, id)
		}
		return m.Open(ctx, id)
	case !errors.Is(err, platform.ErrNotFound):
		return nil, err
	}
	if takeOver {
		used, err := m.config.Store.Used(ctx, id)
		if err != nil {
			return nil, err
		}
		if used {
			return nil, fmt.Errorf("%w: %s", ErrIdentityUsed, id)
		}
	}
	first := rootSequence(m.config.Control.NewEpoch())
	root, err := m.config.Store.Root(ctx, control.Ref{VM: id, Sequence: first}, sizes)
	if err != nil {
		return nil, err
	}
	handle, err := m.config.Control.Create(ctx, id, first, true)
	if errors.Is(err, control.ErrExists) {
		if !takeOver {
			return nil, fmt.Errorf("%w: %s", ErrExists, id)
		}
		return m.Open(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	// The root is this handle's own publication, so it is what the first
	// checkpoint over it reclaims: leaving it behind would cost every VM ever
	// created one checkpoint nothing reads.
	return m.attach(ctx, id, handle, root, root, nil)
}

// Open takes the VM over and returns a handle on it: it advances the epoch in
// the control record, which fences whatever host held it before, and reads the
// root of the checkpoint that record selects. The overlays start empty —
// nothing is durable between checkpoints, so there is nothing to replay.
//
// It reads two objects, the control record and that root, and no page: pages
// are read lazily by the reads that need them. It waits for no publication,
// including one the host that handed this VM over still has in flight, which is
// what lets a migration's destination start as soon as the record is its own.
//
// A VM whose control record says its selected checkpoint has never been
// published — a fork that has not published its own first checkpoint — reports
// ErrForkPending, and the epoch is left alone so the host that holds that fork
// is not fenced by an open that could never have succeeded.
func (m *Manager) Open(ctx context.Context, id string) (*VM, error) {
	if !validID(id) {
		return nil, ErrInvalidConfig
	}
	if err := m.usable(); err != nil {
		return nil, err
	}
	// The record is read before the epoch is taken, so discovering that a
	// fork never published its root costs the host that holds it nothing.
	record, err := m.config.Control.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	if !record.Created {
		return nil, ErrForkPending
	}
	handle, err := m.config.Control.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	index, err := m.index(ctx, id, handle.Record().Selected)
	if err != nil {
		handle.Close()
		return nil, err
	}
	return m.attach(ctx, id, handle, index, nil, nil)
}

// Delete removes a VM's control record, after which nothing can open it, and
// then the checkpoint objects nothing else reads. Close the VM's writer first.
//
// The objects go because the identity must be usable again: a create is refused
// while anything is stored under an identity no record accounts for, so what
// this sweep leaves behind is a name nothing may be created under. The record
// goes before them, so nothing can open the VM while its objects are being
// removed.
//
// What the sweep spares is the checkpoints this VM's record pinned, and every
// checkpoint their indexes name. Those are the points this VM was forked at,
// and a descendant of it — a child, or a grandchild whose own index names these
// directly — may still read through them. This VM has no way to find out
// whether one does, so the objects stay: a collector reclaims them, once it has
// surveyed the deployment and found that nothing reads them. The record goes
// either way, because the identity must be free.
//
// The record is therefore the only thing that says what a sweep may take, and a
// VM that has none is not swept at all. That is not only an interrupted delete:
// a finished delete of a VM that was ever forked is exactly a record-less VM
// whose objects a fork still reads, so a repeat that swept what it found
// would destroy them. Repeating a delete is harmless and finishes nothing; what
// an interrupted sweep left is a collector's, like everything else no record
// accounts for.
//
// A record that cannot be read is not deleted at all. Its pins are exactly what
// the sweep would have to spare, and a sweep that cannot read them would take
// checkpoints out from under whoever reads them; an identity nobody can delete is the
// lesser loss, and the record can be repaired.
func (m *Manager) Delete(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrInvalidConfig
	}
	record, err := m.config.Control.Read(ctx, id)
	if errors.Is(err, platform.ErrNotFound) {
		// Nothing left that says which of this identity's objects are read.
		return nil
	}
	if err != nil {
		return err
	}
	if err := m.config.Control.Delete(ctx, id); err != nil {
		return err
	}
	return m.config.Store.DeleteVM(ctx, id, record.Pinned)
}

// attach builds and starts a handle over the control record it holds. selected is
// the index of the checkpoint the record selects — the parent's, for a fork —
// owned that same index when this handle is the one that published it, and
// point the fork point a fork reads through until it publishes its own root.
func (m *Manager) attach(ctx context.Context, id string, handle *control.Handle,
	selected, owned *checkpoint.Index, point *ForkPoint) (*VM, error) {
	base, specs, err := resolve(m.config.Store, selected, point)
	if err == nil {
		vm := newVM(m, id, handle, base, selected, owned, specs, point)
		if err = vm.start(ctx); err == nil {
			return vm, nil
		}
		m.release(vm)
		return nil, err
	}
	handle.Close()
	return nil, err
}

// resolve reports what a handle sits on: the checkpoint index its control
// record selects, or, for a fork, the parent's checkpoint with the point the
// fork was taken at over it. A fork's volumes are its parent's.
func resolve(store *checkpoint.Store, selected *checkpoint.Index, point *ForkPoint) (source, []VolumeSpec, error) {
	if selected == nil {
		return nil, nil, ErrCorrupt
	}
	specs := make([]VolumeSpec, 0, len(selected.Volumes()))
	for _, name := range selected.Volumes() {
		specs = append(specs, VolumeSpec{Name: name, Size: selected.Size(name)})
	}
	if point != nil && point.checkpoint != nil {
		return point.checkpoint, specs, nil
	}
	return indexSource{store: store, index: selected}, specs, nil
}

// index reads the root of one selected checkpoint. The record said that
// checkpoint was published, so one that is not there is durable state
// disagreeing with itself rather than a fork still starting: a record whose
// first checkpoint has not landed says so, and Open reports ErrForkPending from
// the record itself.
func (m *Manager) index(ctx context.Context, id string, sequence uint64) (*checkpoint.Index, error) {
	index, err := m.config.Store.Open(ctx, control.Ref{VM: id, Sequence: sequence})
	if errors.Is(err, platform.ErrNotFound) {
		return nil, errors.Join(ErrCorrupt, err)
	}
	return index, err
}
