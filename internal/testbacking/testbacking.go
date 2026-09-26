// Package testbacking wraps a volume so a simulated run decides the order in
// which memory regions reach it. A pager's memory regions fault concurrently and their
// volumes' loads complete out of order, so a test that must replay the same
// interleaving twice has to name the caller of every load rather than let I/O
// completion order stand in for it. Every harness that drives a pager under
// platform/sim needs the same wrapper, which is why it is here rather
// than copied into each of them.
package testbacking

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The calls this wrapper admits, named as the task they run under and as the
// adapter resource they compete for. They are also what Admitted is given.
const (
	Load   = "load"
	Verify = "verify"
)

// Admitting forwards a vmmemory.Backing's calls through the simulated
// runtime's admission under one task name, so the scheduler orders this
// memory region's work against every other task's.
type Admitting struct {
	vmmemory.Backing
	runtime *sim.Runtime
	task    string
	// Admitted, when set, runs after a call is admitted and before it is
	// forwarded, named by the call. It is where a harness counts what a volume
	// was asked to do.
	Admitted func(call string)

	// mu guards disagreement, the first load whose two answers were not one.
	mu           sync.Mutex
	disagreement error
}

// Disagreement reports the first load of a backing whose pages can come from
// another host that answered where a page's bytes are differently from its
// Locate, nil while there has been none. See peerAdmitting.agree.
func (b *Admitting) Disagreement() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.disagreement
}

// New wraps backing and reports the value to attach beside the wrapper itself,
// which is where Admitted is set. The task name must be unique among the tasks
// that run concurrently with this one, which for a pager's memory regions means the VM
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
// memory region's fault costs its volume what a real one's does.
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
	unpublished, err := b.Backing.(vmmemory.UnpublishedLoader).LoadUnpublished(ctx, offset, dst)
	if err == nil {
		b.agree(ctx, offset, uint64(len(dst)), unpublished)
	}
	return unpublished, err
}

// agree requires the two answers such a backing gives about where a page's
// bytes are to be one answer. Locate reports no identity for exactly the pages
// whose only copy is the source's, and a load that succeeded reports exactly
// those pages as the source's own. The pager believes both: the first decides
// what it shares under a name and what its retire may give up, the second what
// it keeps as the guest's own. Where the two disagree, one of those decisions
// is made on the wrong answer, and some guest reads bytes that are not its
// page's — whether or not this run goes on to read them.
//
// The check is the answers against each other rather than either against a
// rule of its own, so it holds whatever rule the backing keeps. It is recorded
// rather than returned: a failed load is something the pager and the harness
// both know how to excuse.
func (b peerAdmitting) agree(ctx context.Context, offset, length uint64, unpublished []bool) {
	paged, ok := b.Backing.(vmmemory.PagedBacking)
	if !ok {
		return
	}
	size := paged.PageSize()
	extents, err := b.Backing.Locate(ctx, offset, length)
	if err != nil {
		// A fault on the volume's metadata says nothing about the two answers,
		// so this load goes unchecked rather than counted against the backing.
		slog.WarnContext(ctx, "testbacking: locating a load to check it", "task", b.task, "error", err)
		return
	}
	for _, extent := range extents {
		for page := max(extent.Offset, offset) / size; page*size < min(extent.Offset+extent.Length, offset+length); page++ {
			index := page - offset/size
			own := index < uint64(len(unpublished)) && unpublished[index]
			stripped := extent.Identity.Ref.IsZero() && !extent.Identity.Zero
			if own == stripped {
				continue
			}
			b.mu.Lock()
			if b.disagreement == nil {
				b.disagreement = fmt.Errorf("%s: page %d loads as the source's own = %t, and Locate reports %+v",
					b.task, page, own, extent.Identity)
			}
			b.mu.Unlock()
		}
	}
}

// InstalledUnpublished forwards the pager's report of which of those pages the
// memory region went on to hold. It is admitted through nothing: it moves no bytes and
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
