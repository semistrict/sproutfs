package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/volume"
)

// CaptureInto captures a VM this host runs into a new VM that never boots, and
// reports the new VM's first checkpoint. It is a fork whose child publishes
// its root here and is then closed, without a VMM. The new VM is then like a
// stopped one: any host can open it, or a create can start from it.
//
// The source pauses once, for the state capture and the seal, as a fork's
// parent does. Its checkpoint is pinned, and it keeps running. The child's
// root publishes the pages the pause sealed, read out of the fork point, and
// the VMM state the pause saved. So opening the new VM resumes the guest where
// the pause left the source.
//
// The capture holds the point itself until the child's root has landed or
// failed. So the source's seal ends when this returns, on every path. A child
// whose root did not land is closed, and closing it removes its record. An
// identity that exists is refused before anything is published.
func (h *Host) CaptureInto(ctx context.Context, source, child string) (control.Ref, error) {
	point, err := h.seal(ctx, source)
	if err != nil {
		return control.Ref{}, err
	}
	if err := point.Hold(); err != nil {
		return control.Ref{}, errors.Join(err, retiring(ctx, point))
	}
	root, err := h.captureRoot(ctx, child, point)
	if err != nil {
		return control.Ref{}, errors.Join(fmt.Errorf("capturing %s into %s", source, child), err,
			retiring(ctx, point))
	}
	if err := retiring(ctx, point); err != nil {
		return control.Ref{}, fmt.Errorf("retiring the point %s was captured at: %w", source, err)
	}
	slog.InfoContext(ctx, "host: captured a VM into a new one", "vm", source, "into", child,
		"parent", point.Parent().Sequence, "checkpoint", root.Sequence)
	return root, nil
}

// captureRoot creates the child of a fork point and publishes its root with
// the point's VMM state, and then closes it. The root reads the pages the
// point sealed through the point, as a child on the parent's own host does.
func (h *Host) captureRoot(ctx context.Context, child string, point *volume.ForkPoint) (control.Ref, error) {
	vm, err := h.volumes.Fork(ctx, child, point)
	if err != nil {
		return control.Ref{}, err
	}
	ckpt, err := vm.Snapshot(ctx, volume.Prepared(point.State(), nil))
	if err == nil {
		err = ckpt.Wait(ctx)
	}
	if err != nil {
		return control.Ref{}, errors.Join(fmt.Errorf("publishing the root of %s", child), err,
			closing(ctx, vm))
	}
	if err := vm.Close(ctx); err != nil {
		return control.Ref{}, fmt.Errorf("closing %s: %w", child, err)
	}
	return ckpt.Ref(), nil
}
