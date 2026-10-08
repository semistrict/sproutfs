package vmmemory_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// machine is a campaign guest's VM as its host runs it. Its tasks, a vCPU, a
// flusher or the campaign's last look at its memory, run under its VMM's
// context. Its session ends as production's does: where its region fails of a
// fault its client injected (lostToItsClient), which the host learns of as
// Connection.verify does, and where a request of its guest is answered with
// an error a fault injected, a backing's read, an arena's allocation or the
// spill's I/O, as Connection.serveFaults ends it. The host then ends the VMM,
// which ends every fault and command it had in flight, and once nothing of it
// runs closes it (Connection.Close), which detaches the region. The pages it
// held are then back for the guests that go on, as a host gives a dead VM's
// memory back at once: a campaign that left a lost guest attached to the end
// starved the others of slots. The host also verifies the region on a timer,
// as Connection.verify does. An error no fault injected is the pager's
// failure, which the guest's tasks report.
type machine struct {
	name   string
	region *vmmemory.MemoryRegion
	m      *mapping
	// ctx is the VMM's, which end ends.
	ctx context.Context
	end context.CancelCauseFunc
	// running counts the machine's tasks running, and stopped is closed when
	// it falls to zero, nil while nothing waits for that; both guarded by mu.
	// A task may start after others have stopped, which a WaitGroup a host
	// waits on does not allow.
	mu      sync.Mutex
	running int
	stopped chan struct{}
	// lost is set once the machine is gone (gone), and failed closed once its
	// session has ended of an injected fault.
	lost       atomic.Bool
	failed     chan struct{}
	failedOnce sync.Once
	// closed is set once the machine's region is detached.
	closed bool
}

// errMachineGone ends a lost machine's VMM.
var errMachineGone = errors.New("the machine's VMM has ended")

// boot starts the machine's VMM under world.
func (g *machine) boot(world context.Context) {
	g.ctx, g.end = context.WithCancelCause(world)
	g.failed = make(chan struct{})
}

// run runs one of the machine's tasks, named task, under its VMM's context,
// counted in wg as well.
func (g *machine) run(wg *sync.WaitGroup, task string, body func(ctx context.Context)) {
	wg.Add(1)
	g.mu.Lock()
	g.running++
	g.mu.Unlock()
	go func() {
		defer wg.Done()
		defer g.taskStopped()
		body(sim.WithTask(g.ctx, task))
	}()
}

func (g *machine) taskStopped() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running--
	if g.running == 0 && g.stopped != nil {
		close(g.stopped)
		g.stopped = nil
	}
}

// waitStopped returns once none of the machine's tasks runs. The last to stop
// goes on beside it, and so may whatever that one woke; in a controlled run
// they go on one at a time, in the order it chooses.
func (g *machine) waitStopped(ctx context.Context) error {
	g.mu.Lock()
	if g.running == 0 {
		g.mu.Unlock()
		return nil
	}
	if g.stopped == nil {
		g.stopped = make(chan struct{})
	}
	stopped := g.stopped
	g.mu.Unlock()
	<-stopped
	return sim.Admit(ctx, "campaign/stopped")
}

// gone reports whether the machine is gone: its region failed of a fault its
// client injected, which the campaign takes as a machine ended rather than a
// failure of the pager. Nothing of it is checked again.
func (g *machine) gone() bool {
	if g.lost.Load() {
		return true
	}
	if !lostToItsClient(g.region, g.m) {
		return false
	}
	g.lost.Store(true)
	return true
}

// lose reports whether err ended the machine's session: it is gone, or err is
// a fault the campaign injected, which ends the session as any error a
// request of its guest is answered with does.
func (g *machine) lose(err error) bool {
	if err == nil {
		return false
	}
	if !g.gone() && !injected(err) {
		return false
	}
	g.lost.Store(true)
	g.failedOnce.Do(func() {
		// A machine never booted has no host to tell.
		if g.failed != nil {
			close(g.failed)
		}
	})
	return true
}

// injected reports an error a campaign's fixtures or simulated platform
// injected: a fixture's, a device's (EIO), or a filesystem's refusal for want
// of space, which a simulated disk makes whatever it holds.
func injected(err error) bool {
	return errors.Is(err, errInjected) || errors.Is(err, platform.ErrInjectedFault) ||
		errors.Is(err, platform.ErrNoSpace)
}

// lostToItsClient reports whether region is a machine its client ended: the
// region failed after the client injected a fault it cannot survive (a lost
// command or a refused revocation).
func lostToItsClient(region *vmmemory.MemoryRegion, m *mapping) bool {
	return m.injected.Load() && vmmemory.Failed(region) != nil
}

// verifyInterval is how often a campaign's host verifies a machine's region.
const verifyInterval = 5 * time.Millisecond

// host is the machine's host until stop closes: it verifies the region every
// verifyInterval, and once the machine is gone it ends the VMM, waits for the
// machine's tasks to stop and closes it. ctx is the host's own, not the VMM's.
func (g *machine) host(ctx context.Context, t *testing.T, stop <-chan struct{}) {
	for {
		select {
		case <-vmmemory.Ended(g.region):
		case <-g.failed:
		case <-time.After(verifyInterval):
		case <-stop:
			return
		}
		// Whatever woke the host goes on beside it; in a controlled run they go
		// on one at a time, in the order it chooses. What it does then is
		// decided by what holds by then, not by which of them woke it.
		if err := sim.Admit(ctx, "campaign/host"); err != nil {
			t.Errorf("%s host: %v", g.name, err)
			return
		}
		if g.gone() {
			break
		}
		if vmmemory.Failed(g.region) != nil {
			// A failure of the pager, which the guest's tasks report.
			return
		}
		err := g.region.Verify(ctx)
		if errors.Is(err, vmmemory.ErrClosed) {
			// The machine's own guest stopped it, as a fork campaign's parent
			// may: there is nothing left to host.
			return
		}
		if err != nil && !g.lose(err) {
			t.Errorf("%s verifying: %v", g.name, err)
			return
		}
	}
	g.end(errMachineGone)
	if err := g.waitStopped(ctx); err != nil {
		t.Errorf("%s host: %v", g.name, err)
		return
	}
	g.close(ctx, t)
}

// close detaches a machine nothing of which runs, unless it is detached.
func (g *machine) close(ctx context.Context, t *testing.T) {
	if g.closed {
		return
	}
	if err := g.detach(ctx); err != nil {
		t.Errorf("%s detaching: %v", g.name, err)
	}
}

// detach detaches the machine's region, its VMM's mappings gone with the VMM.
func (g *machine) detach(ctx context.Context) error {
	g.closed = true
	g.m.arena.mu.Lock()
	clear(g.m.pages)
	g.m.arena.mu.Unlock()
	return g.region.Detach(ctx)
}

// hostMachines runs a host for each machine until the returned stop is
// called, which returns once each has stopped: a gone machine is closed by
// then.
func hostMachines(ctx context.Context, t *testing.T, machines []*machine) (stop func()) {
	done := make(chan struct{})
	var hosts sync.WaitGroup
	for _, g := range machines {
		hosts.Go(func() { g.host(sim.WithTask(ctx, g.name+"-host"), t, done) })
	}
	return func() {
		close(done)
		hosts.Wait()
	}
}
