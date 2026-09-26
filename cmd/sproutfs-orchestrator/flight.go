package main

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// flight is the rows of an operation under way, which it writes again for as
// long as the operation runs. A row in flight is taken at its word only for the
// table's aging bound, because a row whose operation died with its process
// would otherwise hold a handover open for good. An operation that is still
// running is not one that died, however long it takes, and writing its rows
// again well inside that bound is what shows it.
//
// A fan-out of forks is the operation that needs it: its children are received
// one after another, so the last of them can begin long after its row was
// written. A row that aged out while its child waited would have the child's
// hold given up by the next reconcile, and the fork would fail for nothing.
type flight struct {
	o    *orchestrator
	mu   sync.Mutex
	rows map[string]vmRecord
	done chan struct{}
	wg   sync.WaitGroup
}

// fly writes rows and keeps them in flight until each one lands or the flight
// ends.
func (o *orchestrator) fly(ctx context.Context, rows []vmRecord) *flight {
	f := &flight{o: o, rows: map[string]vmRecord{}, done: make(chan struct{})}
	for _, row := range rows {
		f.rows[row.ID] = row
		o.note(ctx, row)
	}
	if o.table == nil {
		return f
	}
	// A quarter of the bound leaves a row three chances to be written again
	// before anything could take it for an operation that died.
	every := o.table.believed() / 4
	f.wg.Go(func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-f.done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			f.mu.Lock()
			// The table stamps each row with the time of this write.
			for _, id := range slices.Sorted(maps.Keys(f.rows)) {
				o.note(ctx, f.rows[id])
			}
			f.mu.Unlock()
		}
	})
	return f
}

// land ends one row's flight: its VM runs on the host the row names, and the
// row says so from here.
func (f *flight) land(ctx context.Context, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, found := f.rows[id]
	if !found {
		return
	}
	delete(f.rows, id)
	row.State = stateRunning
	f.o.note(ctx, row)
}

// end ends every row's flight, and returns once nothing more of it will be
// written: what the operation writes next is its own.
func (f *flight) end() {
	close(f.done)
	f.wg.Wait()
}
