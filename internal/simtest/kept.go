package simtest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// Keep takes a checkpoint of the named VM that its writer keeps: of its disks
// alone, or of its disks, its memory and its VMM state. It is Checkpoint or
// CheckpointDisks otherwise, and fails as they fail under a fault.
func (w *World) Keep(ctx context.Context, id string, disks bool) error {
	if disks {
		return w.checkpointDisks(ctx, id, volume.Terms{Keep: true})
	}
	return w.checkpoint(ctx, id, volume.Terms{Keep: true})
}

// noteKept records the pause a checkpoint asked to be kept stands for.
func (w *World) noteKept(id string, at durableState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.kept[id] == nil {
		w.kept[id] = map[uint64]durableState{}
	}
	w.kept[id][at.sequence] = at
}

// keptPause is the pause one kept checkpoint of a VM stands for, if a writer
// of this world asked for it.
func (w *World) keptPause(id string, sequence uint64) (durableState, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	at, found := w.kept[id][sequence]
	return at, found
}

// KeptOf reports the checkpoints the named VM's control record keeps and this
// world knows the pause of, in ascending order, and which of them a fork was
// taken from. It reads the record straight out of the store, so a fault that
// has taken the store away reports nothing kept.
func (w *World) KeptOf(ctx context.Context, id string) (kept []uint64, forked map[uint64]bool) {
	records, err := w.records()
	if err != nil {
		return nil, nil
	}
	record, err := records.Read(ctx, id)
	if err != nil {
		return nil, nil
	}
	forked = map[uint64]bool{}
	for _, entry := range record.Kept {
		if _, known := w.keptPause(id, entry.Sequence); known {
			kept = append(kept, entry.Sequence)
			forked[entry.Sequence] = record.IsPinned(entry.Sequence)
		}
	}
	return kept, forked
}

// CreateFromKept creates the VM spec names from one kept checkpoint of its
// parent, on the host the spec puts it on, as a create names one: the
// checkpoint pinned without the parent's writer, the fork, and the root. A
// checkpoint that holds VMM state resumes the guest from it unless cold asks
// for a shape, which boots it cold; one without state always boots cold.
//
// What the child reads, through its own fault path before it stores anything,
// must be exactly the kept checkpoint's bytes — its memory as well when it
// resumed, zeroes there when it booted — and never a byte its parent wrote
// after the pause. A resumed guest continues at that pause's store counter.
//
// Under a fault the create may not happen: the pin, the fork or the root can
// be refused, and a child whose root never published is given up.
func (w *World) CreateFromKept(ctx context.Context, spec VMSpec, sequence uint64, cold bool) error {
	if !spec.Kept || w.Exists(spec.ID) {
		return nil
	}
	pause, known := w.keptPause(spec.Parent, sequence)
	if !known {
		return fmt.Errorf("%s: %s kept no checkpoint %d this world asked for", spec.ID, spec.Parent, sequence)
	}
	running := w.up(spec.Host)
	if running == nil {
		return nil
	}
	h := w.hosts[spec.Host]
	point, err := running.Volumes().InheritPublished(ctx, spec.ID, control.Ref{VM: spec.Parent, Sequence: sequence})
	if err != nil {
		w.logf("%s: %s could not pin %s/%d: %v", spec.ID, h.name, spec.Parent, sequence, err)
		return nil
	}
	vm, _, err := host.CreateFork(ctx, running.Volumes(), spec.ID, point)
	if err != nil {
		w.logf("%s: %s could not fork %s/%d: %v", spec.ID, h.name, spec.Parent, sequence, err)
		return nil
	}
	shape := host.ColdShape{Memory: MemoryVolume}
	if cold {
		// A shape is a cold boot's alone, whatever the checkpoint holds.
		shape.VCPUs = 1
	}
	state, err := running.CreateRoot(ctx, vm, point, shape)
	if err != nil {
		w.logf("%s: %s could not publish the root over %s/%d: %v", spec.ID, h.name, spec.Parent, sequence, err)
		w.giveUpCreate(ctx, running, spec.ID)
		return nil
	}
	resumed := !cold && !pause.stateless
	if resumed != (state != nil) {
		return fmt.Errorf("%s: created from %s/%d with %d bytes of VMM state, and that checkpoint was published stateless=%t",
			spec.ID, spec.Parent, sequence, len(state), pause.stateless)
	}
	came := durableState{sequence: vm.Status().Checkpoint.Sequence, model: pause.model, writes: pause.writes}
	if !resumed {
		came = durableState{sequence: came.sequence, model: withoutMemory(pause.model), stateless: true}
	}
	g, err := w.newGuest(h, h.pager, vm, nil, state)
	if err != nil {
		return err
	}
	h.running(g)
	read, missing, unreadable := g.readAll(ctx)
	if !agrees(read, missing, came.model) {
		return fmt.Errorf("%s created from %s/%d reads what that checkpoint did not publish: %s",
			spec.ID, spec.Parent, sequence, describeRead(read, []durableState{came}))
	}
	if got := g.stored(); got != came.writes {
		return fmt.Errorf("%s created from %s/%d continues at %d stores, and that checkpoint's pause held %d",
			spec.ID, spec.Parent, sequence, got, came.writes)
	}
	if unreadable != nil {
		w.logf("a page could not be read while a fault was on: %v", unreadable)
	}
	g.adopt(came.model)
	in := &instance{spec: spec}
	w.adopt(in)
	w.place(in, spec.Host, g)
	w.notePublished(spec.ID, came.sequence)
	w.landed(in, g, came)
	if err := running.AddMachine(spec.ID, g); err != nil {
		return err
	}
	w.logf("%s: created on %s from %s/%d, resumed=%t", spec.ID, h.name, spec.Parent, sequence, resumed)
	return nil
}

