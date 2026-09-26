package volume

import (
	"context"

	"github.com/semistrict/sproutfs/control"
)

// Release gives up a kept checkpoint of a VM that nothing need be running, and
// deletes what only that checkpoint held. A checkpoint a fork was taken from is
// refused with control.ErrForked, because the fork's pin is permanent; one the
// record does not keep is refused with control.ErrNotKept.
//
// The release is a conditional write without the VM's epoch
// (control.Client.Release), so a writer that runs the VM adopts it, and a pin
// of the same checkpoint is ordered against it by the record.
//
// The sweep behind it is reclamation with the released checkpoint in place of
// the replaced one: every checkpoint of this VM the released index names, and
// the released checkpoint itself, less what the selected index names and what
// stays pinned or kept. Nothing newer than the selected checkpoint can name
// anything the selected index does not, so nothing a writer publishes after
// the record was read loses a page to it. A released checkpoint that is still
// the selected one stays until a later selection replaces it.
//
// The release is what the caller asked for, and it has landed once the record
// is written. A sweep that fails leaves unreferenced objects, as a failed
// reclamation does, and is reported rather than returned.
func (m *Manager) Release(ctx context.Context, vm string, sequence uint64) error {
	if !validID(vm) || sequence == 0 {
		return ErrInvalidConfig
	}
	if err := m.usable(); err != nil {
		return err
	}
	record, err := m.config.Control.Release(ctx, vm, sequence)
	if err != nil {
		return err
	}
	if err := m.sweepReleased(ctx, control.Ref{VM: vm, Sequence: sequence}, record); err != nil {
		report(ctx, "volume: reclaiming what a released checkpoint held failed", vm, err)
	}
	return nil
}

// sweepReleased deletes what only a released checkpoint held, against the
// record its release left.
func (m *Manager) sweepReleased(ctx context.Context, released control.Ref, record control.Record) error {
	if released.Sequence == record.Selected || !record.Created {
		return nil
	}
	store := m.config.Store
	previous, err := store.Open(ctx, released)
	if err != nil {
		return err
	}
	// A later selection may have replaced the one the release read, and its
	// sweep taken it. Nothing is deleted then: what only the released
	// checkpoint held is left unreferenced, for a collector.
	current, err := store.Open(ctx, control.Ref{VM: released.VM, Sequence: record.Selected})
	if err != nil {
		return err
	}
	return store.Reclaim(ctx, previous, current, record.Protected())
}
