package rank

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// DefaultInterval is how often a host reads the list of caches.
const DefaultInterval = 10 * time.Second

// errOrchestratorDown is what a read the simulation failed reports.
var errOrchestratorDown = errors.New("rank: the list of caches could not be read")

// FollowerConfig is where a host reads the list of caches, and how often.
type FollowerConfig struct {
	// Initial is the list held until a read succeeds: the host alone.
	Initial List
	// Read reads the list. Nil never reads, and the host stays with Initial.
	Read func(ctx context.Context) (List, error)
	// Interval is how often the list is read. Zero is DefaultInterval, and a
	// negative interval reads only when Refresh is called.
	Interval time.Duration
	Clock    platform.Clock
}

// FollowerStatus is the list a host holds and how it got it.
type FollowerStatus struct {
	List List
	// Read is when the last read that succeeded finished, zero before any.
	Read time.Time
	// Reads counts the reads that succeeded and Failures the ones that did
	// not. Error is why the last read failed, empty when it succeeded.
	Reads, Failures uint64
	Error           string
}

// Follower keeps the last list of caches it read. A read that fails leaves
// the list as it was: two hosts that hold different lists disagree only about
// whom to ask, and the worst a stale list costs is a miss.
type Follower struct {
	ctx    context.Context
	cancel context.CancelFunc
	config FollowerConfig
	clock  platform.Clock
	done   chan struct{}
	once   sync.Once
	// reading admits one read at a time, so a slow read cannot land after a
	// later one and put an older list back.
	reading *ctxsync.Mutex

	mu     sync.Mutex
	status FollowerStatus
}

// NewFollower holds config.Initial and, with a reader and a positive
// interval, reads the list at once and then every interval until ctx ends or
// the follower is closed.
func NewFollower(ctx context.Context, config FollowerConfig) *Follower {
	if config.Interval == 0 {
		config.Interval = DefaultInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	f := &Follower{ctx: ctx, cancel: cancel, config: config, clock: platform.ClockOr(config.Clock),
		done: make(chan struct{}), reading: ctxsync.NewMutex(), status: FollowerStatus{List: config.Initial}}
	if config.Read == nil || config.Interval < 0 {
		close(f.done)
		return f
	}
	// The ticker is armed before the first read, so the next read comes one
	// interval after this one began however long it takes.
	go f.loop(f.clock.NewTicker(config.Interval))
	return f
}

func (f *Follower) loop(ticker platform.Ticker) {
	defer close(f.done)
	defer ticker.Stop()
	for {
		f.refresh()
		select {
		case <-f.ctx.Done():
			return
		case <-ticker.C():
		}
	}
}

// refresh reads the list on the timer. A failure is reported once, when the
// reads start failing, and again when they recover, rather than at every
// tick of an orchestrator that is down for an hour.
func (f *Follower) refresh() {
	failing := f.Status().Error != ""
	err := f.Refresh(f.ctx)
	switch {
	case err != nil && !failing && f.ctx.Err() == nil:
		slog.WarnContext(f.ctx, "rank: reading the list of caches failed; keeping the list held",
			"caches", f.List().Len(), "error", err)
	case err == nil && failing:
		slog.InfoContext(f.ctx, "rank: reading the list of caches again", "caches", f.List().Len())
	}
}

// Refresh reads the list now. A read that fails keeps the list held and is
// reported here and in Status.
func (f *Follower) Refresh(ctx context.Context) error {
	if f.config.Read == nil {
		return nil
	}
	if err := f.reading.Lock(ctx); err != nil {
		return err
	}
	defer f.reading.Unlock()
	list, err := f.read(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.status.Failures++
		f.status.Error = err.Error()
		if f.status.Reads == 0 {
			// Nothing has been read, so the host is still alone.
			sim.Probe(ctx, "rank/alone-until-the-list-is-read")
		} else {
			sim.Probe(ctx, "rank/list-kept-after-a-failed-read")
		}
		if sim.Bug(ctx, "rank-forget-list-on-failure") {
			f.status.List = f.config.Initial
		}
		return err
	}
	if !list.Equal(f.status.List) {
		if f.status.Reads > 0 && sim.Bug(ctx, "rank-keep-first-list") {
			list = f.status.List
		} else {
			sim.Probe(ctx, "rank/list-replaced")
		}
	}
	f.status.List = list
	f.status.Reads++
	f.status.Read = f.clock.Now()
	f.status.Error = ""
	return nil
}

// read reads the list once, through the simulation's faults: an orchestrator
// that is down, and one whose list has not yet caught up with a cache the
// rest of the cluster already knows.
func (f *Follower) read(ctx context.Context) (List, error) {
	if sim.Buggify(ctx, "rank/list-read-fails", 0.3) {
		return List{}, errOrchestratorDown
	}
	list, err := f.config.Read(ctx)
	if err != nil {
		return List{}, err
	}
	if caches := list.Caches(); len(caches) > 1 && sim.Buggify(ctx, "rank/list-lags-a-cache", 0.3) {
		list = list.Without(caches[len(caches)-1].Identity)
	}
	return list, nil
}

// List is the list held now.
func (f *Follower) List() List {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status.List
}

// Status is the list held now and how it was read.
func (f *Follower) Status() FollowerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// Close stops the reads and waits for the one in flight. It is safe to call
// repeatedly.
func (f *Follower) Close() {
	f.once.Do(f.cancel)
	<-f.done
}
