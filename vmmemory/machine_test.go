package vmmemory_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// machine is a campaign guest's VM as its host runs it. Its tasks, a vCPU, a
// flusher or the campaign's last look at its memory, run under its VMM's
// context. When its region fails of a fault its client injected
// (lostToItsClient), the host learns of it from the session, as
// Connection.verify does, and ends the VMM, which ends every fault and
// command it had in flight; once nothing of it runs, the host closes it
// (Connection.Close), which detaches the region. The pages it held are then
// back for the guests that go on, as a host gives a dead VM's memory back at
// once: a campaign that left a lost guest attached to the end starved the
// others of slots. A region that fails of anything else is the pager's
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
	// lost is set once the machine is gone (gone).
	lost atomic.Bool
	// closed is set once the machine's region is detached.
	closed bool
}

// errMachineGone ends a lost machine's VMM.
var errMachineGone = errors.New("the machine's VMM has ended")

// boot starts the machine's VMM under world.
func (g *machine) boot(world context.Context) {
	g.ctx, g.end = context.WithCancelCause(world)
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

// lose reports whether err ended the machine: it is gone.
func (g *machine) lose(err error) bool { return err != nil && g.gone() }

// lostToItsClient reports whether region is a machine its client ended: the
// region failed after the client injected a fault it cannot survive (a lost
// command or a refused revocation).
func lostToItsClient(region *vmmemory.MemoryRegion, m *mapping) bool {
	return m.injected.Load() && vmmemory.Failed(region) != nil
}

// host is the machine's host until stop closes: once the machine is gone, it
// ends the VMM, waits for the machine's tasks to stop and closes it. ctx is
// the host's own, not the VMM's.
func (g *machine) host(ctx context.Context, t *testing.T, stop <-chan struct{}) {
	select {
	case <-vmmemory.Ended(g.region):
	case <-stop:
		return
	}
	// Whatever ended the region goes on beside the host; in a controlled run
	// they go on one at a time, in the order it chooses.
	if err := sim.Admit(ctx, "campaign/close"); err != nil {
		t.Errorf("%s host: %v", g.name, err)
		return
	}
	if !g.gone() {
		return
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
