package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
)

const defaultDrainConcurrency = 4

// DrainBudget bounds one drain: how long the whole of it may take, how long one
// VM's handover may take inside that, and how many VMs move at once.
//
// A drain has to bound itself. The deployment's is a preStop hook, and what is
// waiting on the other end of it is a termination grace period after which the
// pod is killed with whatever it still holds; the caller's context is the
// kubelet's request and says nothing about that. A drain that returns having
// moved some of its VMs leaves the rest to the host loss the kill is; a drain
// that never returns leaves all of them to it.
type DrainBudget struct {
	// Timeout is the whole drain's, and PerVM one VM's handover inside it. Zero
	// selects no bound, which only a caller that bounds its own context wants.
	Timeout, PerVM time.Duration
	// Concurrency bounds how many VMs are handed over at once. Zero selects
	// MigrationConfig.DrainConcurrency, and zero there selects four.
	Concurrency int
}

// DrainVia hands every VM this host runs over through move, and reports which
// of them moved and which are still here. It is what the deployment's drain
// uses: the orchestrator drives both halves of every migration, so this host
// asks it to move each VM rather than migrating on its own account.
//
// A VM that did not move keeps running here and goes on being checkpointed, so
// a drain that gave up on it costs the VM nothing beyond the host loss that
// follows.
func (h *Host) DrainVia(ctx context.Context, budget DrainBudget,
	move func(ctx context.Context, vmID string) error) (moved, remaining []string, err error) {
	if budget.Timeout > 0 {
		whole, cancel := context.WithTimeout(ctx, budget.Timeout)
		defer cancel()
		ctx = whole
	}
	ids := h.Machines()
	failures := eachVM(ctx, ids, h.drainConcurrency(budget), func(ctx context.Context, vmID string) error {
		if budget.PerVM > 0 {
			one, cancel := context.WithTimeout(ctx, budget.PerVM)
			defer cancel()
			ctx = one
		}
		return move(ctx, vmID)
	})
	var errs []error
	for index, vmID := range ids {
		if failures[index] != nil {
			remaining = append(remaining, vmID)
			errs = append(errs, fmt.Errorf("draining %s: %w", vmID, failures[index]))
			continue
		}
		moved = append(moved, vmID)
	}
	return moved, remaining, errors.Join(errs...)
}

// drainConcurrency is how many VMs one drain hands over at once: what the
// budget asks for, what this host was configured with, and four.
func (h *Host) drainConcurrency(budget DrainBudget) int {
	if budget.Concurrency > 0 {
		return budget.Concurrency
	}
	return h.migration.DrainConcurrency
}

// eachVM runs action for every VM named, at most concurrent of them at once,
// and reports what each returned in the order the VMs were named. A VM whose
// turn never came because the context ended reports that instead.
//
// A drain is planned work whose cost is one host's pages: moving every VM at
// once would put all of them on the network together, and moving them one at a
// time would not finish inside any grace period worth having.
func eachVM(ctx context.Context, ids []string, concurrent int,
	action func(ctx context.Context, vmID string) error) []error {
	if concurrent <= 0 {
		concurrent = defaultDrainConcurrency
	}
	slots := make(chan struct{}, concurrent)
	results := make([]error, len(ids))
	var wg sync.WaitGroup
	for index, vmID := range ids {
		wg.Go(func() {
			select {
			case <-ctx.Done():
				results[index] = context.Cause(ctx)
				return
			case slots <- struct{}{}:
			}
			defer func() { <-slots }()
			results[index] = action(ctx, vmID)
		})
	}
	wg.Wait()
	return results
}

// Drain migrates every VM this host runs, which is what a planned restart or a
// scale-down does before the process exits. Concurrency is bounded: a drain is
// planned work, and moving every VM's memory at once would put all of it on the
// network together.
//
// It returns the handoffs of the VMs that moved and joins the failures of the
// ones that did not, which keep running here.
func (h *Host) Drain(ctx context.Context, destinations func(vmID string) platform.Address) ([]vmmigrate.Handoff, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if h.pages == nil {
		return nil, fmt.Errorf("%w: no migration endpoint is configured", ErrNotMigratable)
	}
	if destinations == nil {
		return nil, ErrInvalidConfig
	}
	// Machines is in identity order, so the handoffs are too.
	ids := h.Machines()
	handed := make([]vmmigrate.Handoff, len(ids))
	failures := eachVM(ctx, ids, h.migration.DrainConcurrency, func(ctx context.Context, vmID string) error {
		index := slices.Index(ids, vmID)
		handoff, err := h.Migrate(ctx, vmID, destinations(vmID))
		if err != nil {
			return err
		}
		handed[index] = handoff
		return nil
	})
	var handoffs []vmmigrate.Handoff
	var errs []error
	for index, vmID := range ids {
		if failures[index] != nil {
			errs = append(errs, fmt.Errorf("migrate %s: %w", vmID, failures[index]))
			continue
		}
		handoffs = append(handoffs, handed[index])
	}
	return handoffs, errors.Join(errs...)
}
