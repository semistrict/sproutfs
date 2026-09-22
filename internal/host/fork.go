package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// forkHold is one child this host holds a fork point for: the parent that
// point was taken on, the point itself, whether the child is being taken in
// here, and the deadline that retires it when nothing releases it. The parent is
// what says which holds a host fenced out of that parent must give up.
//
// The release is the destination's word that it has every page it inherited,
// carried by the orchestrator, and the deadline is what keeps a parent from
// being sealed for good when that word never comes — an orchestrator that
// restarted, a destination that died. The parent then takes its pages back and
// is checkpointed again; the child falls back to the checkpoint it was forked
// from, which its record already selects.
//
// It is one table for both destinations. A child on another host is served the
// point's pages out of this host's page server; a child taken in here maps
// the pages themselves. What the hold means, when it ends and what ending it
// costs are the same either way.
type forkHold struct {
	parent string
	point  *volume.ForkPoint
	// local reports a child this host takes in itself, which receives the
	// handoff over the fork point rather than over the page server. It is what
	// says the point is the one that Receive binds the child's regions to.
	local bool
	// timer retires the hold when nothing releases it. It is armed on the
	// host's clock, so a simulation reaches the deadline by advancing to it
	// rather than by waiting several checkpoint intervals for it.
	timer platform.Stopper
}

// forkedFrom reports the children this host serves a fork point of one parent
// to, in ascending identity order.
func (h *Host) forkedFrom(parent string) []string {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	var children []string
	for child, hold := range h.machines.forked {
		if hold.parent == parent {
			children = append(children, child)
		}
	}
	slices.Sort(children)
	return children
}

// inherited reports the fork point one child of a fork this host took is to be
// received over, which exists only for a child this host takes in itself. Every
// other receive — a migration, or a child whose parent runs elsewhere — has
// none, and rebuilds what it inherits from the checkpoint the parent pinned.
func (h *Host) inherited(child string) *volume.ForkPoint {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	if hold := h.machines.forked[child]; hold != nil && hold.local {
		return hold.point
	}
	return nil
}

// Fork takes one fork point on a VM this host runs and hands every child of
// it over, exactly as a migration hands a VM over — except that nothing is
// given up: the parent pauses for its VMM state capture and the seal, is
// running again before this returns, and keeps its handle, its volumes and its
// pages. Each child inherits the checkpoint the parent's control record
// already selects and the pages written since it.
//
// One pause starts them all: a fan-out of forks is one fork point.
//
// The returned handoffs are what the deployment gives each child's destination,
// whichever host that is. A child destined for another host is served its
// inherited pages out of this host's page server until ReleaseMigrated releases
// it; a child taken in here — destination empty, or this host's own page
// address — is served nothing at all, because the fork point it attaches over is
// the pages themselves. Either way the parent's seal ends when the last hold
// is released, and only then is the parent checkpointed again.
func (h *Host) Fork(ctx context.Context, parent string, children []string,
	destination platform.Address) ([]vmmigrate.Handoff, error) {
	if len(children) == 0 {
		return nil, fmt.Errorf("%w: a fork of %s names no child", ErrInvalidConfig, parent)
	}
	// Only a child that leaves this host needs an endpoint to leave through.
	local := destination == "" || destination == h.migration.Address
	if !local && h.pages == nil {
		return nil, fmt.Errorf("%w: no migration endpoint is configured", ErrNotMigratable)
	}
	point, err := h.seal(ctx, parent)
	if err != nil {
		return nil, err
	}
	// The fan-out itself holds the point until every child of it has a hold of
	// its own. A child released while a later one had not been recorded yet
	// would otherwise retire the point and hand the parent pages the next child
	// still has to inherit.
	if err := point.Hold(); err != nil {
		return nil, err
	}
	handoffs := make([]vmmigrate.Handoff, 0, len(children))
	for _, child := range children {
		// The hold is this child's, recorded before the child exists anywhere
		// and under the deadline every handover gets: a takeover of the parent
		// found in between still finds the hold to give up, and a child nothing
		// ever releases does not seal the parent for good.
		if err := h.hold(parent, child, point, local); err != nil {
			return nil, err
		}
		// The pin on the parent's checkpoint is written here, by the parent's own
		// writer, because a destination has no handle on the parent to write one
		// with. It is the one pin the point took, shared by every child of it,
		// and nothing ever gives it back.
		var handoff vmmigrate.Handoff
		err := point.Pin(ctx)
		if err == nil {
			handoff, err = vmmigrate.Fork(ctx, child, point, h.served(local), vmmigrate.Options{})
		}
		if err != nil {
			// The whole fan-out is given up: a child this host cannot hand over
			// is one no destination will ever take, and the holds of the ones
			// before it keep the parent sealed.
			err = errors.Join(err, h.Abandon(child))
			for _, handed := range handoffs {
				err = errors.Join(err, h.Abandon(handed.VMID))
			}
			return nil, errors.Join(fmt.Errorf("forking %s into %s", parent, child), err,
				retiring(ctx, point))
		}
		handoffs = append(handoffs, handoff)
		slog.InfoContext(ctx, "host: forked a VM", "vm", parent, "child", child,
			"checkpoint", handoff.ParentCheckpoint, "destination", destination,
			"pause_began", handoff.PausedAt)
	}
	// Every child has a hold of its own from here, so the fan-out's own hold is
	// given back. It happens on a context of its own: a caller that hung up
	// between the last child and this would otherwise leave the parent holding
	// a point nobody counts.
	if err := retiring(ctx, point); err != nil {
		return nil, fmt.Errorf("retiring the point %s was forked at: %w", parent, err)
	}
	return handoffs, nil
}

// served is the page server a fork's children are handed over through, and
// nothing for a child this host takes in itself: no page of such a child ever
// reaches the wire, so nothing is registered and nothing is released.
func (h *Host) served(local bool) *vmmigrate.PageSource {
	if local {
		return nil
	}
	return h.pages
}

// hold records one child this host holds a fork point for, under the deadline
// that retires the point when nothing releases it. It is taken before the child
// exists, so a takeover of the parent found in between still finds it.
func (h *Host) hold(parent, child string, point *volume.ForkPoint, local bool) error {
	if err := point.Hold(); err != nil {
		return err
	}
	hold := &forkHold{parent: parent, point: point, local: local}
	hold.timer = h.clock.AfterFunc(h.handoffTimeout(), func() { h.expire(child) })
	h.machines.mu.Lock()
	h.machines.forked[child] = hold
	h.machines.mu.Unlock()
	return nil
}

// seal is the fork point itself: the pause, the state capture and the seal of
// one VM this host runs, with the checkpoint the child inherits pinned.
func (h *Host) seal(ctx context.Context, vmID string) (*volume.ForkPoint, error) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	if entry == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotMigratable, vmID)
	}
	vm := h.vm(vmID)
	if vm == nil {
		return nil, fmt.Errorf("%w: %s is not open here", ErrNotMigratable, vmID)
	}
	if err := confirmHandoff(ctx, vm); err != nil {
		return nil, err
	}
	return Seal(ctx, vm, entry.runtime)
}
