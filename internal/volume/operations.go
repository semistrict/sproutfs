package volume

import (
	"bytes"
	"context"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// WriteExtent is one range of a batched write. Data replaces the volume's bytes
// from Offset; an empty payload writes nothing.
type WriteExtent struct {
	Offset uint64
	Data   []byte
}

// change is one range a write replaces. A nil payload is a discard, which makes
// the range read as zeroes including bytes inherited from the checkpoint.
type change struct {
	offset, length uint64
	data           []byte
}

// Read fills dst from one consistent overlay-plus-checkpoint view. It is served
// from the overlay and from the checkpoint objects the index names. On error dst
// may be partially filled.
func (v *Volume) Read(ctx context.Context, offset uint64, dst []byte) error {
	if !validRange(v.size, offset, uint64(len(dst))) {
		return ErrInvalidRange
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	current := v.vm.current.Load()
	if current.err != nil {
		return current.err
	}
	base := func(ctx context.Context, offset uint64, dst []byte) error {
		return current.base.read(ctx, v.name, offset, dst)
	}
	return readOverlay(ctx, base, current.overlays[v.ordinal], offset, dst)
}

// Load is Read under another name, for a pager whose fault path must not
// contact the network. Neither call does more than the other.
func (v *Volume) Load(ctx context.Context, offset uint64, dst []byte) error {
	return v.Read(ctx, offset, dst)
}

// LoadPages is Load of only the pages of the range that wanted marks — one
// element per page the range touches, or nil for every one of them — leaving
// the bytes of every other page as the caller had them.
//
// It is one read whatever the mask leaves out, which is what a pager's window
// is: a fault's read-ahead run with the pages the region already holds resident
// taken out of it. Those pages cost neither a request nor bytes, and the rest
// of the run is still grouped by the part it lies in, so the run costs what the
// run costs rather than one request per stretch of it.
func (v *Volume) LoadPages(ctx context.Context, offset uint64, dst []byte, wanted []bool) error {
	if !validRange(v.size, offset, uint64(len(dst))) {
		return ErrInvalidRange
	}
	if len(dst) == 0 {
		return context.Cause(ctx)
	}
	size := v.geometry.PageSize
	if wanted != nil && uint64(len(wanted)) != (offset+uint64(len(dst))-1)/size-offset/size+1 {
		return ErrInvalidRange
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	current := v.vm.current.Load()
	if current.err != nil {
		return current.err
	}
	base := func(ctx context.Context, offset uint64, dst []byte, wanted []bool) error {
		return current.base.readPages(ctx, v.name, offset, dst, wanted)
	}
	return readOverlayPages(ctx, base, current.overlays[v.ordinal], size, offset, dst, wanted)
}

// Write replaces one range in the VM's in-memory overlay and returns. It
// contacts nothing and waits for nothing: the bytes become durable when the next
// checkpoint publishes them, and are lost if this host dies first.
func (v *Volume) Write(ctx context.Context, offset uint64, data []byte) error {
	return v.WriteBatch(ctx, []WriteExtent{{Offset: offset, Data: data}})
}

// WriteBatch replaces several ranges as one overlay generation, so a checkpoint
// holds all of them or none. Overlapping extents are applied in order. The
// combined payload is bounded by Config.MaxWriteBytes.
func (v *Volume) WriteBatch(ctx context.Context, extents []WriteExtent) error {
	total := 0
	changes := make([]change, 0, len(extents))
	for _, item := range extents {
		if !validRange(v.size, item.Offset, uint64(len(item.Data))) {
			return ErrInvalidRange
		}
		if len(item.Data) == 0 {
			continue
		}
		total += len(item.Data)
		offset := item.Offset
		if sim.Bug(ctx, "volume-shift-write") {
			offset++
		}
		changes = append(changes, change{offset: offset, length: uint64(len(item.Data)), data: bytes.Clone(item.Data)})
	}
	if total > v.vm.manager.config.MaxWriteBytes && !sim.Bug(ctx, "volume-unbounded-write") {
		return ErrWriteTooLarge
	}
	if len(changes) == 0 {
		return nil
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return v.vm.apply(v.ordinal, changes)
}

// Discard makes a range read as zeroes, including bytes inherited from the
// checkpoint. However large the range, it costs one overlay entry.
func (v *Volume) Discard(ctx context.Context, offset, length uint64) error {
	if !validRange(v.size, offset, length) {
		return ErrInvalidRange
	}
	if length == 0 {
		return nil
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if sim.Bug(ctx, "volume-ignore-discard") {
		return nil
	}
	return v.vm.apply(v.ordinal, []change{{offset: offset, length: length}})
}

// Verify confirms that this handle still owns its VM. It makes nothing durable
// and orders nothing: durability is a checkpoint, and a checkpoint is taken on
// the interval or on request, never by a guest's flush. A caller that needs the
// bytes in object storage calls Checkpoint.
func (v *Volume) Verify(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	v.vm.mu.Lock()
	defer v.vm.mu.Unlock()
	return v.vm.readyLocked()
}

// Locate reports the page identity of every byte of a range as sorted,
// adjacent extents covering it exactly. Bytes the overlay holds
// report this VM's next checkpoint reference, so they are private and unshared
// until that checkpoint publishes them; everything else reports the identity the
// checkpoint index gives it, which a fork inherits unchanged.
func (v *Volume) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	if !validRange(v.size, offset, length) {
		return nil, ErrInvalidRange
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	current := v.vm.current.Load()
	if current.err != nil {
		return nil, current.err
	}
	return locateOverlay(ctx, current.base, current.overlays[v.ordinal], current.owner,
		v.geometry, v.name, offset, length)
}
