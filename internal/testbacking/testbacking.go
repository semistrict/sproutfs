// Package testbacking wraps a volume so a simulated run decides the order in
// which regions reach it. A pager's regions fault concurrently and their
// volumes' loads complete out of order, so a test that must replay the same
// interleaving twice has to name the caller of every load rather than let I/O
// completion order stand in for it. Every harness that drives a pager under
// internal/platform/sim needs the same wrapper, which is why it is here rather
// than copied into each of them.
package testbacking

import (
	"context"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// The calls this wrapper admits, named as the task they run under and as the
// adapter resource they compete for. They are also what Admitted is given.
const (
	Load   = "load"
	Verify = "verify"
)

// Admitting forwards a vmmemory.Backing's calls through the simulated
// runtime's admission under one task name, so the scheduler orders this
// region's work against every other task's.
type Admitting struct {
	vmmemory.Backing
	runtime *sim.Runtime
	task    string
	// Admitted, when set, runs after a call is admitted and before it is
	// forwarded, named by the call. It is where a harness counts what a volume
	// was asked to do.
	Admitted func(call string)
}

// New wraps backing and reports the value to attach beside the wrapper itself,
// which is where Admitted is set. The task name must be unique among the tasks
// that run concurrently with this one, which for a pager's regions means the VM
// and the volume together.
//
// The two results differ because a pager reads what a backing can do from the
// methods it has: a backing whose pages can come from another host is attached
// through a wrapper that reports them, one a fault can ask for part of a window
// through a wrapper that forwards that, and an ordinary one through neither. A
// single wrapper claiming everything would make every simulated volume look
// like a migration destination's, and none of them look like a volume a fault
// can ask for the window it needs.
func New(backing vmmemory.Backing, runtime *sim.Runtime, task string) (vmmemory.Backing, *Admitting) {
	admitting := &Admitting{Backing: backing, runtime: runtime, task: task}
	switch backing.(type) {
	case vmmemory.UnpublishedLoader:
		return peerAdmitting{admitting}, admitting
	case vmmemory.SparseLoader:
		return sparseAdmitting{admitting}, admitting
	}
	return admitting, admitting
}

// peerAdmitting wraps a backing whose loads can return bytes its volume does
// not hold, which is a migration destination's.
type peerAdmitting struct{ *Admitting }

// sparseAdmitting wraps a backing a fault can ask for part of a window, which
// every volume is.
type sparseAdmitting struct{ *Admitting }

// LoadPages forwards the wrapped backing's masked window read, so a simulated
// region's fault costs its volume what a real one's does.
func (b sparseAdmitting) LoadPages(ctx context.Context, offset uint64, dst []byte, wanted []bool) error {
	ctx, err := b.admit(ctx, Load)
	if err != nil {
		return err
	}
	return b.Backing.(vmmemory.SparseLoader).LoadPages(ctx, offset, dst, wanted)
}

func (b *Admitting) admit(ctx context.Context, call string) (context.Context, error) {
	ctx = sim.WithTask(ctx, b.task+"/"+call)
	if err := b.runtime.Admit(ctx, "volume/"+call); err != nil {
		return nil, err
	}
	if b.Admitted != nil {
		b.Admitted(call)
	}
	return ctx, nil
}

func (b *Admitting) Load(ctx context.Context, offset uint64, dst []byte) error {
	ctx, err := b.admit(ctx, Load)
	if err != nil {
		return err
	}
	return b.Backing.Load(ctx, offset, dst)
}

func (b *Admitting) Verify(ctx context.Context) error {
	ctx, err := b.admit(ctx, Verify)
	if err != nil {
		return err
	}
	return b.Backing.Verify(ctx)
}

// LoadUnpublished forwards the wrapped backing's report of which pages a
// migration source served out of its own dirty pages, so a destination's
// pager keeps them.
func (b peerAdmitting) LoadUnpublished(ctx context.Context, offset uint64, dst []byte) ([]bool, error) {
	ctx, err := b.admit(ctx, Load)
	if err != nil {
		return nil, err
	}
	return b.Backing.(vmmemory.UnpublishedLoader).LoadUnpublished(ctx, offset, dst)
}

// InstalledUnpublished forwards the pager's report of which of those pages the
// region went on to hold. It is admitted through nothing: it moves no bytes and
// takes no lock, so there is no ordering here for a run to decide.
func (b peerAdmitting) InstalledUnpublished(offset uint64, installed []bool) {
	if held, ok := b.Backing.(vmmemory.UnpublishedInstaller); ok {
		held.InstalledUnpublished(offset, installed)
	}
}

var (
	_ vmmemory.UnpublishedLoader    = peerAdmitting{}
	_ vmmemory.UnpublishedInstaller = peerAdmitting{}
	_ vmmemory.SparseLoader         = sparseAdmitting{}
)
