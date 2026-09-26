package checkpoint

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Reclaim deletes the checkpoints the current one no longer names: every
// checkpoint the previous root named, and the previous checkpoint itself, that
// the current root does not name and no pin protects. A dead checkpoint goes
// whole, its index object first, because a root names every checkpoint it reads
// a page from and every one it addresses a segment in: a checkpoint nothing
// names holds nothing anyone can reach.
//
// Both indexes must belong to the same VM, and previous must not be the
// checkpoint that is now selected. Only this VM's own checkpoints are deleted:
// a fork's index names its parent's checkpoints, and those belong to the parent.
//
// pinned names the sequences of this VM that have been forked. Each one is
// spared, along with every checkpoint its index names — which is what makes a
// grandchild safe, because its own index names those checkpoints directly and
// no record here says so. Each expansion is read once and memoised, because
// indexes are immutable. Deletion is idempotent: an object already gone is not
// an error. Failures are joined and returned rather than stopping the sweep,
// because every object reclamation misses is merely unreferenced.
func (s *Store) Reclaim(ctx context.Context, previous, current *Index, pinned []uint64) error {
	if previous == nil || current == nil || previous.ref.VM != current.ref.VM ||
		previous.ref == current.ref || previous.ref.IsZero() {
		return ErrInvalidConfig
	}
	dead := make(map[control.Ref]bool, len(previous.checkpoints)+1)
	dead[previous.ref] = true
	for _, ref := range previous.named() {
		dead[ref] = true
	}
	// Every checkpoint the current index names is spared, the ones it no longer
	// reads but still names included: those are what its compaction emptied, and
	// a reader holding the view it replaced goes on reading them until the next
	// checkpoint drops them.
	if !sim.Bug(ctx, "checkpoint-reclaim-live-checkpoint") {
		for _, ref := range current.named() {
			delete(dead, ref)
		}
	}
	if err := s.sparePinned(ctx, previous.ref.VM, pinned, dead); err != nil {
		return err
	}
	var errs []error
	for _, ref := range sortedRefs(dead) {
		if ref.VM != previous.ref.VM {
			continue
		}
		errs = append(errs, s.deleteCheckpoint(ctx, ref))
	}
	return errors.Join(errs...)
}

// sparePinned takes every checkpoint a pin protects out of a sweep's dead set:
// the pinned checkpoint itself and every checkpoint its index names. A pin
// whose index cannot be read protects everything, because nothing a sweep could
// free is worth what a fork still reads.
func (s *Store) sparePinned(ctx context.Context, vm string, pinned []uint64, dead map[control.Ref]bool) error {
	for _, sequence := range pinned {
		protected, err := s.protectedBy(ctx, control.Ref{VM: vm, Sequence: sequence})
		if err != nil {
			return errors.Join(err, errors.New("checkpoint: pinned checkpoint unreadable"))
		}
		for _, spared := range protected {
			delete(dead, spared)
		}
	}
	return nil
}

// protectedBy reports the checkpoints a pinned one keeps alive: itself and
// every checkpoint its index names. Indexes are immutable and pins are
// permanent, so the answer is memoised for the life of this store and never
// goes stale.
//
// Named rather than read, as for the index a sweep is publishing: a checkpoint
// a pinned index no longer reads is one its own compaction emptied, and the pin
// says nothing under that checkpoint may be taken until a collector that can
// see every fork says so. A sweep that spared only what the pinned index reads
// leaves it naming a checkpoint nothing can fetch, which is a hole in what a
// fork inherits however few pages it holds.
func (s *Store) protectedBy(ctx context.Context, ref control.Ref) ([]control.Ref, error) {
	s.protectedMu.Lock()
	cached, found := s.protected[ref]
	s.protectedMu.Unlock()
	if found {
		return cached, nil
	}
	index, err := s.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	protected := append(index.named(), ref)
	slices.SortFunc(protected, compareRefs)
	protected = slices.CompactFunc(protected, func(a, b control.Ref) bool { return a == b })
	s.protectedMu.Lock()
	s.protected[ref] = protected
	s.protectedMu.Unlock()
	return protected, nil
}

// deleteCheckpoint removes everything one checkpoint published, its index
// object first: that object is what makes the checkpoint openable, so the
// checkpoint stops being readable before any of what it names goes, and an
// interrupted sweep leaves parts a repeat finds again under the same prefix.
func (s *Store) deleteCheckpoint(ctx context.Context, ref control.Ref) error {
	prefix, err := s.ObjectPrefix(ref)
	if err != nil {
		return err
	}
	index, err := s.indexKey(ref)
	if err != nil {
		return err
	}
	if err := s.deleteObject(ctx, index); err != nil {
		return err
	}
	var errs []error
	err = platform.ListAll(ctx, s.objects, prefix, func(object platform.ObjectMetadata) error {
		errs = append(errs, s.deleteObject(ctx, object.Key))
		return nil
	})
	return errors.Join(append(errs, err)...)
}