// giveUpCreate deletes a child whose root never published, handle and record,
// and leaves an identity the store would not let go of to be deleted again at
// every step.
func (w *World) giveUpCreate(ctx context.Context, running *host.Host, id string) {
	if err := running.Delete(ctx, id); err != nil {
		w.logf("%s: the identity of a create whose root never published is still not free: %v", id, err)
		if w.orphans == nil {
			w.orphans = map[string]bool{}
		}
		w.orphans[id] = true
	}
}

// withoutMemory is a pause as a cold boot over it reads: zeroes where the
// memory was, and the disks exactly as the pause left them.
func withoutMemory(model map[string][]byte) map[string][]byte {
	booted := make(map[string][]byte, len(model))
	for name, data := range model {
		if name == MemoryVolume {
			booted[name] = make([]byte, len(data))
			continue
		}
		booted[name] = bytes.Clone(data)
	}
	return booted
}

// Release gives up one kept checkpoint of the named VM through whichever host
// is up. It is refused for a checkpoint a VM was created from, and the record
// is what says whether one was: a release that was refused for any other
// reason, or that succeeded, of a checkpoint the record pins, is a release that
// took what a descendant reads.
func (w *World) Release(ctx context.Context, id string, sequence uint64) error {
	var running *host.Host
	for index := range w.hosts {
		if running = w.up(index); running != nil {
			break
		}
	}
	if running == nil {
		return nil
	}
	err := running.Volumes().Release(ctx, id, sequence)
	records, readErr := w.records()
	if readErr != nil {
		return readErr
	}
	record, readErr := records.Read(ctx, id)
	if readErr != nil {
		// The store is away, so nothing can be said about what the release did.
		w.logf("%s: the release of %d could not be read back: %v", id, sequence, errors.Join(err, readErr))
		return nil
	}
	switch {
	case record.IsPinned(sequence) && !errors.Is(err, control.ErrForked):
		return fmt.Errorf("%s: releasing %d, which a VM was created from, reported %v", id, sequence, err)
	case errors.Is(err, control.ErrForked) && !record.IsPinned(sequence):
		return fmt.Errorf("%s: releasing %d was refused as forked, and the record pins %v", id, sequence, record.Pinned)
	case err == nil && record.IsKept(sequence):
		return fmt.Errorf("%s: releasing %d reported success, and the record still keeps it", id, sequence)
	case err != nil:
		w.logf("%s: the release of %d did not happen: %v", id, sequence, err)
	default:
		w.logf("%s: released %d", id, sequence)
	}
	return nil
}

