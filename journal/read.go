package journal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/semistrict/sproutfs/platform/sim"
)

var (
	// ErrFenced reports an entry of a VM that a read fenced at a newer
	// epoch.
	ErrFenced = errors.New("journal: the VM is fenced at a newer epoch")
	// ErrGeneration reports a read of a generation the journal is not: its
	// disk was formatted again, and the entries asked for are gone.
	ErrGeneration = errors.New("journal: the journal is of another generation")
	// ErrTrimmed reports an entry trimmed and written over while it was read.
	ErrTrimmed = errors.New("journal: an entry was trimmed while it was read")
)

// ReadRequest asks for one VM's entries of one epoch: JOURNAL_READ.
type ReadRequest struct {
	VM string
	// Epoch is the epoch whose entries are asked for.
	Epoch uint64
	// After is the covered position: only entries after it are returned.
	After uint64
	// Generation is the journal's generation the control record names.
	Generation uint64
	// Reader is the reader's own epoch, newer than Epoch. The VM is fenced
	// at it.
	Reader uint64
}

// Read is the server side of JOURNAL_READ. It fences the VM at the reader's
// epoch, so no entry of an older epoch is placed from then on, and every
// commit of one fails. It waits for the batches placed before the fence, so
// every entry any commit of the VM was answered for is among what it reads.
// Then it reads the VM's live entries of the epoch after the covered
// position from the disk, checks each, and hands them to yield in position
// order. Read stops at the first error yield returns.
func (j *Journal) Read(ctx context.Context, request ReadRequest, yield func(Entry) error) error {
	if request.Generation != j.generation {
		return fmt.Errorf("%w: %d, not %d", ErrGeneration, request.Generation, j.generation)
	}
	if request.Reader <= request.Epoch {
		return fmt.Errorf("journal: a reader at epoch %d may not read %s's epoch %d", request.Reader, request.VM,
			request.Epoch)
	}
	j.mu.Lock()
	if err := j.refusal(); err != nil {
		j.mu.Unlock()
		return err
	}
	if j.fences[request.VM] < request.Reader && !sim.Bug(ctx, "journal-read-without-fence") {
		j.fences[request.VM] = request.Reader
	}
	for placed := j.placed; j.completed < placed; {
		changed := j.changed
		j.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		j.mu.Lock()
	}
	var wanted []indexed
	for epoch, h := range j.held[request.VM] {
		if epoch != request.Epoch && !sim.Bug(ctx, "journal-replay-any-epoch") {
			continue
		}
		for _, e := range h.entries {
			if e.position > request.After {
				wanted = append(wanted, *e)
			}
		}
	}
	slices.SortFunc(wanted, func(a, b indexed) int { return cmp.Compare(a.position, b.position) })
	j.mu.Unlock()
	reader := j.reader()
	for _, e := range wanted {
		offset, _ := j.offset(e.position)
		b, err := reader.read(ctx, offset, e.length)
		if err != nil {
			return fmt.Errorf("journal: reading the entry at %d: %w", e.position, err)
		}
		h, entry, err := decodeEntry(b)
		if err != nil || h.kind != kindBlocks || h.position != e.position || h.generation != j.generation {
			return fmt.Errorf("%w: at %d", ErrTrimmed, e.position)
		}
		if err := yield(entry); err != nil {
			return err
		}
	}
	return nil
}
