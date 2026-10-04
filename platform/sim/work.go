package sim

import (
	"context"
	"slices"
	"time"
)

// Work spends the simulated time that n bytes of one kind of work cost under
// the runtime ctx carries, as Config.Compute prices it: nothing outside a
// simulation, and nothing for a kind the runtime does not price. It is how a
// test sees work that costs a processor in a deployment take time, so that
// work done side by side and work done one piece at a time finish at
// different instants. The runtime counts the pieces of each kind and the most
// that were under way at once (Runtime.Work).
//
// Nothing is traced: the time a piece takes is fixed by its size, and a run
// that prices no work runs exactly as it did before a site called Work.
func Work(ctx context.Context, kind string, n int) error {
	r := RuntimeFrom(ctx)
	if r == nil {
		return nil
	}
	rate := r.compute[kind]
	if rate <= 0 {
		return nil
	}
	r.beginWork(kind, taskName(ctx), n)
	defer r.endWork(kind)
	return r.sleep(ctx, workTime(n, rate))
}

// workTime is what n bytes cost at rate bytes a second, rounded down to a
// nanosecond.
func workTime(n int, rate int64) time.Duration {
	return time.Duration(int64(max(n, 0)) * int64(time.Second) / rate)
}

// WorkStats is what one kind of priced work did in a run: how many pieces of
// it began, and the most that were under way at one instant.
type WorkStats struct {
	Pieces uint64
	Peak   int
}

// Work reports what the pieces of one kind of priced work did.
func (r *Runtime) Work(kind string) WorkStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if count := r.work[kind]; count != nil {
		return count.WorkStats
	}
	return WorkStats{}
}

// WorkPiece is one piece of priced work: the task (WithTask) it ran under,
// empty where its caller named none, the simulated instant it began, and the
// bytes it was priced by.
type WorkPiece struct {
	Task  string
	Began time.Time
	Bytes int
}

// WorkPieces reports each piece of one kind of priced work, in the order the
// pieces began. It is how a test sees which of several callers waiting for
// one processor got it first. Pieces that began at one instant are in the
// order their goroutines happened to run, which the instants do not depend on.
func (r *Runtime) WorkPieces(kind string) []WorkPiece {
	r.mu.Lock()
	defer r.mu.Unlock()
	if count := r.work[kind]; count != nil {
		return slices.Clone(count.pieces)
	}
	return nil
}

// workCount is one kind's stats, how many of its pieces are under way, and
// its pieces, in the order they began.
type workCount struct {
	WorkStats
	running int
	pieces  []WorkPiece
}

func (r *Runtime) beginWork(kind, task string, bytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.work == nil {
		r.work = make(map[string]*workCount)
	}
	count := r.work[kind]
	if count == nil {
		count = &workCount{}
		r.work[kind] = count
	}
	count.Pieces++
	count.pieces = append(count.pieces, WorkPiece{Task: task, Began: r.now(), Bytes: bytes})
	count.running++
	count.Peak = max(count.Peak, count.running)
}

func (r *Runtime) endWork(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.work[kind].running--
}
