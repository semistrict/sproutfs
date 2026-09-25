package sim

import (
	"context"
	"fmt"
	"sync"
	"testing/synctest"
	"time"

	simv1 "github.com/semistrict/sproutfs/platform/internal/gen/sproutfs/sim/v1"
)

type dynamicWait struct {
	id               string
	ctx              context.Context
	earliest, latest time.Time
	gate             chan struct{}
}

// Scheduler discovers each next frontier with synctest.Wait: every other
// goroutine has reached a durable wait. No upfront operation count is needed.
// Release one completion, let its consequences run to quiescence, then include
// newly discovered work before choosing again. This governs adapter boundaries,
// not arbitrary shared-memory accesses between them.
type Scheduler struct {
	mu         sync.Mutex
	pending    map[string]*dynamicWait
	seen       map[string]bool
	changed    chan struct{}
	seed       uint64
	trace      *scheduleTrace
	releases   int
	maxPending int
}

// NewScheduler constructs a controller inside the synctest bubble it will run.
// Seed zero selects the same default seed as Runtime. A controller serves one
// workload; operation IDs must not be reused during that workload.
func NewScheduler(seed uint64) *Scheduler {
	if seed == 0 {
		seed = 1
	}
	return &Scheduler{
		pending: make(map[string]*dynamicWait), seen: make(map[string]bool),
		changed: make(chan struct{}, 1),
		seed:    seed, trace: &scheduleTrace{seed: seed, start: time.Now()},
	}
}

// Wait parks an operation until Run releases it within the given relative
// timing window, or observes its cancellation. Even a zero-width window parks
// until a scheduling decision. Supply this method as Config.Wait and use stable
// task IDs for workload admission. Invalid windows and reused IDs panic.
func (s *Scheduler) Wait(ctx context.Context, id string, minimum, maximum time.Duration) error {
	if id == "" || minimum < 0 || maximum < minimum {
		panic("invalid dynamic wait")
	}
	now := time.Now()
	wait := &dynamicWait{id: id, ctx: ctx, earliest: now.Add(minimum), latest: now.Add(maximum), gate: make(chan struct{})}
	s.mu.Lock()
	if s.seen[id] {
		s.mu.Unlock()
		panic("reused dynamic operation ID: " + id)
	}
	s.seen[id] = true
	s.pending[id] = wait
	s.maxPending = max(s.maxPending, len(s.pending))
	s.trace.window(id, wait.earliest.Sub(s.trace.start), wait.latest.Sub(s.trace.start))
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
	stop := context.AfterFunc(ctx, func() {
		select {
		case s.changed <- struct{}{}:
		default:
		}
	})
	defer stop()
	// Cancellation is also released by the controller at a quiescent frontier.
	// Returning directly from ctx.Done would reintroduce uncontrolled execution.
	<-wait.gate
	return context.Cause(ctx)
}

func (s *Scheduler) before(a, b *dynamicWait) bool {
	pa, pb := keyedSample(s.seed, "dynamic-overlap\x00"+a.id), keyedSample(s.seed, "dynamic-overlap\x00"+b.id)
	return pa < pb || pa == pb && a.id < b.id
}

// Run drives waits from one goroutine of a synctest bubble. Run the
// workload in another goroutine, shut down its background actors there, and
// close done after cleanup. Run also drains remaining waits before returning.
func (s *Scheduler) Run(done <-chan struct{}) error {
	for {
		synctest.Wait()
		select {
		case <-s.changed:
		default:
		}
		now := time.Now()
		s.mu.Lock()
		var canceled *dynamicWait
		var at time.Time
		for _, wait := range s.pending {
			if wait.ctx.Err() != nil {
				if canceled == nil || s.before(wait, canceled) {
					canceled = wait
				}
			}
			if at.IsZero() || wait.latest.Before(at) {
				at = wait.latest
			}
		}
		var next *dynamicWait
		if canceled != nil {
			next, at = canceled, now
		} else if !at.IsZero() {
			if at.Before(now) {
				s.mu.Unlock()
				return fmt.Errorf("missed dynamic completion window: %s < %s", at, now)
			}
			for _, wait := range s.pending {
				if !wait.earliest.After(at) && (next == nil || s.before(wait, next)) {
					next = wait
				}
			}
		}
		s.mu.Unlock()
		if next == nil {
			select {
			case <-done:
				return nil
			default:
			}
			// Uncontrolled timers (for example context deadlines) may introduce
			// work. A new wait wakes this controller before it advances further.
			select {
			case <-s.changed:
			case <-done:
				return nil
			}
			continue
		}
		if at.After(now) {
			timer := time.NewTimer(at.Sub(now))
			select {
			case <-timer.C:
			case <-s.changed:
				timer.Stop()
			}
			continue // Recompute after any timer or newly arriving operation.
		}
		s.mu.Lock()
		delete(s.pending, next.id)
		s.mu.Unlock()
		kind := simv1.EventKind_EVENT_KIND_RELEASED
		if next.ctx.Err() != nil {
			kind = simv1.EventKind_EVENT_KIND_CANCELED
		}
		s.trace.record(kind, next.id, scheduleOutcome(context.Cause(next.ctx)), nil)
		close(next.gate)
		synctest.Wait()
		s.trace.record(simv1.EventKind_EVENT_KIND_COMPLETED, next.id, "quiescent", nil)
		s.releases++
		if s.releases > 100000 {
			return fmt.Errorf("dynamic scheduler failed to converge")
		}
	}
}

// SchedulerStats reports work discovered and released by a completed run.
type SchedulerStats struct{ Releases, MaxPending int }

// Stats must be called after Run returns.
func (s *Scheduler) Stats() SchedulerStats { return SchedulerStats{s.releases, s.maxPending} }

// Record captures a semantic observation such as acknowledged or recovered data.
// It copies payload; callers may reuse their buffers immediately.
func (s *Scheduler) Record(id, outcome string, payload []byte) {
	s.trace.record(simv1.EventKind_EVENT_KIND_CHECKED, id, outcome, payload)
}
