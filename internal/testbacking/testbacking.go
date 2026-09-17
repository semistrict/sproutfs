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

// New wraps backing. The task name must be unique among the tasks that run
// concurrently with this one, which for a pager's regions means the VM and the
// volume together.
func New(backing vmmemory.Backing, runtime *sim.Runtime, task string) *Admitting {
	return &Admitting{Backing: backing, runtime: runtime, task: task}
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
// migration source served out of its own dirty frames, so a destination's
// pager keeps them. A backing that does not track them is loaded plainly.
func (b *Admitting) LoadUnpublished(ctx context.Context, offset uint64, dst []byte) ([]bool, error) {
	tracked, ok := b.Backing.(vmmemory.UnpublishedLoader)
	if !ok {
		return nil, b.Load(ctx, offset, dst)
	}
	ctx, err := b.admit(ctx, Load)
	if err != nil {
		return nil, err
	}
	return tracked.LoadUnpublished(ctx, offset, dst)
}

// InstalledUnpublished forwards the pager's report of which of those pages the
// region went on to hold. It is admitted through nothing: it moves no bytes and
// takes no lock, so there is no ordering here for a run to decide.
func (b *Admitting) InstalledUnpublished(offset uint64, installed []bool) {
	if held, ok := b.Backing.(vmmemory.UnpublishedInstaller); ok {
		held.InstalledUnpublished(offset, installed)
	}
}

var _ vmmemory.UnpublishedInstaller = (*Admitting)(nil)
