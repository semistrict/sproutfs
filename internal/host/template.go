package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	hostapi "github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

const (
	// importBatchBytes is how much of a guest image one write carries, and
	// importCheckpointBytes how much is written before a checkpoint publishes
	// it: an import that never checkpointed would hold the whole image in this
	// process's overlay.
	importBatchBytes      = 1 << 20
	importCheckpointBytes = 32 << 20
)

// DefaultTemplateWait is how long a host waits for another host's import of the
// same guest image to publish before it takes that import for one whose host is
// gone. It is generous: what it bounds is a whole image read and checkpointed
// over the object store, and waiting longer than that costs a host that is
// starting anyway, while recovering too early fences an import that was fine
// and makes the deployment read the image twice.
const DefaultTemplateWait = 10 * time.Minute

// templatePoll is how often a host waiting on another host's import re-reads
// the template's control record. It is one small GET, and it is what the wait
// above is spent on.
const templatePoll = 500 * time.Millisecond

// TemplateImport is one guest image the deployment creates VMs from.
type TemplateImport struct {
	// Image is the name of the guest image in this host's configuration. It
	// says which of the configured images this is and nothing about the
	// template's identity, which is the image's own bytes.
	Image string
	// Volumes are the template's volumes and Root the one the image is written
	// into. Everything else reads as the zeroes a cold boot starts from, which
	// is what the RAM of a VM that has never run is.
	Volumes []volume.VolumeSpec
	Root    string
	// Source is the image itself, read twice: once for the digest that names
	// the template, and once more for the bytes that go into it, so the
	// identity cannot disagree with what was imported under it.
	Source io.ReadSeeker
	// Wait is how long this host waits for another host's import of this image
	// to publish before it recovers it. Zero is DefaultTemplateWait; a negative
	// wait recovers an unfinished import at once, which is what a test that has
	// already established the host that wrote it is gone wants.
	Wait time.Duration
}

// ImportedTemplate is one guest image in a published checkpoint: the identity
// it was imported under and the pinned fork point every VM created from it is
// forked at. The VM has no guest and nothing holds it open — a template is
// written once and read for ever after — so the point seals nothing and any
// number of VMs on any number of hosts start from it.
type ImportedTemplate struct {
	id string
	// Point is the checkpoint a create forks. It is rebuilt from what the
	// template's record pins rather than held by a writer of it: the host that
	// imported the image is not the only one that forks it, and need not be
	// running.
	Point *volume.ForkPoint
}

// ID is the identity this image is the template of, which is the image's own
// sha256.
func (t *ImportedTemplate) ID() string { return t.id }

// TemplateOf returns the fork point every VM of one guest image is forked at,
// importing the image if the deployment has no template of it yet. Forking it
// copies nothing — the fork inherits the checkpoint — so every VM created here
// reads the image's pages through this host's shared cache.
//
// The identity is the image's bytes, so this is the same template on every host
// configured with that image. What the deployment already holds of it decides
// what this does:
//
//   - published — its record pins the checkpoint it selects — and nothing is
//     opened and nothing is written. The template is read the way any host
//     reads a checkpoint it did not publish, which is what makes a restart free
//     and a second host's start free;
//   - absent, and the image is imported: the template is created, the image
//     written into its root volume and checkpointed, and that checkpoint
//     pinned, which is what makes the import published;
//   - a record with no pin, which is an import that has not finished. It is
//     waited for, because an import in flight is the ordinary reason for it and
//     opening the record would take the epoch out from under the host doing it.
//     Past the wait, the host that wrote it is taken for gone and the template
//     is recovered as any VM is: this host takes the epoch, which fences a
//     writer that turns out to be alive, and imports again under it.
//
// Two hosts starting together race on the record's create-if-absent. The one
// that loses is refused the create, reads the winner's record and waits for it,
// which is the third case above.
func (h *Host) TemplateOf(ctx context.Context, request TemplateImport) (*ImportedTemplate, error) {
	if request.Source == nil || request.Root == "" || len(request.Volumes) == 0 {
		return nil, fmt.Errorf("%w: %w", ErrRequest, ErrInvalidConfig)
	}
	digest, err := imageDigest(request.Source)
	if err != nil {
		return nil, fmt.Errorf("reading the %s image: %w", request.Image, err)
	}
	id := hostapi.TemplateID(digest)
	wait := request.Wait
	if wait == 0 {
		wait = DefaultTemplateWait
	}
	deadline := h.clock.Now().Add(wait)
	for {
		now := h.clock.Now()
		record, err := h.control.Read(ctx, id)
		switch {
		case errors.Is(err, platform.ErrNotFound):
			pinned, err := h.importTemplate(ctx, id, request)
			if errors.Is(err, volume.ErrExists) {
				// Another host wrote the record between the read and the
				// create. Its import is the one this host waits for.
				continue
			}
			if err != nil {
				return nil, err
			}
			slog.InfoContext(ctx, "host: a guest image is imported", "image", request.Image,
				"template", id, "checkpoint", pinned)
			return h.templatePoint(ctx, id, pinned)
		case err != nil:
			return nil, fmt.Errorf("reading the control record of template %s: %w", id, err)
		case record.IsPinned(record.Selected):
			return h.templatePoint(ctx, id, record.Selected)
		case !now.Before(deadline):
			slog.WarnContext(ctx, "host: recovering a template whose import never published",
				"image", request.Image, "template", id, "epoch", record.Epoch, "waited", wait)
			pinned, err := h.recoverTemplate(ctx, id, request)
			if err != nil {
				return nil, err
			}
			return h.templatePoint(ctx, id, pinned)
		default:
			// Never past the deadline: the next read is the one that decides
			// the import's host is gone, and it happens when it is due.
			if err := h.clock.Sleep(ctx, min(templatePoll, deadline.Sub(now))); err != nil {
				return nil, err
			}
		}
	}
}