// DeleteVM removes the checkpoint objects one VM published that no pin of it
// protects, the index object of each checkpoint first, for the same reason one
// checkpoint's index object goes first: while that object is there the
// checkpoint is still openable, so nothing it names goes out from under a
// reader of it.
//
// It is what frees an identity. A checkpoint object is named by its VM and a
// sequence and is written create-if-absent, so objects left under an identity
// are objects a VM created under it later could collide with — which is why
// that caller refuses an identity anything is stored under. Only the caller
// that has removed the control record may do this: while the record is there,
// that VM's state is these objects. Deletion is idempotent, an object already
// gone is not an error, and a VM that published nothing deletes nothing.
//
// pinned is what that VM's record pinned: the checkpoints of it a fork was
// taken at. Each is left whole, with every checkpoint its index names, because
// forks this deployment cannot enumerate from here read through them —
// which does mean a deleted VM that was ever forked leaves objects behind, and
// the identity is free while they stand. They are a collector's to reclaim.
func (s *Store) DeleteVM(ctx context.Context, vm string, pinned []uint64) error {
	if !control.ValidID(vm) {
		return ErrInvalidConfig
	}
	prefix, err := platform.NewObjectPrefix(s.vmPrefix(vm) + "ckpt/")
	if err != nil {
		return err
	}
	spared, err := s.sparedCheckpoints(ctx, vm, pinned)
	if err != nil {
		return err
	}
	var errs []error
	var rest []platform.ObjectKey
	err = platform.ListAll(ctx, s.objects, prefix, func(object platform.ObjectMetadata) error {
		sequence, ok := sequenceOf(prefix, object.Key)
		if !ok || spared[sequence] {
			// Not one of this VM's checkpoint objects, or one a pin keeps.
			return nil
		}
		if !strings.HasSuffix(object.Key.String(), "/index") {
			rest = append(rest, object.Key)
			return nil
		}
		errs = append(errs, s.deleteObject(ctx, object.Key))
		return nil
	})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, key := range rest {
		errs = append(errs, s.deleteObject(ctx, key))
	}
	return errors.Join(errs...)
}

// sparedCheckpoints reports the sequences of one VM a sweep of the whole VM
// must leave: every pinned checkpoint and every checkpoint of this VM a pinned
// index names.
func (s *Store) sparedCheckpoints(ctx context.Context, vm string, pinned []uint64) (map[uint64]bool, error) {
	spared := make(map[uint64]bool, len(pinned))
	for _, sequence := range pinned {
		protected, err := s.protectedBy(ctx, control.Ref{VM: vm, Sequence: sequence})
		if err != nil {
			return nil, errors.Join(err, errors.New("checkpoint: pinned checkpoint unreadable"))
		}
		for _, ref := range protected {
			if ref.VM == vm {
				spared[ref.Sequence] = true
			}
		}
	}
	return spared, nil
}

// sequenceOf reports the checkpoint sequence one object key belongs to, and
// whether the key is one of that VM's checkpoint objects at all: everything
// under the prefix is <sequence>/<something>.
func sequenceOf(prefix platform.ObjectPrefix, key platform.ObjectKey) (uint64, bool) {
	rest, found := strings.CutPrefix(key.String(), prefix.String())
	if !found {
		return 0, false
	}
	name, _, found := strings.Cut(rest, "/")
	if !found {
		return 0, false
	}
	sequence, err := strconv.ParseUint(name, 10, 64)
	if err != nil {
		return 0, false
	}
	return sequence, true
}

// deleteObject removes one object, treating an absent one as already reclaimed.
// It draws on reclamation's own budget rather than the upload one: a sweep must
// never hold the slots a checkpoint needs to become durable.
func (s *Store) deleteObject(ctx context.Context, key platform.ObjectKey) error {
	if err := s.acquireDelete(ctx); err != nil {
		return err
	}
	defer s.releaseDelete()
	err := s.objects.Delete(ctx, platform.DeleteRequest{Key: key})
	if errors.Is(err, platform.ErrNotFound) {
		return nil
	}
	return err
}

// Used reports whether one VM's checkpoint namespace holds any object at all.
// It is what a create asks before it publishes anything: an identity with
// objects under it and no control record is one a VM published under before —
// the checkpoints a deleted VM left pinned, or a create that was interrupted — and
// a new VM there would write into keys that are not its own.
//
// It costs one listing of at most a page, because the answer is whether there
// is anything rather than what there is.
func (s *Store) Used(ctx context.Context, vm string) (bool, error) {
	if !control.ValidID(vm) {
		return false, ErrInvalidConfig
	}
	prefix, err := platform.NewObjectPrefix(s.vmPrefix(vm))
	if err != nil {
		return false, err
	}
	page, err := s.objects.List(ctx, platform.ListRequest{Prefix: prefix})
	if err != nil {
		return false, err
	}
	return len(page.Objects) > 0, nil
}

// ObjectPrefix reports the key prefix every object of one checkpoint shares,
// which is what an operator or a collector lists a checkpoint under.
func (s *Store) ObjectPrefix(ref control.Ref) (platform.ObjectPrefix, error) {
	if !control.ValidID(ref.VM) {
		return platform.ObjectPrefix{}, ErrInvalidConfig
	}
	return platform.NewObjectPrefix(strings.Clone(s.checkpointPrefix(ref)))
}