// VerifyKept reads every checkpoint every VM's control record keeps straight
// out of the store and requires it to hold exactly the pause it was kept at:
// every page of its disks, and its memory and the VMM state's store counter
// when it holds VMM state. It is the invariant keeping exists for: nothing a
// kept checkpoint reads from is reclaimed while it is kept, however many
// checkpoints its VM has published since and whatever faults have come and
// gone. A checkpoint that cannot be read at all while a fault is on is
// reported with reads, as Verify reports a page.
func (w *World) VerifyKept(ctx context.Context, reads Reads) error {
	records, err := w.records()
	if err != nil {
		return err
	}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: w.runtime.ObjectStore(),
		ObjectPrefix: w.config.Prefix})
	if err != nil {
		return err
	}
	w.mu.Lock()
	ids := slices.Sorted(maps.Keys(w.kept))
	w.mu.Unlock()
	var errs []error
	for _, id := range ids {
		record, err := records.Read(ctx, id)
		if errors.Is(err, platform.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, w.unreadableKept(reads, id, 0, err))
			continue
		}
		for _, kept := range record.Kept {
			pause, known := w.keptPause(id, kept.Sequence)
			if !known {
				continue
			}
			if kept.State == pause.stateless {
				errs = append(errs, fmt.Errorf("%s keeps %d as holding state=%t, and it was published stateless=%t",
					id, kept.Sequence, kept.State, pause.stateless))
				continue
			}
			if err := w.readKept(ctx, store, control.Ref{VM: id, Sequence: kept.Sequence}, pause); err != nil {
				if errors.Is(err, errWrongBytes) {
					errs = append(errs, err)
					continue
				}
				errs = append(errs, w.unreadableKept(reads, id, kept.Sequence, err))
			}
		}
	}
	return errors.Join(errs...)
}

// errWrongBytes is a kept checkpoint that reads, and reads bytes other than the
// pause it was kept at, which no fault excuses.
var errWrongBytes = errors.New("a kept checkpoint reads other bytes than its pause")

// unreadableKept is a kept checkpoint that could not be read: a failure while
// reads may fail and the store is what is away, and a violation otherwise —
// above all a checkpoint whose objects are gone.
func (w *World) unreadableKept(reads Reads, id string, sequence uint64, err error) error {
	if reads == ReadsMayFail && excused(err) {
		w.logf("%s: kept checkpoint %d could not be read while a fault was on: %v", id, sequence, err)
		return nil
	}
	return fmt.Errorf("%s: kept checkpoint %d cannot be read: %w", id, sequence, err)
}

// readKept reads one kept checkpoint whole and compares it with its pause.
func (w *World) readKept(ctx context.Context, store *checkpoint.Store, ref control.Ref, pause durableState) error {
	index, err := store.Open(ctx, ref)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(pause.model)) {
		if name == MemoryVolume && pause.stateless {
			// A checkpoint of the disks alone goes on naming the memory an
			// earlier checkpoint published, which no registers describe and
			// nothing boots over.
			continue
		}
		want := pause.model[name]
		got := make([]byte, len(want))
		if err := store.Read(ctx, index, name, 0, got); err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("%w: %s %s reads %s, kept at %s", errWrongBytes, ref, name,
				heads(map[string][]byte{name: got}), heads(map[string][]byte{name: want}))
		}
	}
	if pause.stateless {
		return nil
	}
	state, err := store.ReadState(ctx, index)
	if err != nil {
		return err
	}
	if len(state) != stateBytes || int64(binary.LittleEndian.Uint64(state[1:])) != pause.writes {
		return fmt.Errorf("%w: %s holds VMM state %x, kept at %d stores", errWrongBytes, ref, state, pause.writes)
	}
	return nil
}
