package vmmemory

import (
	"context"
	"errors"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// What the bindings say is mapped is the pager's record of the page tables of
// a process it cannot read: the client's. It is made here and in
// revocation.go, and nowhere else (TestOnlyTheMapperRecordsWhatIsMapped):
// every mapping command is sent here, and what the bindings say changes as a
// consequence of what the command did.
//
//   - A run is recorded mapped once its command has landed, by the code that
//     sent it, while the caller holds the run's pages or its fault stripe, so
//     no eviction, fault or seal reads the record in between.
//   - A refused command changed nothing: the client admits a command against
//     its mapping budget before it touches anything. Nothing is recorded, and
//     the fault that sent it fails rather than the region.
//   - Any other failure may have been applied, so its runs are recorded
//     mapped, the safe way round (a revocation skips a page recorded
//     unmapped, and would let a page the guest still reads through go), and
//     the region is terminal.
//   - A revocation records its pages unmapped once its command has landed
//     (revocation.go).
//
// Until 2026-10-08 each path recorded its runs before sending them and undid
// the record on a refusal. Three paths undid the wrong runs — one undid too
// few and killed an embedder's VM with UFFDIO_CONTINUE: invalid argument —
// and one never recorded at all. Sending and recording in one place is what
// leaves no undo to get wrong.

// mapRun maps one run — writable, from the region's private file; read-only,
// from any file it was given; or to zeros — and records it mapped once its
// command has landed, as the owner above says. Caller holds the run's pages,
// or the fault stripe of a run of zeros.
func (r *MemoryRegion) mapRun(ctx context.Context, run MapRun, writable bool) error {
	var err error
	if run.Zero {
		err = r.mapZeroPages(ctx, run.Page, run.Count)
	} else {
		err = r.mapPages(ctx, run, writable)
	}
	return r.landed(ctx, err, run)
}

// mapReadOnly maps runs read-only or to zeros, by batches where the client
// takes them and one command a run otherwise, and records each run mapped as
// its command lands. A batch is refused whole or not at all: a client that
// refuses one after part of it landed fails the session (commit in
// connection_linux.go), which is not a refusal. It reports the commands and
// runs it sent.
func (r *MemoryRegion) mapReadOnly(ctx context.Context, runs []MapRun) (commands, mappingRuns int, err error) {
	if batch, ok := r.mapping.(BatchMapping); ok {
		commands, mappingRuns, err = r.mapBatch(ctx, batch, runs)
		return commands, mappingRuns, r.landed(ctx, err, runs...)
	}
	for _, run := range runs {
		if err := r.mapRun(ctx, run, false); err != nil {
			return commands, mappingRuns, err
		}
		commands++
		mappingRuns++
	}
	return commands, mappingRuns, nil
}

// landed records runs as their command's err says: mapped where it landed or
// failed ambiguously, which also ends the region, and nothing where it was
// refused.
func (r *MemoryRegion) landed(ctx context.Context, err error, runs ...MapRun) error {
	refused := errors.Is(err, ErrMappingRefused)
	if !refused || sim.Bug(ctx, "pager-record-a-refused-run-mapped") {
		for _, run := range runs {
			r.recordRun(run)
		}
	}
	switch {
	case err == nil:
		return nil
	case refused:
		return err
	}
	return r.fail(err)
}

// recordRun records run mapped: a run of zeros as a zero run of the bindings
// beside the layer, and any other on each page's binding. The audit checks
// the record against what the commands installed.
func (r *MemoryRegion) recordRun(run MapRun) {
	first, last := run.Page, run.Page+uint64(run.Count)
	if run.Zero {
		r.mapZeros(first, last)
	} else {
		r.setMapped(first, last, true)
	}
	r.agreeRecorded(first, last)
}

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

// setMapped records whether the pages of [first, last) are mapped. The pages
// it makes sealable or not sealable are changed in the runs a seal reads a
// span at a time, so that a run of thousands of fresh pages a boot faults is
// one change to them rather than one a page.
func (r *MemoryRegion) setMapped(first, last uint64, mapped bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	span, sealable := first, false
	flush := func(end uint64) {
		if end == span {
			return
		}
		if sealable {
			r.dirtyRuns.add(span, end)
		} else {
			for page := span; page < end; page++ {
				r.dirtyRuns.remove(page)
			}
		}
	}
	for page := first; page < last; page++ {
		b := r.bindingLocked(page)
		b.mapped = mapped
		now := b.writable() && b.mapped
		if page > first && now != sealable {
			flush(page)
			span = page
		}
		sealable = now
	}
	flush(last)
	r.recordedMapped(first, last, mapped, "set")
}

// setBindingMapped records whether b's mapping is installed.
func (r *MemoryRegion) setBindingMapped(b *binding, mapped bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b.mapped = mapped
	r.noteSealableLocked(b)
	r.recordedMapped(b.index, b.index+1, mapped, "set one")
}

// mapZeros records that every page of [start, end) is mapped to zero: a page
// with a binding is marked in the run, and the rest are one interval of the
// page list beside the layer, which costs nothing per page.
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
