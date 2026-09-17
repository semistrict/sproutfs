package volume

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
)

// Checkpoint is a local, immutable view of one VM at one write generation,
// together with the publication that will make it durable. Its Ref is known
// immediately, so a fork can inherit it and a pager can key frames by it before
// any object is uploaded; Wait reports when the index is selected in the VM's
// control record, which is when the checkpoint survives the loss of this host.
//
// A Checkpoint is safe for concurrent use.
type Checkpoint struct {
	// owner is the handle that captured this checkpoint, which is the one that
	// selects its index and whose control record a fork of it pins.
	owner *VM
	ref   control.Ref
	// parent is the checkpoint this one inherits, parentIndex the index the
	// publication builds on, overlays what the VM wrote on top of it, and
	// position the write generation that wrote them. sources is the sealed
	// pager state of the volumes that have a pager behind them, and base
	// presents it over parent as one immutable view.
	parent      source
	base        source
	parentIndex *checkpoint.Index
	overlays    map[string]*extentIndex
	sources     map[string]DirtySource
	// inherited names, per volume, the pages a fork's root index publishes
	// beyond its own: the parent's unpublished pages at the instant it was
	// forked, which it reads through that instant and makes its own.
	inherited map[string][]uint64
	sizes     map[string]uint64
	position  generation
	state     []byte
	hasState  bool
	// dropState publishes a checkpoint naming no VMM state rather than one that
	// goes on naming its parent's, and resized the sizes this checkpoint gives
	// the volumes it names. Both belong to a cold boot and to nothing else: the
	// memory the state describes is gone, and the shape of a VM can change only
	// at the moment nothing in memory describes it.
	dropState bool
	resized   map[string]uint64

	// meter counts the object-store calls this checkpoint's publication makes,
	// which is what one checkpoint cost in traffic. It is attributed by context,
	// so the uploads count into it wherever they run.
	meter platform.ObjectMeter

	// done closes once the publication has finished, successfully or not.
	done chan struct{}
	err  error
	// swept is closed once the sweep behind the publication has run: the
	// deletes of what the checkpoint this one replaced no longer needs. It
	// closes after done, because neither the guest nor the next capture waits
	// for deletes.
	swept chan struct{}
}

// Ref names this checkpoint. It is known before any object is uploaded, so
// lineage can be established immediately.
func (c *Checkpoint) Ref() control.Ref { return c.ref }

// State returns the VMM state captured with this checkpoint, nil when it has
// none. The bytes are owned by the checkpoint and must not be modified.
func (c *Checkpoint) State() []byte { return c.state }

// Sealed reports what this checkpoint froze in the pager: the pages of every
// volume that has one behind it, and their bytes. A page is checkpoint.PageSize,
// so the byte count is the page count times that. It is the dirty set this
// checkpoint publishes, known as soon as it is captured.
//
// A volume written through this package rather than through a pager contributes
// nothing here; its bytes are in the overlay.
func (c *Checkpoint) Sealed() (pages int, bytes uint64) {
	for _, source := range c.sources {
		held := len(source.DirtyPages())
		pages += held
		bytes += uint64(held) * checkpoint.PageSize
	}
	return pages, bytes
}

// Traffic reports the object-store calls this checkpoint's publication made. It
// is complete once Wait has returned; read before that it reports the calls
// made so far.
func (c *Checkpoint) Traffic() platform.ObjectTraffic { return c.meter.Traffic() }

// Wait returns once this checkpoint's index has been published and selected
// in the VM's control record. It reports the publication's failure if it had
// one; the VM retries a failed publication on its own, which produces a later
// checkpoint rather than completing this one.
func (c *Checkpoint) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.done:
		return c.err
	}
}

// Read fills dst from a volume of this checkpoint. It is served entirely from
// the local immutable view, so it neither contacts the network nor waits for
// the publication. On error dst may be partially filled.
func (c *Checkpoint) Read(ctx context.Context, volume string, offset uint64, dst []byte) error {
	size, found := c.sizes[volume]
	if !found {
		return ErrUnknownVolume
	}
	if !validRange(size, offset, uint64(len(dst))) {
		return ErrInvalidRange
	}
	return c.read(ctx, volume, offset, dst)
}

func (c *Checkpoint) read(ctx context.Context, volume string, offset uint64, dst []byte) error {
	base := func(ctx context.Context, offset uint64, dst []byte) error {
		return c.base.read(ctx, volume, offset, dst)
	}
	return readOverlay(ctx, base, c.overlays[volume], offset, dst)
}

func (c *Checkpoint) locate(ctx context.Context, volume string, offset, length uint64) ([]control.Extent, error) {
	if _, found := c.sizes[volume]; !found {
		return nil, ErrUnknownVolume
	}
	// Every entry a checkpoint holds is published by that checkpoint.
	return locateOverlay(ctx, c.base, c.overlays[volume], lineage{next: c.ref}, volume, offset, length)
}

// retire ends every pager seal this checkpoint read from. A published
// checkpoint's pages become the pager's clean state under it; an abandoned
// one's go back to the guest, so the next checkpoint takes them again and
// nothing is lost but the upload.
func (c *Checkpoint) retire(ctx context.Context, published bool) error {
	var result error
	for _, name := range slices.Sorted(maps.Keys(c.sources)) {
		if err := c.sources[name].Retire(ctx, published); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

// finish releases everything waiting on the publication. err is safe to read
// once done is closed.
// Swept waits for the sweep that ran behind this checkpoint's publication. Wait
// returns before it; a caller that wants the replaced checkpoint's objects
// gone, not only this one's landed, waits here.
func (c *Checkpoint) Swept(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.swept:
		return c.err
	}
}

func (c *Checkpoint) finish(err error) {
	c.err = err
	close(c.done)
}

// checkpointSource supplies whole pages to the publication that writes them. The
// bytes are the checkpoint's, never the VM's live state.
type checkpointSource struct{ checkpoint *Checkpoint }

// ReadPage implements checkpoint.Source.
func (s checkpointSource) ReadPage(ctx context.Context, volume string, page uint64, dst []byte) error {
	return s.checkpoint.read(ctx, volume, page*checkpoint.PageSize, dst)
}
