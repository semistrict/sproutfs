package volume

import (
	"context"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
)

// DirtySource is one volume's pager state at a checkpoint: the pager pages the
// pager holds privately when the guest is sealed, the bytes of each, and what
// becomes of them when the publication ends. It is satisfied by
// *vmmemory.RegionCheckpoint.
//
// A checkpoint given sources publishes their pages alongside its own overlay.
// Nothing copies those bytes into this package on the way: the upload reads the
// frames the guest was running on, which is why a checkpoint costs the pause of
// a seal rather than the pause of a copy.
type DirtySource interface {
	// DirtyPages reports the pages this seal holds, in ascending order. A pager
	// page is a store page, so these are page numbers of the volume. The set is
	// fixed for the life of the seal.
	DirtyPages() []uint64
	// ReadDirty fills dst, exactly one page of checkpoint.PageSize bytes, with the
	// bytes the seal froze. A store the guest made since then is not in them.
	ReadDirty(ctx context.Context, page uint64, dst []byte) error
	// UnpublishedAge is how long the oldest of these pages has gone unpublished,
	// zero where the seal holds none. It is what a fork point hands a child on
	// another host, so that child inherits the parent's loss window with the
	// pages it is measured over rather than starting one of its own.
	UnpublishedAge() time.Duration
	// Hold marks this seal as a fork instant's, which a fork point does when it
	// takes the instant. Such a seal lasts as long as the children of that
	// instant rather than as long as an upload, which is no bound a waiting
	// store may wait under.
	Hold()
	// Share offers the frames this seal holds to whatever else runs on this
	// host, under the identity ref gives each of this volume's pages: a machine
	// that inherits that identity maps the frame rather than reading the page.
	// Nothing is copied and nothing becomes durable — the offer lasts exactly as
	// long as the seal, whose bytes cannot change while it does.
	//
	// It is a fork point that calls it, when a child of that instant is taken
	// in on this host, because a fork point's reference is published under by
	// nothing, ever, so the names it gives these pages are every child of that
	// instant's and no one else's.
	Share(ctx context.Context, ref control.Ref, volume string) error
	// Retire ends the seal. published reports that the checkpoint which read
	// these pages was selected, so their bytes are this volume's now; otherwise
	// they go back to the guest as dirty state and the next checkpoint takes
	// them.
	//
	// It is the whole seal's, never one page's or one part's. Retiring a
	// page publishes its frame under the identity this volume reports for it,
	// which is the checkpoint's only once that checkpoint's index has been
	// selected in the control record — a page retired when the part holding it was
	// uploaded would be published under the checkpoint it is replacing. A
	// publication that fails after some of its parts landed must also hand every
	// page back to the guest, and a page already retired as clean has nothing to
	// hand back. So the copy-on-write cost of a large checkpoint is held until the
	// checkpoint is durable, and it is the publication's memory, not the guest's,
	// that the parts bound.
	Retire(ctx context.Context, published bool) error
}

// sealedSource presents a checkpoint's sealed pager pages as the immutable
// state its overlay sits on. Those pages hold bytes no checkpoint has yet: they
// belong to the checkpoint that is publishing them and to everything that
// inherits it afterwards, exactly like the bytes the overlay holds.
type sealedSource struct {
	parent source
	ref    control.Ref
	// sources is the pager state per volume, and pages the set of pager pages
	// each one publishes, for the lookups a read and a locate do per page.
	sources map[string]DirtySource
	pages   map[string]map[uint64]bool
}

// newSealedSource wraps parent, or returns parent unchanged when no volume of
// this checkpoint has a pager behind it.
func newSealedSource(parent source, ref control.Ref, sources map[string]DirtySource) source {
	if len(sources) == 0 {
		return parent
	}
	s := sealedSource{parent: parent, ref: ref, sources: sources,
		pages: make(map[string]map[uint64]bool, len(sources))}
	for name, src := range sources {
		set := make(map[uint64]bool)
		for _, page := range src.DirtyPages() {
			set[page] = true
		}
		s.pages[name] = set
	}
	return s
}

// read fills dst from the sealed frames where they hold the bytes and from the
// inherited checkpoint everywhere else. A page is read whole, because that is
// the unit the pager can serve.
func (s sealedSource) read(ctx context.Context, volume string, offset uint64, dst []byte) error {
	src := s.sources[volume]
	if src == nil {
		return s.parent.read(ctx, volume, offset, dst)
	}
	size := uint64(checkpoint.PageSize)
	held := s.pages[volume]
	var page []byte
	for cursor := offset; cursor < offset+uint64(len(dst)); {
		number := cursor / size
		stop := min(offset+uint64(len(dst)), (number+1)*size)
		target := dst[cursor-offset : stop-offset]
		if !held[number] {
			if err := s.parent.read(ctx, volume, cursor, target); err != nil {
				return err
			}
			cursor = stop
			continue
		}
		if uint64(len(target)) == size {
			// A publication reads whole aligned pages, and the pager fills them
			// where they are wanted: no staging buffer, and nothing of one page
			// held while the next is read.
			if err := src.ReadDirty(ctx, number, target); err != nil {
				return err
			}
			cursor = stop
			continue
		}
		if page == nil {
			page = make([]byte, size)
		}
		if err := src.ReadDirty(ctx, number, page); err != nil {
			return err
		}
		copy(target, page[cursor-number*size:stop-number*size])
		cursor = stop
	}
	return context.Cause(ctx)
}

// locate reports the sealed pages under the reference of the checkpoint that
// publishes them, and everything else under the identity the inherited
// checkpoint gives it. Every reported extent lies inside one page, which is
// what the pager's frame identity needs.
func (s sealedSource) locate(ctx context.Context, volume string, offset, length uint64) ([]control.Extent, error) {
	src := s.sources[volume]
	if src == nil {
		return s.parent.locate(ctx, volume, offset, length)
	}
	size := uint64(checkpoint.PageSize)
	held := s.pages[volume]
	var result []control.Extent
	add := func(next control.Extent) {
		if n := len(result); n > 0 && result[n-1].Identity == next.Identity && result[n-1].Offset+result[n-1].Length == next.Offset {
			result[n-1].Length += next.Length
			return
		}
		result = append(result, next)
	}
	end := offset + length
	for cursor := offset; cursor < end; {
		number := cursor / size
		stop := min(end, (number+1)*size)
		if !held[number] {
			// Runs of inherited pages are located in one call rather than one
			// per pager page.
			for stop < end && !held[stop/size] {
				stop = min(end, stop+size)
			}
			extents, err := s.parent.locate(ctx, volume, cursor, stop-cursor)
			if err != nil {
				return nil, err
			}
			for _, item := range extents {
				add(item)
			}
			cursor = stop
			continue
		}
		for position := cursor; position < stop; {
			page := position / checkpoint.PageSize
			next := min(stop, (page+1)*checkpoint.PageSize)
			add(control.Extent{Offset: position, Length: next - position,
				Identity: control.Identity{Ref: s.ref, Volume: volume, Page: page}})
			position = next
		}
		cursor = stop
	}
	return result, nil
}
