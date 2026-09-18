package volume

import (
	"context"
	"fmt"
)

// Handoff releases this VM to another host without publishing anything. The
// handle is terminal: every later operation on it, including another handoff,
// reports ErrHandedOff.
//
// It is the migration handoff. Nothing is published because nothing needs to
// be: the source's pager still holds the pages of everything written since the
// last checkpoint, and the destination faults those pages out of it rather than
// out of storage. Uploading them inside the pause is exactly the cost a
// post-copy migration exists to avoid; the destination's next interval
// checkpoint publishes them. What the destination opens is therefore the
// checkpoint the control record already selects, and the pages that make it
// current come over the wire.
//
// Handoff waits for a publication another call already had in flight, so a
// checkpoint still uploading when the guest stopped is not left half-selected.
// Callers quiesce writes first, as for Close.
//
// A fork that has not published its own root index is refused with
// ErrForkPending. This handle is the only thing that could ever publish that
// root: releasing it without publishing leaves an identity no host can open,
// and the fork point it reads through is retired by nobody, so the parent stays
// sealed for good — never checkpointed, never fenced, never migratable. Close is
// what gives such a fork up, and it publishes nothing either.
//
// A Handoff canceled before it made the handle terminal can be repeated. One
// canceled while releasing cannot — the handle is already handed off, so it
// reports ErrHandedOff — and Close finishes the release instead.
func (vm *VM) Handoff(ctx context.Context) error {
	if err := vm.pubMu.Lock(ctx); err != nil {
		return err
	}
	vm.pubMu.Unlock()
	vm.mu.Lock()
	if err := vm.readyLocked(); err != nil {
		vm.mu.Unlock()
		return err
	}
	if vm.root {
		vm.mu.Unlock()
		return fmt.Errorf("%w: %s has not published its own root index", ErrForkPending, vm.id)
	}
	vm.handedOff = true
	vm.publishLocked()
	vm.mu.Unlock()
	return vm.stop(ctx)
}
