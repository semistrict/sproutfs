package vmmemory

import (
	"context"
	"errors"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// The mapping commands the pager sends, and the record of what they mapped
// that the bindings keep.

// The pager reaches its mapping only through these, so every command a fault
// issues is timed in one place. They add a monotonic reading and one atomic
// increment each and change nothing else: the command, its arguments, its
// error and its ordering are exactly the interface's.
func (r *MemoryRegion) mapPages(ctx context.Context, run MapRun, writable bool) error {
	start := r.host.clock.Now()
	err := r.mapping.Map(ctx, run.Page, run.File, run.Slot, run.Count, writable)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	if r.audit != nil {
		if writable {
			r.audited(err, run.Page, run.Count, installedWritable, "map writable")
		} else {
			r.audited(err, run.Page, run.Count, installedReadOnly, "map read-only, "+r.describeBinding(run.Page))
		}
	}
	return err
}

func (r *MemoryRegion) mapZeroPages(ctx context.Context, page uint64, count int) error {
	start := r.host.clock.Now()
	err := r.mapping.MapZero(ctx, page, count)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	r.audited(err, page, count, installedReadOnly, "map zero")
	return err
}

func (r *MemoryRegion) mapBatch(ctx context.Context, batch BatchMapping, runs []MapRun) (int, int, error) {
	start := r.host.clock.Now()
	commands, mappingRuns, err := batch.MapBatch(ctx, runs)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	for _, run := range runs {
		r.audited(err, run.Page, run.Count, installedReadOnly, "map batch")
	}
	return commands, mappingRuns, err
}

// audited records what a command left the pages [first, first+count) as:
// state where it landed; nothing changed where it was refused, which the
// pages' histories still name; and unknown after any other failure.
func (r *MemoryRegion) audited(err error, first uint64, count int, state installed, what string) {
	if r.audit == nil {
		return
	}
	switch {
	case err == nil:
		r.audit.installed(first, count, state, what)
	case errors.Is(err, ErrMappingRefused):
		r.audit.refused(first, count, what)
	default:
		r.audit.fail()
	}
}

// mappingFailed reports what a failed mapping command means for this memory region. A
// refusal is the one failure that is known to have changed nothing: the client
// admits a command against its mapping budget before it touches anything, so
// the pages are not mapped, the record the pager made of them is taken back by
// undo, and the fault fails rather than the memory region. Every other failure may
// have been applied, so that record stands — memory the guest can still read
// through must never be reachable from a binding that says it is unmapped,
// which a revocation would skip — and the memory region is terminal.
func (r *MemoryRegion) mappingFailed(err error, undo func()) error {
	if !errors.Is(err, ErrMappingRefused) {
		return r.fail(err)
	}
	undo()
	return err
}

func (r *MemoryRegion) revokePage(ctx context.Context, page uint64) error {
	start := r.host.clock.Now()
	err := r.mapping.Revoke(ctx, page)
	r.host.revokeLatency.Observe(r.host.clock.Since(start))
	r.audited(err, page, 1, notInstalled, "revoke")
	return err
}

func (r *MemoryRegion) revokeBatch(ctx context.Context, batch BatchRevocation, runs []PageRun) (int, int, error) {
	start := r.host.clock.Now()
	commands, revokedRuns, err := batch.RevokeBatch(ctx, runs)
	r.host.revokeLatency.Observe(r.host.clock.Since(start))
	for _, run := range runs {
		r.audited(err, run.Page, run.Count, notInstalled, "revoke batch")
	}
	return commands, revokedRuns, err
}

// setMapped records whether the pages of [first, last) are mapped. A page
// recorded mapped is retained across an ambiguous answer.
func (r *MemoryRegion) setMapped(first, last uint64, mapped bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	for page := first; page < last; page++ {
		b := r.bindingLocked(page)
		b.mapped = mapped
		r.noteSealableLocked(b)
	}
	r.recordedMapped(first, last, mapped, "set")
}

// mapZeros records that a plan mapped every page of [start, end) to zero, as
// MemoryRegion.mapZeros does.
func (r *MemoryRegion) mapZeros(start, end uint64) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	ps := r.host.pageSize
	var gaps [][2]uint64
	if err := r.beside.ForEveryPageAndGapInRange(func(slot *zirconvm.PageOrMarker[binding], _ uint64) error {
		if slot.IsPage() {
			slot.Page().inZeroRun = true
		}
		return nil
	}, func(start, end uint64) error {
		gaps = append(gaps, [2]uint64{start, end})
		return nil
	}, start*ps, end*ps); err != nil {
		panic("vmmemory: walking the bindings beside a layer: " + err.Error())
	}
	for _, gap := range gaps {
		if err := r.beside.AddZeroInterval(gap[0], gap[1], zirconvm.IntervalUntracked); err != nil {
			panic("vmmemory: adding a zero run: " + err.Error())
		}
	}
}

// unmapRuns takes back the record that the runs of one refused command are
// mapped, as MemoryRegion.unmapRuns does.
func (r *MemoryRegion) unmapRuns(runs []MapRun) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	ps := r.host.pageSize
	for _, run := range runs {
		first, last := run.Page, run.Page+uint64(run.Count)
		if run.Zero {
			r.eachBoundLocked(first, last, func(b *binding) { b.inZeroRun = false })
			start, end := first*ps, last*ps
			if r.beside.IsOffsetInZeroInterval(start) {
				r.beside.LookupOrAllocate(start, zirconvm.SplitInterval)
			}
			if lastPage := end - ps; lastPage > start && r.beside.IsOffsetInZeroInterval(lastPage) {
				r.beside.LookupOrAllocate(lastPage, zirconvm.SplitInterval)
			}
			if err := r.beside.RemovePages(func(slot *zirconvm.PageOrMarker[binding], _ uint64) error {
				if slot.IsInterval() {
					slot.Take()
				}
				return nil
			}, start, end); err != nil {
				panic("vmmemory: walking the bindings beside a layer: " + err.Error())
			}
			continue
		}
		r.eachBoundLocked(first, last, func(b *binding) {
			b.mapped = false
			r.noteSealableLocked(b)
		})
	}
}

// setBindingMapped records whether b's mapping is installed.
func (r *MemoryRegion) setBindingMapped(b *binding, mapped bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b.mapped = mapped
	r.noteSealableLocked(b)
	r.recordedMapped(b.index, b.index+1, mapped, "set one")
}
