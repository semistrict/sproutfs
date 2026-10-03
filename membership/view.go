package membership

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// DefaultInterval is how often a view reads the object when nothing else has
// made it read: a slow timer, to learn of a change while the host is idle. A
// host that talks to its peers learns of one at its first request.
const DefaultInterval = 30 * time.Second

// The probes a view marks.
const (
	// ProbeViewAdopted is a read that found a newer generation.
	ProbeViewAdopted = "membership/view-adopted"
	// ProbeViewKept is a read that failed, which leaves the view with the
	// membership it held.
	ProbeViewKept = "membership/view-kept"
)

// Source is a process's copy of the membership: the generation it holds, and
// a way to read the object again when another process names a newer one.
type Source interface {
	// Current is the membership held now.
	Current() Membership
	// Catch returns the membership held, read again first when it is older
	// than generation. A read that fails leaves it as it was, which the
	// error says.
	Catch(ctx context.Context, generation uint64) (Membership, error)
}

// ViewConfig is where a view reads the membership, and how often.
type ViewConfig struct {
	// Store is where the object is. Nil never reads, and the view holds
	// Initial.
	Store *Store
	// Initial is what the view holds until a read succeeds: the host alone.
	Initial Membership
	// Interval is how often the object is read on the view's own timer. Zero
	// is DefaultInterval, and a negative interval reads only when asked to.
	Interval time.Duration
	Clock    platform.Clock
}

// ViewStatus is the membership a view holds and how it got it.
type ViewStatus struct {
	Membership Membership
	// Read is when the last read that succeeded finished, zero before any.
	Read time.Time
	// Reads counts the reads that succeeded and Failures the ones that did
	// not. Error is why the last read failed, empty when it succeeded.
	Reads, Failures uint64
	Error           string
}

// View is one process's copy of the membership. It only ever moves to a
// newer generation: a read that finds the generation it holds or an older
// one changes nothing, and a read that fails leaves what it held. It is the
// orchestrator's and every host's, and never an authority.
type View struct {
	ctx    context.Context
	cancel context.CancelFunc
	config ViewConfig
	clock  platform.Clock
	done   chan struct{}
	once   sync.Once
	// reading admits one read at a time, so a host behind many requests at
	// once reads the object once, and a slow read cannot land after a later
	// one.
	reading *ctxsync.Mutex
	current atomic.Pointer[Membership]

	mu      sync.Mutex
	status  ViewStatus
	changed chan struct{}
}

var _ Source = (*View)(nil)

// NewView holds config.Initial and, with a store and a positive interval,
// reads the object at once and then every interval until ctx ends or the view
// is closed.
func NewView(ctx context.Context, config ViewConfig) *View {
	if config.Interval == 0 {
		config.Interval = DefaultInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	v := &View{ctx: ctx, cancel: cancel, config: config, clock: platform.ClockOr(config.Clock),
		done: make(chan struct{}), reading: ctxsync.NewMutex(), status: ViewStatus{Membership: config.Initial},
		changed: make(chan struct{})}
	initial := config.Initial
	v.current.Store(&initial)
	if config.Store == nil || config.Interval < 0 {
		close(v.done)
		return v
	}
	// The ticker is armed before the first read, so the next read comes one
	// interval after this one began however long it takes.
	go v.loop(v.clock.NewTicker(config.Interval))
	return v
}

func (v *View) loop(ticker platform.Ticker) {
	defer close(v.done)
	defer ticker.Stop()
	for {
		v.refresh()
		select {
		case <-v.ctx.Done():
			return
		case <-ticker.C():
		}
	}
}

// refresh reads the object on the timer. A failure is logged once, when the
// reads start failing, and again when they recover.
func (v *View) refresh() {
	failing := v.Status().Error != ""
	_, err := v.Refresh(v.ctx)
	switch {
	case err != nil && !failing && v.ctx.Err() == nil:
		slog.WarnContext(v.ctx, "membership: reading the membership failed; keeping the one held",
			"generation", v.Current().Generation(), "error", err)
	case err == nil && failing:
		slog.InfoContext(v.ctx, "membership: reading the membership again", "generation", v.Current().Generation())
	}
}

// Current is the membership held now.
func (v *View) Current() Membership { return *v.current.Load() }

// Refresh reads the object now, and returns the membership held after it.
func (v *View) Refresh(ctx context.Context) (Membership, error) {
	return v.read(ctx, 0, true)
}

// Catch returns the membership held, read again first when it is older than
// generation. Of the requests that wait on one read, the first reads and the
// rest find it done.
func (v *View) Catch(ctx context.Context, generation uint64) (Membership, error) {
	return v.read(ctx, generation, false)
}

// read reads the object, unless always is false and the view already holds
// generation or later once the read before it is done.
func (v *View) read(ctx context.Context, generation uint64, always bool) (Membership, error) {
	if !always && v.Current().Generation() >= generation {
		return v.Current(), nil
	}
	if v.config.Store == nil {
		return v.Current(), nil
	}
	if err := v.reading.Lock(ctx); err != nil {
		return v.Current(), err
	}
	defer v.reading.Unlock()
	if !always && v.Current().Generation() >= generation {
		return v.Current(), nil
	}
	read, err := v.config.Store.Read(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		v.status.Failures++
		v.status.Error = err.Error()
		sim.Probe(ctx, ProbeViewKept)
		return v.status.Membership, err
	}
	v.status.Reads++
	v.status.Read = v.clock.Now()
	v.status.Error = ""
	if read.Generation() > v.status.Membership.Generation() {
		sim.Probe(ctx, ProbeViewAdopted)
		v.status.Membership = read
		v.current.Store(&read)
		close(v.changed)
		v.changed = make(chan struct{})
	}
	return v.status.Membership, nil
}

// Changed is closed when the view next moves to a newer generation.
func (v *View) Changed() <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.changed
}

// Status is the membership held now and how it was read.
func (v *View) Status() ViewStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.status
}

// Close stops the reads and waits for the one in flight. It is safe to call
// repeatedly.
func (v *View) Close() {
	v.once.Do(v.cancel)
	<-v.done
}

// Fixed is a source that holds what it is given and reads nothing: a test's,
// or a bench's whose processes are all given one membership.
type Fixed struct {
	current atomic.Pointer[Membership]
}

var _ Source = (*Fixed)(nil)

// NewFixed holds m.
func NewFixed(m Membership) *Fixed {
	f := &Fixed{}
	f.Set(m)
	return f
}

// Set holds m from now on.
func (f *Fixed) Set(m Membership) { f.current.Store(&m) }

// Current is what it holds.
func (f *Fixed) Current() Membership { return *f.current.Load() }

// Catch is what it holds, which no generation changes.
func (f *Fixed) Catch(context.Context, uint64) (Membership, error) { return f.Current(), nil }
