package volume

import (
	"context"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
)

// source is the immutable state a VM's overlays sit on: the index of the
// checkpoint its control record selects, or, for a fork whose parent publication
// has not finished, the local view of the checkpoint it forked from.
type source interface {
	// read fills dst from a volume of this source.
	read(ctx context.Context, volume string, offset uint64, dst []byte) error
	// locate reports where the current bytes of a range live. It may fetch the
	// segments of the checkpoint index a range falls in, which is why it takes
	// a context; nothing on this path reads a page.
	locate(ctx context.Context, volume string, offset, length uint64) ([]control.Extent, error)
}

// indexSource is a published checkpoint read through the object store.
type indexSource struct {
	store *checkpoint.Store
	index *checkpoint.Index
}

func (s indexSource) read(ctx context.Context, volume string, offset uint64, dst []byte) error {
	return s.store.Read(ctx, s.index, volume, offset, dst)
}

func (s indexSource) locate(ctx context.Context, volume string, offset, length uint64) ([]control.Extent, error) {
	return s.index.Locate(ctx, volume, offset, length)
}

// publisher names the checkpoint the bytes an overlay holds will be published
// under. A publication in flight already owns the sequence it froze, so the
// entries at or below its position report that sequence and the writes that
// continued after it report the next one. One reference therefore never names
// two different contents.
type publisher struct {
	frozen    generation
	frozenRef control.Ref
	next      control.Ref
}

func (p publisher) ref(at generation) control.Ref {
	if !p.frozenRef.IsZero() && at <= p.frozen {
		return p.frozenRef
	}
	return p.next
}

// locateOverlay reports the page identity of every byte of a range. A page
// the overlay touched anywhere is republished whole by the checkpoint that will
// publish it, so all of it belongs to that checkpoint: private to it until it is
// published and shared by everything that inherits it afterwards. Pages the
// overlay did not touch come from the inherited source.
//
// A page written both before and after a publication froze its generation
// belongs to the later checkpoint, which is the one that will publish the
// bytes this view reads.
//
// Every reported extent lies inside one page of this volume's own geometry,
// which is the identity a pager keys a resident page by.
func locateOverlay(ctx context.Context, parent source, overlay *extentIndex, owner publisher,
	geometry checkpoint.Geometry, volume string, offset, length uint64) ([]control.Extent, error) {
	end := offset + length
	if length == 0 {
		return nil, nil
	}
	written := make(map[uint64]generation)
	for item := range overlay.between(offset, end) {
		first := geometry.PageOf(max(item.start, offset))
		last := geometry.PageOf(min(item.end, end) - 1)
		for page := first; page <= last; page++ {
			if at, seen := written[page]; !seen || item.generation > at {
				written[page] = item.generation
			}
		}
	}
	var result []control.Extent
	add := func(next control.Extent) {
		if n := len(result); n > 0 && result[n-1].Identity == next.Identity && result[n-1].Offset+result[n-1].Length == next.Offset {
			result[n-1].Length += next.Length
			return
		}
		result = append(result, next)
	}
	for page, last := geometry.PageOf(offset), geometry.PageOf(end-1); page <= last; page++ {
		start, stop := max(offset, page*geometry.PageSize), min(end, (page+1)*geometry.PageSize)
		at, touched := written[page]
		if touched {
			add(control.Extent{Offset: start, Length: stop - start,
				Identity: control.Identity{Ref: owner.ref(at), Volume: volume, Page: page}})
			continue
		}
		// Inherited pages are located in one call per run rather than one each.
		for page < last {
			if _, next := written[page+1]; next {
				break
			}
			page++
			stop = min(end, (page+1)*geometry.PageSize)
		}
		extents, err := parent.locate(ctx, volume, start, stop-start)
		if err != nil {
			return nil, err
		}
		for _, item := range extents {
			add(item)
		}
	}
	return result, nil
}
