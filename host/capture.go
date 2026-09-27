// Capture, Seal and CreateFork are the host operations that make a running VM
// durable and that start a new one from it.
//
// A capture is the checkpoint: the VMM process is paused, its memory regions
// are sealed, the process resumes, and the checkpoint publishes the sealed
// pages straight out of the arena with the VMM state attached. Sealing moves
// no bytes, so the guest's pause is the state capture and page-table work,
// never the bytes the checkpoint uploads. The checkpoint is returned before the
// publication that makes it durable completes.
//
// A fork is the same pause without the publication. Seal pauses the parent,
// saves its state, seals its dirty set and resumes it — the parent keeps its
// handle, its volumes and its pages — and returns the point a child starts
// from. Nothing is published on the parent's side: the child inherits the
// checkpoint the parent's control record already selects, and the pages written
// since it come out of the parent's sealed pages, over the fork point itself on
// this host and over the parent's page server on another. The destination
// publishes the child's root as soon as it holds them all, and that root is
// what publishes them as the child's own.

package host

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// ErrInvalidCapture reports a missing VM, manager, store or machine.
var ErrInvalidCapture = errors.New("host: invalid capture argument")

// Capture pauses the VM through machine, resumes it as soon as its memory is
// sealed, and publishes a checkpoint of the sealed pages with the captured VMM
// state. It returns as soon as the checkpoint exists, before its publication
// completes; volume.Checkpoint.Wait reports when it became durable.
//
// The guest is therefore paused for the state capture and the seal only, never
// for the bytes the checkpoint moves.
//
// clock measures the pause the guest paid and the upload that ran behind it,
// which are the two numbers the checkpoint's log line carries. Nil is the wall
// clock; a simulated host passes its own so that the line a recorded run prints
// is the line its replay prints.
//
// The pause happens under the VM's publication lock, so two captures of one
// guest — an explicit one and the host's interval checkpoint — serialize there
// rather than racing to seal the same memory regions. A phase that fails after the
// pause began releases the VM: every memory region is unsealed and the guest resumes.
// A VM that refused the capture before its guest was touched is left exactly as
// it was, which is what a VM whose pages a fork point holds does. Once the
// publication has the checkpoints it owns them, so nothing here unseals
// anything: it retires each of them when it lands, and hands their pages back
// to the guest when it does not.
//
// terms says whether the checkpoint is kept and what a failed publication
// does; see volume.Terms.
func Capture(ctx context.Context, vm *volume.VM, machine Machine, clock platform.Clock, terms volume.Terms) (*volume.Checkpoint, error) {
	if machine == nil {
		return nil, ErrInvalidCapture
	}
	return capture(ctx, vm, machine, clock, vm.Snapshot, terms, machine.Prepare)
}

// CaptureDisks is the checkpoint the interval takes: Capture of the VM's disks
// alone. The pause seals the memory regions of its disks and captures no VMM state, so
// the checkpoint publishes what the guest stored into its disks, nothing of its
// RAM, and opening it is a cold boot over those disks — a power cut at the
// moment the pause began. A disk is what a guest expects to survive the loss
// of the machine it runs on, and RAM is not; RAM is uploaded only by Capture,
// which is a request for it.
//
// The pause is the vCPUs stopping and the disks' write-protect commands, with
// no state written anywhere, and every disk is sealed inside the one pause, so
// the checkpoint is a single point in time across all of them: nothing a guest
// stored after a flush is in it without everything it stored before.
//
// terms says whether the checkpoint is kept, and whether a publication that
// fails keeps the disks sealed and publishes again; see volume.Terms.
func CaptureDisks(ctx context.Context, vm *volume.VM, machine Machine, clock platform.Clock, terms volume.Terms) (*volume.Checkpoint, error) {
	if machine == nil {
		return nil, ErrInvalidCapture
	}
	return capture(ctx, vm, machine, clock, vm.SnapshotDisks, terms,
		func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
			sources, err := machine.SealDisks(ctx)
			return nil, sources, err
		})
}

