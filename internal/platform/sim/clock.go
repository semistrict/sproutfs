package sim

import (
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
)

// Epoch is where every simulated clock starts. It is a fixed instant rather
// than the wall clock's so that a recorded run's timestamps are the same in
// every process that replays it.
var Epoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// Clock is a virtual clock the simulation moves itself. Nothing here elapses:
// time passes when a test decides it has, and every deadline it passes is
// released in one deterministic order. That is what makes a deadline written in
// checkpoint intervals — a fork hold, a migrated hold — reachable by a decision
// rather than by waiting four minutes for it.
//
// It is the Clock port, so the production code under test is unchanged: a host
// given one arms the same timers it arms in a deployment. It is usable inside a
// testing/synctest bubble, where every wait it hands out is a channel receive
// and therefore a durable block, and outside one, where Settle waits for the
// callbacks an advance started.
//
// Two waits that come due within the same advance are released in seeded order
// rather than in the order they were armed, so a run that happens to arm two
// holds in the opposite order still releases them the same way. The draw is
// keyed by this clock's id and the arming sequence, so adding a timer in one
// module cannot perturb another module's order.
//
// Clock is safe for concurrent use. A callback passed to AfterFunc must not
// wait on this clock: nothing would advance it.
type Clock struct {
	random Random
	id     string

	mu       sync.Mutex
	now      time.Time
	sequence uint64
	pending  waitQueue
	index    map[uint64]*clockWait
	// started counts AfterFunc callbacks an advance has launched, which Settle
	// waits for.
	started sync.WaitGroup
}

// NewClock returns a virtual clock starting at Epoch. The id names it in the
// seeded order two simultaneous deadlines are released in; two clocks sharing
// an id share that order, which only matters if they also share a deadline.
func (r *Runtime) NewClock(id string) *Clock {
	if id == "" {
		panic("sim: clock id must not be empty")
	}
	return &Clock{random: r.Random("clock"), id: id, now: Epoch, index: map[uint64]*clockWait{}}
}

// NewEntropy returns deterministic stand-in entropy: the nonce a control record
// carries, the epoch a creating handle draws and the jitter a checkpoint
// interval is spread by, drawn from this runtime's seed instead of the
// operating system's pool. The id separates one consumer's stream from
// another's, and each stream is a counted sequence, so the values one writer
// chooses are reproducible and still distinct.
//
// The two kinds of draw are counted apart. A value a caller asks for by number
// decides things a trace records — the epoch a creation takes is in the key of
// every object that VM publishes — while filled bytes are opaque; counting them
// together would make a value depend on how many byte draws happened to come
// first, which is a goroutine order rather than a decision.
func (r *Runtime) NewEntropy(id string) platform.Entropy {
	if id == "" {
		panic("sim: entropy id must not be empty")
	}
	return &entropy{random: r.Random("entropy"), id: id}
}

type entropy struct {
	random Random
	id     string
	mu     sync.Mutex
	// values counts the draws Uint64 answered and bytes the words Fill
	// consumed, each its own sequence.
	values uint64
	bytes  uint64
}

// next draws the count-th value of one of this stream's two sequences.
func (e *entropy) next(kind string, count *uint64) uint64 {
	e.mu.Lock()
	*count++
	drawn := *count
	e.mu.Unlock()
	return e.random.Uint64(fmt.Sprintf("%s/%s/%d", e.id, kind, drawn))
}

func (e *entropy) Uint64() uint64 { return e.next("value", &e.values) }

func (e *entropy) Fill(b []byte) {
	for len(b) > 0 {
		var word [8]byte
		binary.LittleEndian.PutUint64(word[:], e.next("bytes", &e.bytes))
		b = b[copy(b, word[:]):]
	}
}

// clockWait is one armed deadline: a sleep, a timer tick, a ticker's next tick
// or an AfterFunc call.
type clockWait struct {
	id       uint64
	order    uint64
	deadline time.Time
	// period rearms a ticker; zero is a one-shot.
	period time.Duration
	// deliver releases a channel wait, and call runs an AfterFunc callback.
	// Exactly one of them is set.
	deliver chan time.Time
	call    func()
	// heapIndex is maintained by the queue.
	heapIndex int
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return context.Cause(ctx)
	}
	wait := c.arm(d, 0, make(chan time.Time, 1), nil)
	defer c.disarm(wait.id)
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-wait.deliver:
		return nil
	}
}

func (c *Clock) AfterFunc(d time.Duration, f func()) platform.Stopper {
	if f == nil {
		panic("sim: AfterFunc needs a function")
	}
	return &simStopper{clock: c, id: c.arm(d, 0, nil, f).id}
}

func (c *Clock) NewTimer(d time.Duration) platform.Timer {
	wait := c.arm(d, 0, make(chan time.Time, 1), nil)
	return &simTimer{clock: c, id: wait.id, ticks: wait.deliver}
}

func (c *Clock) NewTicker(d time.Duration) platform.Ticker {
	if d <= 0 {
		panic("sim: a ticker needs a positive period")
	}
	wait := c.arm(d, d, make(chan time.Time, 1), nil)
	return &simTicker{clock: c, id: wait.id, ticks: wait.deliver}
}

// arm registers one deadline d from now and returns it.
func (c *Clock) arm(d, period time.Duration, deliver chan time.Time, call func()) *clockWait {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armLocked(d, period, deliver, call)
}

