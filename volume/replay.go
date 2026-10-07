package volume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Replay after a host loss (plans/fsync-journal-2026-10-06.md). A VM's control
// record names the journals that may hold flushes answered after its selected
// checkpoint. An open that is not a migration's reads their entries back, in
// the record's order, and writes their blocks into the VM's volumes before
// anything runs it. A VM something was replayed into is cold booted, since no
// VMM state the selected checkpoint holds describes those disks.

var (
	// ErrJournalPending reports a VM whose record names a journal no member
	// serves yet: its disk is still moving to a survivor. The open took no
	// epoch, and succeeds once the disk is served.
	ErrJournalPending = errors.New("volume: a journal the VM's record names is not served yet")
	// ErrJournalLost reports a VM whose record names a journal whose disk was
	// formatted again: the flushes it held are gone. Only an operator's
	// discard opens such a VM.
	ErrJournalLost = errors.New("volume: a journal the VM's record names was formatted again")
)

// Replayer reads back what the journals a record names hold of a VM.
type Replayer interface {
	// Served reports ErrJournalPending unless some member serves every
	// journal named.
	Served(ctx context.Context, journals []control.Journal) error
	// Replay reads the VM's entries of each journal after its covered
	// position, in the order named and in position order within each, and
	// calls apply for each. The holder fences the VM at reader, the epoch the
	// open took, first. A journal formatted again is journal.ErrGeneration.
	Replay(ctx context.Context, vm string, journals []control.Journal, reader uint64,
		apply func(journal.Entry) error) error
}

// served checks, before an open takes the epoch, that every journal record
// names can be read.
func (m *Manager) served(ctx context.Context, record control.Record) error {
	if len(record.Journals) == 0 {
		return nil
	}
	if m.config.Replay == nil {
		return fmt.Errorf("%w: %s names %d journals and this host reads none", ErrJournalPending, record.VM,
			len(record.Journals))
	}
	return m.config.Replay.Served(ctx, record.Journals)
}

// replay writes into vm what the journals its record names hold of it. With
// discard, a journal that is not served or was formatted again is left out,
// and logged, rather than refusing the open.
func (m *Manager) replay(ctx context.Context, vm *VM, journals []control.Journal, discard bool) error {
	if len(journals) == 0 {
		return nil
	}
	if m.config.Replay == nil {
		if !discard {
			return fmt.Errorf("%w: %s names %d journals and this host reads none", ErrJournalPending, vm.id,
				len(journals))
		}
		slog.WarnContext(ctx, "volume: a VM's journals were discarded unread: this host reads none", "vm", vm.id,
			"journals", len(journals))
		return nil
	}
	entries, blocks := 0, 0
	apply := func(entry journal.Entry) error {
		target := vm.byName[entry.Volume]
		if target == nil || target.ephemeral {
			return fmt.Errorf("%w: a journal entry of %s names volume %q, which it has no durable volume of",
				ErrCorrupt, vm.id, entry.Volume)
		}
		extents := make([]WriteExtent, 0, len(entry.Blocks))
		for index, block := range entry.Blocks {
			data := entry.Data[index*journal.BlockBytes : (index+1)*journal.BlockBytes]
			extents = append(extents, WriteExtent{Offset: block * journal.BlockBytes, Data: data})
		}
		for len(extents) > 0 {
			batch := min(len(extents), max(1, m.config.MaxWriteBytes/journal.BlockBytes))
			if err := target.WriteBatch(ctx, extents[:batch]); err != nil {
				return fmt.Errorf("replaying a journal entry of %s into %s: %w", vm.id, entry.Volume, err)
			}
			extents = extents[batch:]
		}
		entries, blocks = entries+1, blocks+len(entry.Blocks)
		return nil
	}
	var err error
	if discard {
		var discarded int
		if discarded, err = m.replayDiscarding(ctx, vm, journals, apply, &entries); err == nil && discarded > 0 {
			// The record names the journals discarded until a selection
			// names others, and every open meanwhile would find them lost
			// again. This checkpoint's selection names none.
			err = vm.checkpoint(ctx, true)
		}
	} else {
		err = m.config.Replay.Replay(ctx, vm.id, journals, vm.Epoch(), apply)
	}
	if errors.Is(err, journal.ErrGeneration) {
		return fmt.Errorf("%w: %v", ErrJournalLost, err)
	}
	if err != nil {
		return err
	}
	vm.replayed = entries > 0
	slog.InfoContext(ctx, "volume: a VM's journals were replayed", "vm", vm.id, "journals", len(journals),
		"entries", entries, "blocks", blocks)
	return nil
}

// replayDiscarding replays the journals one at a time, so one that is not
// served or was formatted again is left out of what the others hold, and
// logged. It reports how many it left out. A failure part way through a journal still fails the open: the
// entries before it were written, and a journal that can be read is never
// discarded.
func (m *Manager) replayDiscarding(ctx context.Context, vm *VM, journals []control.Journal,
	apply func(journal.Entry) error, entries *int) (int, error) {
	discarded := 0
	for _, named := range journals {
		one := []control.Journal{named}
		err := m.config.Replay.Served(ctx, one)
		if err == nil {
			before := *entries
			if err = m.config.Replay.Replay(ctx, vm.id, one, vm.Epoch(), apply); err != nil && *entries != before {
				return discarded, err
			}
		}
		if errors.Is(err, ErrJournalPending) || errors.Is(err, journal.ErrGeneration) {
			slog.WarnContext(ctx, "volume: a journal of a VM was discarded, with the flushes it held", "vm", vm.id,
				"disk", fmt.Sprintf("%x", named.Disk), "generation", named.Generation, "epoch", named.Epoch,
				"covered", named.Covered, "why", err)
			discarded++
			continue
		}
		if err != nil {
			return discarded, err
		}
	}
	return discarded, nil
}

// abandonReplay closes a VM whose replay failed without publishing what it
// replayed: a checkpoint of part of the journals' entries would select a
// state no flush ever saw, and drop the journals that hold the rest.
func (vm *VM) abandonReplay(ctx context.Context, cause error) error {
	if !sim.Bug(ctx, "volume-publish-a-partial-replay") {
		vm.mu.Lock()
		vm.poisoned = cause
		vm.publishLocked()
		vm.mu.Unlock()
	}
	return vm.Close(ctx)
}

// Replayed reports whether opening this VM wrote flushed blocks from its
// journals into it. Its selected checkpoint's VMM state, if it has one, then
// describes disks that are no longer there: the VM is cold booted.
func (vm *VM) Replayed() bool { return vm.replayed }