// capture is Capture with the pause's own step: snapshot takes the checkpoint
// on terms, prepare seals what it seals, the guest resumes, and the checkpoint
// publishes behind the running guest.
func capture(ctx context.Context, vm *volume.VM, machine Machine, clock platform.Clock,
	snapshot func(context.Context, volume.PrepareFunc, volume.Terms) (*volume.Checkpoint, error),
	terms volume.Terms, prepare volume.PrepareFunc) (*volume.Checkpoint, error) {
	if vm == nil {
		return nil, ErrInvalidCapture
	}
	clock = platform.ClockOr(clock)
	paused := false
	var pause time.Duration
	ckpt, err := snapshot(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		began := clock.Now()
		paused = true
		state, sources, err := prepare(ctx)
		if err != nil {
			return nil, nil, err
		}
		// The guest runs again from here: the checkpoint reads the pages it
		// sealed while the guest stores into copies of them.
		if err := machine.Resume(ctx); err != nil {
			return nil, nil, err
		}
		pause = clock.Since(began)
		return state, sources, nil
	}, terms)
	if err != nil {
		if !paused {
			return nil, err
		}
		return nil, errors.Join(err, machine.Release(ctx))
	}
	// What the checkpoint cost is only known when its publication ends, which is
	// after this call returns, so the report waits for it on its own. Every
	// checkpoint of every VM goes through here, so one line is logged per
	// checkpoint whether a caller waits for the publication or not.
	go report(context.WithoutCancel(ctx), vm.ID(), ckpt, pause, clock, clock.Now())
	return ckpt, nil
}

// report logs what one checkpoint cost, once it is durable or has failed: the
// dirty set the pause sealed, the pages of it the settle found the guest had
// never stored into, the object-store traffic publishing it took, and the two
// durations — the pause the guest paid and the upload that ran behind it. The
// checkpoint always finishes, so this returns whatever the publication did.
func report(ctx context.Context, vmID string, ckpt *volume.Checkpoint, pause time.Duration, clock platform.Clock, uploaded time.Time) {
	err := ckpt.Wait(ctx)
	pages, bytes := ckpt.Sealed()
	traffic := ckpt.Traffic()
	slog.InfoContext(ctx, "host: checkpoint",
		"vm", vmID, "checkpoint", ckpt.Ref().Sequence,
		"dirty_pages", pages, "dirty_bytes", bytes,
		"unchanged_pages", ckpt.Unchanged(),
		"uploaded_bytes", traffic.Put.Bytes, "objects", traffic.Put.Calls,
		"deleted", traffic.Delete.Calls, "read_bytes", traffic.Get.Bytes,
		"pause_seconds", pause.Seconds(), "upload_seconds", clock.Since(uploaded).Seconds(),
		"error", err)
}

// Seal takes the fork point on a running parent: it pauses the VM through the
// machine, saves its state, seals its dirty set, resumes it, and pins the
// checkpoint its control record selects. It publishes nothing and gives nothing
// up — the parent goes on running with its own volumes and pages.
//
// The parent stays sealed until the returned point is retired, which is when
// the child has published or pulled every page it inherited. Nothing may
// capture the parent in the meantime, which volume.Status reports as Sealed.
func Seal(ctx context.Context, vm *volume.VM, machine Machine) (*volume.ForkPoint, error) {
	if vm == nil || machine == nil {
		return nil, ErrInvalidCapture
	}
	paused := false
	point, err := vm.ForkPoint(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		paused = true
		state, sources, err := machine.Prepare(ctx)
		if err != nil {
			return nil, nil, err
		}
		// The parent runs again from here, storing into copies of the pages
		// the fork keeps.
		if err := machine.Resume(ctx); err != nil {
			return nil, nil, err
		}
		return state, sources, nil
	})
	if err != nil {
		if !paused {
			return nil, err
		}
		return nil, errors.Join(err, machine.Release(ctx))
	}
	return point, nil
}

// CreateFork creates the VM a fork point starts and returns the VMM state to restore
// into it. It copies no byte: the child reads the parent's checkpoint through
// the objects the pin protects, and the pages written since it through the
// point.
//
// The child exists only on this host until its first checkpoint publishes its
// root index; using it anywhere else reports volume.ErrForkPending.
func CreateFork(ctx context.Context, manager *volume.Manager, id string, point *volume.ForkPoint,
	added ...volume.VolumeSpec) (*volume.VM, []byte, error) {
	if manager == nil || point == nil {
		return nil, nil, ErrInvalidCapture
	}
	vm, err := manager.Fork(ctx, id, point, added...)
	if err != nil {
		return nil, nil, err
	}
	return vm, bytes.Clone(point.State()), nil
}

// State reads the VMM state of a published checkpoint, which is how a host that
// never held the checkpoint restores it. A checkpoint whose publication has not
// landed has no index to read; one published without VMM state reports
// checkpoint.ErrNoState.
func State(ctx context.Context, store *checkpoint.Store, ref control.Ref) ([]byte, error) {
	if store == nil {
		return nil, ErrInvalidCapture
	}
	index, err := store.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	return store.ReadState(ctx, index)
}