func (c *Clock) armLocked(d, period time.Duration, deliver chan time.Time, call func()) *clockWait {
	c.sequence++
	wait := &clockWait{id: c.sequence, deadline: c.now.Add(max(0, d)), period: period,
		deliver: deliver, call: call,
		order: c.random.Uint64(fmt.Sprintf("%s/%d", c.id, c.sequence))}
	c.index[wait.id] = wait
	heap.Push(&c.pending, wait)
	return wait
}

// disarm removes a deadline and reports whether it was still armed.
func (c *Clock) disarm(id uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	wait, armed := c.index[id]
	if !armed {
		return false
	}
	heap.Remove(&c.pending, wait.heapIndex)
	delete(c.index, id)
	return true
}

// Pending is how many deadlines are armed on this clock, which is what a test
// asserting that a hold was never armed — or that a release disarmed it —
// reads.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending.Len()
}

// Advance moves this clock forward by d and releases every deadline it passes,
// in deadline order and, within one instant, in the seeded order above. It
// reports how many it released.
//
// It does not wait for what it released: a sleeper wakes on its own goroutine,
// and the AfterFunc callbacks one advance released run in that order on a
// goroutine of their own. Inside a synctest bubble, synctest.Wait is what
// reaches the quiescent point after it; elsewhere Settle waits for the
// callbacks this advance started.
//
// The clock reads as the target instant once the advance returns, not as each
// deadline's own. A wait that is delivered a time — a sleep, a timer, a tick —
// is given the instant it was armed for; a callback that reads Now sees where
// the advance ended.
func (c *Clock) Advance(d time.Duration) int {
	if d < 0 {
		panic("sim: a clock does not run backwards")
	}
	c.mu.Lock()
	target := c.now.Add(d)
	released := 0
	var batch []func()
	for c.pending.Len() > 0 {
		next := c.pending.peek()
		if next.deadline.After(target) {
			break
		}
		// The clock stands at the firing instant while this deadline is
		// released, so the time a wait is delivered is the one it was armed
		// for rather than wherever the advance ends.
		c.now = next.deadline
		heap.Pop(&c.pending)
		if next.period > 0 {
			// A ticker rearms before it delivers, so a period that the advance
			// also passes ticks again within this same advance.
			next.deadline = next.deadline.Add(next.period)
			heap.Push(&c.pending, next)
		} else {
			delete(c.index, next.id)
		}
		released++
		if next.call != nil {
			batch = append(batch, next.call)
			continue
		}
		select {
		case next.deliver <- c.now:
		default:
			// A tick nobody has read yet stands in for this one, exactly as the
			// stdlib's ticker drops rather than queues.
		}
	}
	c.now = target
	c.mu.Unlock()
	if len(batch) > 0 {
		// The callbacks one advance released run in that order, on a goroutine
		// of their own so the advance does not wait for them. Running them in
		// order rather than each on its own goroutine is what makes the seeded
		// order above observable: a host that releases the second of two
		// simultaneous holds first must be able to do so reproducibly.
		c.started.Add(1)
		go func() {
			defer c.started.Done()
			for _, call := range batch {
				call()
			}
		}()
	}
	return released
}

// AdvanceTo moves this clock to an instant at or after its current one.
func (c *Clock) AdvanceTo(at time.Time) int {
	c.mu.Lock()
	d := at.Sub(c.now)
	c.mu.Unlock()
	return c.Advance(d)
}

// Settle waits for every AfterFunc callback this clock has started to return.
// A test inside a synctest bubble uses synctest.Wait instead, which also waits
// for what a released sleeper went on to do.
func (c *Clock) Settle() { c.started.Wait() }

type simStopper struct {
	clock *Clock
	id    uint64
}

func (s *simStopper) Stop() bool { return s.clock.disarm(s.id) }

type simTimer struct {
	clock *Clock
	mu    sync.Mutex
	id    uint64
	ticks chan time.Time
}

func (t *simTimer) C() <-chan time.Time { return t.ticks }

func (t *simTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clock.disarm(t.id)
}

func (t *simTimer) Reset(d time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	armed := t.clock.disarm(t.id)
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.id = t.clock.armLocked(d, 0, t.ticks, nil).id
	return armed
}

type simTicker struct {
	clock *Clock
	mu    sync.Mutex
	id    uint64
	ticks chan time.Time
}

func (t *simTicker) C() <-chan time.Time { return t.ticks }

func (t *simTicker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clock.disarm(t.id)
}

func (t *simTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("sim: a ticker needs a positive period")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clock.disarm(t.id)
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.id = t.clock.armLocked(d, d, t.ticks, nil).id
}

// waitQueue orders armed deadlines by instant and then by their seeded draw, so
// that two deadlines at the same instant are released in an order the seed
// chooses rather than the order they were armed in.
type waitQueue []*clockWait

func (q waitQueue) Len() int { return len(q) }

func (q waitQueue) Less(i, j int) bool {
	if !q[i].deadline.Equal(q[j].deadline) {
		return q[i].deadline.Before(q[j].deadline)
	}
	if q[i].order != q[j].order {
		return q[i].order < q[j].order
	}
	return q[i].id < q[j].id
}

func (q waitQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].heapIndex, q[j].heapIndex = i, j
}

func (q *waitQueue) Push(value any) {
	wait := value.(*clockWait)
	wait.heapIndex = len(*q)
	*q = append(*q, wait)
}

func (q *waitQueue) Pop() any {
	old := *q
	n := len(old)
	wait := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return wait
}

func (q waitQueue) peek() *clockWait { return q[0] }

var (
	_ platform.Clock   = (*Clock)(nil)
	_ platform.Entropy = (*entropy)(nil)
)