// imageDigest is the sha256 of a guest image, which is the whole of what names
// its template. The file is left where the import wants it: at the front.
func imageDigest(source io.ReadSeeker) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return digest, err
	}
	sum := sha256.New()
	if _, err := io.CopyBuffer(sum, source, make([]byte, importBatchBytes)); err != nil {
		return digest, err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return digest, err
	}
	return [sha256.Size]byte(sum.Sum(nil)), nil
}

// templatePoint rebuilds the fork point a create forks from one published
// checkpoint of a template. Nothing is opened: the pin on that checkpoint is
// the template's own and permanent — nothing in this deployment gives a pin
// back — so a fork of it inherits checkpoints nothing reclaims whether or not
// the host that imported it still exists.
func (h *Host) templatePoint(ctx context.Context, id string, sequence uint64) (*ImportedTemplate, error) {
	point, err := h.volumes.Inherit(ctx, control.Ref{VM: id, Sequence: sequence})
	if err != nil {
		return nil, fmt.Errorf("opening checkpoint %d of template %s: %w", sequence, id, err)
	}
	return &ImportedTemplate{id: id, Point: point}, nil
}

// importTemplate creates the template and imports the image into it, reporting
// the checkpoint it pinned. It is refused with volume.ErrExists when another
// host got the record first, which is the loser of the create-if-absent race.
func (h *Host) importTemplate(ctx context.Context, id string, request TemplateImport) (uint64, error) {
	vm, err := h.volumes.CreateIfAbsent(ctx, id, request.Volumes)
	if err != nil {
		if errors.Is(err, volume.ErrExists) {
			return 0, err
		}
		return 0, fmt.Errorf("creating template %s: %w", id, err)
	}
	return h.fillTemplate(ctx, id, vm, request)
}

// recoverTemplate takes over a template whose import never published and
// imports the image again under it. Opening it advances the epoch, which fences
// the host that wrote the record if it is still writing; what that host
// published stays where it is, as a superseded epoch's checkpoints do wherever
// a VM is taken over.
func (h *Host) recoverTemplate(ctx context.Context, id string, request TemplateImport) (uint64, error) {
	vm, err := h.volumes.Open(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("recovering the unfinished template %s: %w", id, err)
	}
	return h.fillTemplate(ctx, id, vm, request)
}

// fillTemplate writes the image into a template, publishes it and pins the
// checkpoint every VM created from it is forked at, reporting that checkpoint.
//
// The handle goes at the end. A template is written once and read for ever
// after: holding it open would keep one host's epoch on an identity every host
// names, and the pin it leaves is what a fork of it reads through.
func (h *Host) fillTemplate(ctx context.Context, id string, vm *volume.VM,
	request TemplateImport) (uint64, error) {
	if _, err := request.Source.Seek(0, io.SeekStart); err != nil {
		return 0, errors.Join(fmt.Errorf("rereading the %s image", request.Image), err,
			closing(ctx, vm))
	}
	if err := importImage(ctx, vm, request.Root, request.Source); err != nil {
		return 0, errors.Join(fmt.Errorf("importing the %s image into template %s", request.Image, id), err,
			closing(ctx, vm))
	}
	// The image has to be in object storage before anything forks it: the
	// checkpoint a fork inherits is a published one, and a template has no
	// guest to hold anything back.
	if err := vm.Checkpoint(ctx); err != nil {
		return 0, errors.Join(fmt.Errorf("checkpointing template %s", id), err, closing(ctx, vm))
	}
	// Nothing pauses and nothing is sealed: the point is the published
	// checkpoint itself, pinned so nothing reclaims it under the VMs that
	// inherit it. The pin is also what says this import finished — a record
	// that pins the checkpoint it selects is a template every host may fork.
	point, err := vm.ForkPoint(ctx, volume.Prepared(nil, nil))
	if err != nil {
		return 0, errors.Join(fmt.Errorf("pinning template %s", id), err, closing(ctx, vm))
	}
	pinned := point.Parent().Sequence
	if err := errors.Join(point.Retire(ctx), vm.Close(ctx)); err != nil {
		return 0, fmt.Errorf("releasing template %s: %w", id, err)
	}
	return pinned, nil
}

// importImage writes a guest image into a volume, checkpointing as it goes so
// that the import's cost is bounded by importCheckpointBytes rather than by the
// image. Runs of zeroes are skipped: the volume already reads as zeroes, and a
// page that is never written is a page no object is ever published for.
func importImage(ctx context.Context, vm *volume.VM, name string, file io.Reader) error {
	target := vm.Volume(name)
	if target == nil {
		return fmt.Errorf("%w: the template has no volume named %s", ErrRequest, name)
	}
	buffer := make([]byte, importBatchBytes)
	zeroes := make([]byte, importBatchBytes)
	var offset, pending uint64
	for {
		count, err := io.ReadFull(file, buffer)
		if count > 0 {
			if offset+uint64(count) > target.Size() {
				return fmt.Errorf("the image is larger than the %d-byte volume", target.Size())
			}
			if !bytes.Equal(buffer[:count], zeroes[:count]) {
				if err := target.Write(ctx, offset, buffer[:count]); err != nil {
					return err
				}
				pending += uint64(count)
			}
			offset += uint64(count)
		}
		if pending >= importCheckpointBytes {
			if err := vm.Checkpoint(ctx); err != nil {
				return err
			}
			pending = 0
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
