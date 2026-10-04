package sim

import (
	"context"
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
	r.beginWork(kind)
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

// workCount is one kind's stats and how many of its pieces are under way.
type workCount struct {
	WorkStats
	running int
}

func (r *Runtime) beginWork(kind string) {
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
	count.running++
	count.Peak = max(count.Peak, count.running)
}

func (r *Runtime) endWork(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.work[kind].running--
}
