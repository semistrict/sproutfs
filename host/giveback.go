package host

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The bounds of one VM's give-back in one pass. It compares at most
// giveBackPages copies and giveBackBytes of them, whichever is fewer: 512 pages
// at 2 MiB and 4096 at 4 KiB. Each page costs a write-protect, a comparison and,
// where the page goes back, one mapping command, all with the guest running, so
// the bound is what keeps that work a trickle beside the guest rather than a
// stall. A copy is compared once unless it goes back, so what is left over is
// taken up by the next pass. A VM that has made a pass's worth of copies since
// the last one is given back at once rather than at the next interval.
const giveBackBytes = 1 << 30

// giveBackPages is a variable only so a test can make a pass's worth of copies
// small enough to make.
var giveBackPages = 4096

// givingBack gives back the RAM copies of one VM whose bytes its guest never
// changed: see vmmemory.MemoryRegion.GiveBack. RAM is never checkpointed on
// the interval, so nothing else would. A disk's copies are settled by the
// checkpoint the interval takes of it instead. The cold copies of either are
// given back sooner by their sessions, so what this finds is the copies those
// left: see vmmemory.MemoryRegion.GiveBackColdCopies.
//
// It keeps a schedule of its own, the give-back interval, because it has
// nothing to do with checkpoints: it reads none, pauses nothing, and a host
// that checkpoints nothing still wants its guests' pages held once. A pass
// runs at each interval, or as soon as a memory region has made a pass's worth
// of copies, and only over memory regions with something to give back, so an
// idle guest costs none. Each pass is a callback of the host's clock rather
// than a goroutine of its own, so a VM costs one timer between passes.
//
// The schedule is armed before this returns, so the first interval runs from
// the moment the machine was added. The schedule ends with ctx: wait returns
// once it has and the pass in flight, if any, has returned.
func (h *Host) givingBack(ctx context.Context, vmID string, entry *registration) (wait func()) {
	g := &giveBack{h: h, ctx: ctx, vmID: vmID, regions: entry.runtime.MemoryRegions()}
	g.names = slices.Sorted(maps.Keys(g.regions))
	for _, name := range g.names {
		if memory := g.regions[name]; memory.Kind() == vmmemory.Ram {
			memory.NotifyCopies(giveBackLimit(memory), g.due)
		}
	}
	g.schedule(jittered(h.entropy, h.giveBackInterval))
	return func() {
		<-ctx.Done()
		g.mu.Lock()
		g.stopped = true
		g.cancelLocked()
		g.mu.Unlock()
		g.passes.Wait()
	}
}

// giveBack is one VM's give-back schedule: the pass the clock will run next,
// whether one is running, and whether another was asked for while it ran.
type giveBack struct {
	h       *Host
	ctx     context.Context
	vmID    string
	regions map[string]*vmmemory.MemoryRegion
	names   []string
	passes  sync.WaitGroup

	mu      sync.Mutex
	next    platform.Stopper
	running bool
	again   bool
	stopped bool
}

// schedule has the clock run the next pass in d, in place of the one it would
// have run. Caller does not hold mu.
func (g *giveBack) schedule(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scheduleLocked(d)
}

func (g *giveBack) scheduleLocked(d time.Duration) {
	if g.stopped {
		return
	}
	g.cancelLocked()
	g.passes.Add(1)
	g.next = g.h.clock.AfterFunc(d, func() {
		defer g.passes.Done()
		g.pass()
	})
}

// cancelLocked stops the pass scheduled next. One the clock had not started
// yet never runs, so it is counted out here. Caller holds mu.
func (g *giveBack) cancelLocked() {
	if g.next != nil && g.next.Stop() {
		g.passes.Done()
	}
	g.next = nil
}

// due asks for a pass now: a memory region has made a pass's worth of copies.
// It runs on the copying fault's goroutine under the region's own lock, so it
// only schedules, and a pass already running runs again when it ends.
func (g *giveBack) due() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		g.again = true
		return
	}
	g.scheduleLocked(0)
}

// pass gives back what each RAM memory region has pending, then schedules the
// next pass: at once when one was asked for meanwhile, and otherwise an
// interval from now.
func (g *giveBack) pass() {
	g.mu.Lock()
	if g.stopped || g.running {
		g.mu.Unlock()
		return
	}
	g.running = true
	g.mu.Unlock()
	for _, name := range g.names {
		memory := g.regions[name]
		if memory.Kind() != vmmemory.Ram || !memory.GiveBackPending() {
			continue
		}
		given, err := memory.GiveBack(g.ctx, giveBackLimit(memory))
		switch {
		case err == nil:
			if given > 0 {
				slog.DebugContext(g.ctx, "host: gave back unchanged RAM copies", "vm", g.vmID, "volume", name, "pages", given)
			}
		case g.ctx.Err() != nil:
		case errors.Is(err, vmmemory.ErrClosed), errors.Is(err, vmmemory.ErrHandedOff):
			// The VM is leaving this host, and its schedule with it.
		default:
			slog.WarnContext(g.ctx, "host: giving back a VM's unchanged RAM copies failed",
				"vm", g.vmID, "volume", name, "error", err)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = false
	if g.again {
		g.again = false
		g.scheduleLocked(0)
		return
	}
	g.scheduleLocked(jittered(g.h.entropy, g.h.giveBackInterval))
}

// giveBackLimit is how many of one memory region's copies a pass compares.
func giveBackLimit(memory *vmmemory.MemoryRegion) int {
	return min(giveBackPages, int(giveBackBytes/memory.PageSize()))
}
