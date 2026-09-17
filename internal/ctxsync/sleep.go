package ctxsync

import (
	"context"
	"time"
)

// Sleep waits for d, returning nil once it elapses and context.Cause(ctx) if
// the context is canceled first. The timer is always released, so callers may
// use it inside a retry loop without accumulating pending timers.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
