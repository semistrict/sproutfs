package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Source supplies the whole contents of a page a publication is republishing.
// ReadPage must fill dst with the page's contents as of the checkpoint; dst is
// the page's length, which is the volume's own page size except for a tail page
// shorter than one. It must fill all of it: the publication reads its pages
// into buffers it reuses, so bytes a source leaves untouched are an earlier
// page's. A publication reads one page at a time, in page order, from one
// goroutine.
type Source interface {
	ReadPage(ctx context.Context, volume string, page uint64, dst []byte) error
}

// Publication builds one checkpoint. Dirty records the pages that changed since
// the parent; Commit reads each of them whole from the Source, writes them into
// parts with the VMM state — its own where SetState gave it one, and otherwise
// the parent's, which it goes on naming — and whatever compaction rescues,
// uploads those parts, and then writes the index object holding the segments
// they changed and the root, which is the commit. Nothing is visible until the
// caller selects the checkpoint in the control record. A failed Commit leaves
// only unreferenced objects, and retrying with the same Ref is idempotent. A
// Publication is not safe for concurrent use.
type Publication struct {
	store  *Store
	parent *Index
	ref    control.Ref
	// sizes is each volume's size as this checkpoint publishes it, and geometry
	// the page geometry each was created with, inherited from the parent and
	// never changed by a publication: a volume's page size is fixed for its life.
	sizes    map[string]uint64
	geometry map[string]Geometry
	// ephemeral is the volumes no checkpoint holds, inherited like the
	// geometry and fixed for each volume's life.
	ephemeral map[string]bool
	edits     map[string]map[uint64]bool
	protected map[control.Ref]bool
	// dirty is the segments of each volume whose page table this publication
	// changed, by number. They are written into its index object after every
	// part is durable, so a page compaction moved is in the segment that names
	// it; every other segment entry keeps the address an earlier checkpoint's
	// index object gave it. Holding the segment rather than a flag is what lets
	// the liveness measurement account for a changed table without opening it
	// again: a segment is here only because this publication has it in hand.
	dirty    map[string]map[uint64]*segment
	state    []byte
	hasState bool
	// dropState publishes a checkpoint that names no VMM state at all, rather
	// than going on naming the parent's. A cold boot needs it, because the
	// memory the state describes is being discarded in the same publication,
	// and so does a checkpoint of the disks alone, because the disks it
	// publishes are not the ones the parent's state was captured over. Either
	// way state beside the pages it would restore is a moment that never
	// existed.
	dropState bool
	// vcpus is the processor count this checkpoint records, zero to keep the
	// parent's.
	vcpus uint32
	// nested is what this checkpoint records of Index.Nested when setNested
	// says it records anything; otherwise it keeps the parent's.
	nested, setNested bool
	// keep is the pull of the VM publishing, which keeps what this
	// publication uploads; nil for a VM not pulling its memory.
	keep *Pull
	// working is the segments this publication has taken out of its parent's
	// tables to change, each a copy of its own.
	working map[segmentKey]*segment
	// written is the tables of the segments it wrote into its index object,
	// as a reader decodes them, which the index it publishes keeps for its
	// readers once the object is durable.
	written []writtenTable
	err     error
}

// writtenTable is one segment a publication wrote, decoded.
type writtenTable struct {
	volume string
	number uint64
	table  *pageTable
}

// Begin starts a checkpoint that inherits parent, which may be nil for a VM
// with no history, and publishes under ref. A fork passes the source VM's index
// as parent and its own ref.
func (s *Store) Begin(parent *Index, ref control.Ref) *Publication {
	p := &Publication{store: s, parent: parent, ref: ref,
		sizes: make(map[string]uint64), geometry: make(map[string]Geometry),
		ephemeral: make(map[string]bool), edits: make(map[string]map[uint64]bool),
		protected: make(map[control.Ref]bool), dirty: make(map[string]map[uint64]*segment),
		working: make(map[segmentKey]*segment)}
	if parent != nil {
		for _, name := range parent.names {
			p.sizes[name] = parent.volumes[name].size
			p.geometry[name] = parent.volumes[name].geometry
			p.ephemeral[name] = parent.volumes[name].ephemeral
		}
	}
	if !control.ValidID(ref.VM) || ref.Sequence == 0 {
		p.err = ErrInvalidConfig
	}
	return p
}

// Protect names the sequences of this VM a fork pinned or a checkpoint request
// kept, which compaction must leave alone: their objects are the fork's as much
// as this VM's, or will be. A protected checkpoint protects every checkpoint
// its index names, not only itself: a fork reads its whole view through them,
// so rewriting one copies bytes reclamation cannot free while it is protected.
func (p *Publication) Protect(sequences []uint64) {
	for _, sequence := range sequences {
		p.protected[control.Ref{VM: p.ref.VM, Sequence: sequence}] = true
	}
}

// SetSize truncates or extends a volume the parent already has. Sizes must be
// whole numbers of sectors. Extending a volume exposes zeroes; truncating one
// only hides bytes, so a caller that must not expose them again after a later
// extension zeroes the pages itself.
//
// It cannot create a volume: a volume's geometry is chosen when it is created
// and a resize inherits it, so a name the parent does not have is one this
// publication could not say the page size of.
func (p *Publication) SetSize(volume string, size uint64) {
	if !validName(volume) || size%SectorSize != 0 {
		p.fail(ErrInvalidConfig)
		return
	}
	if _, known := p.geometry[volume]; !known {
		p.fail(ErrUnknownVolume)
		return
	}
	p.sizes[volume] = size
}

// Add gives this checkpoint a volume its parent does not have, at spec's size
// and page size, reading as zeroes. It is how a cold boot adds a disk: a create
// gives its VM the ephemeral disk it asked for this way. A name the parent
// already has is refused, because that volume's geometry is its own for life.
func (p *Publication) Add(volume string, spec VolumeSpec) {
	table, err := spec.table(volume)
	if err != nil {
		p.fail(err)
		return
	}
	if _, known := p.geometry[volume]; known {
		p.fail(ErrInvalidConfig)
		return
	}
	p.sizes[volume], p.geometry[volume], p.ephemeral[volume] = table.size, table.geometry, table.ephemeral
}

// Dirty records that a page changed and must be republished whole. Commit reads
// it from the Source; a page that reads as all zeroes then leaves the index
// instead of being written. A page of an ephemeral volume fails the Commit with
// ErrEphemeral: no checkpoint holds one.
func (p *Publication) Dirty(volume string, page uint64) {
	if !validName(volume) {
		p.fail(ErrInvalidConfig)
		return
	}
	pages := p.edits[volume]
	if pages == nil {
		pages = make(map[uint64]bool)
		p.edits[volume] = pages
	}
	pages[page] = true
}

// SetState attaches VMM state to this checkpoint. The bytes are copied.
func (p *Publication) SetState(data []byte) {
	if len(data) > maximumStateSize {
		p.fail(ErrInvalidRange)
		return
	}
	p.state, p.hasState = slices.Clone(data), true
	p.dropState = false
}

// DropState publishes a checkpoint with no VMM state, rather than one that goes
// on naming the parent's. A checkpoint with no state of its own keeps the
// parent's, because the VM stays restorable from the last capture between
// captures; the one case where that is wrong is a checkpoint that discards the
// guest's memory, because state and memory describe one moment and half of it
// is a VM nothing can resume.
func (p *Publication) DropState() {
	p.state, p.hasState, p.dropState = nil, false, true
}

// SetVCPUs records how many processors a boot of this checkpoint gives the
// guest. A checkpoint that sets none keeps its parent's count.
func (p *Publication) SetVCPUs(vcpus int) {
	if vcpus < 1 || vcpus > maximumVCPUs {
		p.fail(ErrInvalidConfig)
		return
	}
	p.vcpus = uint32(vcpus)
}

// SetNested records whether a boot of this checkpoint is a nested VM (see
// Index.Nested). A checkpoint that sets nothing keeps its parent's.
func (p *Publication) SetNested(nested bool) {
	p.nested, p.setNested = nested, true
}

// maximumVCPUs bounds the processor count a checkpoint records, which is the
// most a Firecracker guest can have.
const maximumVCPUs = 32

// Keep has every member and segment this publication uploads kept in pull's
// copy as it lands, so the VM pulling its memory reads them from this host's
// disk once they are evicted, as it reads the checkpoint it started from. A
// publication that fails leaves envelopes no index names, which cost the copy
// their space and are never read.
func (p *Publication) Keep(pull *Pull) { p.keep = pull }

func (p *Publication) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

// Commit publishes the checkpoint. Every part is uploaded and waited for before
// the index object, drawing on the store's host-wide upload budget of
// Config.Concurrency slots, so a Commit interrupted at any point leaves the
// parent checkpoint readable and only unreferenced objects behind: the index
// object is what makes this checkpoint openable, and it lands only once
// everything it names is durable. Publications that run at the same time share
// that budget rather than each getting one of their own.
func (p *Publication) Commit(ctx context.Context, source Source) (*Index, error) {
	if p.err != nil {
		return nil, p.err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	index := newIndex(p.store, p.ref)
	for name, size := range p.sizes {
		geometry := p.geometry[name]
		if !geometry.supported() {
			return nil, ErrInvalidConfig
		}
		index.volumes[name] = &volumeTable{size: size, geometry: geometry, ephemeral: p.ephemeral[name],
			segments: p.inherit(name, size)}
		index.names = append(index.names, name)
	}
	slices.Sort(index.names)
	index.inheritMemo(p.parent)
	for name := range p.edits {
		table := index.volumes[name]
		if table == nil {
			return nil, ErrUnknownVolume
		}
		if table.ephemeral {
			return nil, fmt.Errorf("%w: %s", ErrEphemeral, name)
		}
	}
	if p.parent != nil {
		for ref, entry := range p.parent.checkpoints {
			index.checkpoints[ref] = entry
		}
		index.state = p.parent.state
		index.vcpus = p.parent.vcpus
		index.nested = p.parent.nested
	}
	if p.vcpus != 0 {
		index.vcpus = p.vcpus
	}
	if p.setNested {
		index.nested = p.nested
	}
	// A checkpoint with no state of its own keeps the parent's: only a capture
	// pauses the guest for VMM state, and the VM stays restorable from the last
	// one between captures. A cold boot and a checkpoint of the disks alone
	// are the exceptions and say so.
	if p.dropState {
		index.state = location{}
	}
	writer := &partWriter{store: p.store, ref: p.ref, cancel: cancel, keep: p.keep, geometry: p.geometry}
	if err := p.trim(ctx, index); err != nil {
		return nil, writer.abandon(err)
	}
	if p.hasState {
		if err := p.writeState(ctx, writer, index); err != nil {
			return nil, writer.abandon(err)
		}
	}
	if err := p.writeEdits(ctx, writer, index, source); err != nil {
		return nil, writer.abandon(err)
	}
	if err := p.compact(ctx, writer, index); err != nil {
		return nil, writer.abandon(err)
	}
	// The parts are finished and durable before anything of the index object
	// is written: what the parts cost is settled here, and a segment says
	// where the pages of its range are, so it cannot be encoded before they are.
	if err := writer.finish(ctx); err != nil {
		return nil, writer.abandon(err)
	}
	index.checkpoints[p.ref] = checkpointCost{parts: writer.next, bytes: writer.bytes}
	object := newIndexObject(p.store)
	if err := p.writeSegments(ctx, object, index); err != nil {
		return nil, writer.abandon(err)
	}
	index.retainReferencedCheckpoints()
	data, err := object.seal(ctx, index)
	if err != nil {
		return nil, writer.abandon(err)
	}
	if err := p.store.putIndexObject(ctx, p.ref, data); err != nil {
		return nil, writer.abandon(err)
	}
	p.keepSegments(ctx, index, data, &writer.pace)
	for _, written := range p.written {
		index.keep(ctx, written.volume, written.number, written.table)
	}
	return index, nil
}

// keepSegments hands the segments this publication wrote into its index object
// to the pull that keeps them, and to the cluster, once the object is durable.
func (p *Publication) keepSegments(ctx context.Context, index *Index, object []byte, pace *fillPace) {
	if p.keep == nil && !p.store.cache.fills() {
		return
	}
	var envelopes []envelope
	for _, name := range index.names {
		table := index.volumes[name]
		for _, number := range slices.Sorted(maps.Keys(p.dirty[name])) {
			entry, written := table.segments[number]
			if !written || entry.at.ref != p.ref {
				continue
			}
			envelopes = append(envelopes, envelope{key: segmentDiskKey(name, number, entry.at.ref),
				data: object[entry.at.offset:][:entry.at.length]})
		}
	}
	if p.keep != nil {
		p.keep.keep(ctx, envelopes)
	}
	p.store.cache.publish(ctx, pace, envelopes)
}

// writeState writes the VMM state as this checkpoint's first member, so its
// extent is the same for a retry of the same publication. The index names it
// once it is in a part, which is before writeEdits returns.
func (p *Publication) writeState(ctx context.Context, writer *partWriter, index *Index) error {
	writer.plan(1, uint64(len(p.state)))
	return writer.submit(ctx, "", 0, memberState, p.ref, p.state, func(at location) { index.state = at })
}

// segmentFor reports the working copy of one segment of the index being built,
// taken out of the table the root addresses the first time it is touched. The
// table is the parent's, shared through the store's cache, so the copy is the
// publication's own, and a segment the cache holds costs no fetch.
func (p *Publication) segmentFor(ctx context.Context, index *Index, volume string, number uint64) (*segment, error) {
	key := segmentKey{volume: volume, number: number}
	if held := p.working[key]; held != nil {
		return held, nil
	}
	table, release, err := index.table(ctx, volume, number)
	if err != nil {
		return nil, err
	}
	held := table.mutable()
	release()
	p.working[key] = held
	return held, nil
}

// markDirty records that a segment's page table has changed, so that this
// checkpoint writes it into its own index object. The caller passes the segment
// it changed, which is the copy every later step works from.
func (p *Publication) markDirty(volume string, number uint64, held *segment) {
	numbers := p.dirty[volume]
	if numbers == nil {
		numbers = make(map[uint64]*segment)
		p.dirty[volume] = numbers
	}
	numbers[number] = held
}

// trim drops the pages a smaller size cut off from the one segment that can
// straddle the volume's new end; the segments wholly past it were never
// inherited. A volume that did not shrink has nothing to drop.
func (p *Publication) trim(ctx context.Context, index *Index) error {
	if p.parent == nil {
		return nil
	}
	for _, name := range index.names {
		table := index.volumes[name]
		was := p.parent.volumes[name]
		if was == nil || was.size <= table.size || table.size == 0 {
			continue
		}
		geometry := table.geometry
		pages := geometry.PageCount(table.size)
		number := geometry.SegmentOf(pages - 1)
		if _, addressed := table.segments[number]; !addressed {
			continue
		}
		held, err := p.segmentFor(ctx, index, name, number)
		if err != nil {
			return err
		}
		cut := false
		for relative := range held.pages {
			if geometry.SegmentBase(number)+uint64(relative) >= pages {
				delete(held.pages, relative)
				cut = true
			}
		}
		if cut {
			p.markDirty(name, number, held)
		}
	}
	return nil
}

// writeSegments writes the page table of every segment this checkpoint changed
// into its index object, in ascending volume-name and segment-number order so a
// retry produces identical bytes, and addresses the root at what it wrote. A
// segment left with no pages is not written at all and loses its entry: an
// absent segment reads as zeroes, which is what the pages it held now do.
func (p *Publication) writeSegments(ctx context.Context, object *indexObject, index *Index) error {
	for _, name := range index.names {
		table := index.volumes[name]
		for _, number := range slices.Sorted(maps.Keys(p.dirty[name])) {
			held := p.dirty[name][number]
			if len(held.pages) == 0 {
				delete(table.segments, number)
				continue
			}
			data, err := encodeSegment(held)
			if err != nil {
				return err
			}
			at, err := object.add(ctx, p.ref, data)
			if err != nil {
				return err
			}
			// What the index keeps for its readers is what they would decode
			// from the object, decoded from the same bytes.
			written, err := parsePageTable(data)
			if err != nil {
				return err
			}
			p.written = append(p.written, writtenTable{volume: name, number: number, table: written})
			table.segments[number] = segmentEntry{at: at, reads: held.reads()}
		}
	}
	return nil
}

// writeEdits reads every changed page whole from the source and writes it into
// a part, in ascending volume-name and page-number order so a retry produces
// identical parts. A page that reads as all zeroes leaves the index instead: a
// hole costs no member and reads back as the zeroes it holds.
//
// The pages are read one at a time, in that order, on this goroutine, and
// encoded side by side by the writer, which holds a bounded number of them at
// once whatever the size of the dirty set. Each page's segment entry is
// written as the page lands in its part; when this returns, every page has.
func (p *Publication) writeEdits(ctx context.Context, writer *partWriter, index *Index, source Source) error {
	for _, name := range index.names {
		table := index.volumes[name]
		geometry := table.geometry
		numbers := slices.Sorted(maps.Keys(p.edits[name]))
		writer.plan(len(numbers), geometry.PageSize)
		for _, number := range numbers {
			_, span := geometry.PageSpan(table.size, number)
			if span == 0 {
				return ErrInvalidRange
			}
			if source == nil {
				return ErrInvalidConfig
			}
			data, err := writer.room(ctx, int(span))
			if err != nil {
				return err
			}
			if err := source.ReadPage(ctx, name, number, data); err != nil {
				return err
			}
			segment := geometry.SegmentOf(number)
			held, err := p.segmentFor(ctx, index, name, segment)
			if err != nil {
				return err
			}
			relative := geometry.OffsetIn(number)
			if isZero(data) {
				if _, found := held.pages[relative]; !found {
					continue
				}
				// The page simply leaves the segment this checkpoint rewrites,
				// which is the whole record that it is gone: an absent page reads
				// as the zeroes the guest wrote.
				delete(held.pages, relative)
				p.markDirty(name, segment, held)
				continue
			}
			if err := writer.submitRead(ctx, name, number, data, func(at location) {
				held.pages[relative] = at
				p.markDirty(name, segment, held)
			}); err != nil {
				return err
			}
		}
	}
	return writer.drain(ctx)
}

// inherit copies the parent's segment entries for a volume, dropping the
// segments a smaller size has cut off entirely. The one segment that straddles
// the new end is trimmed instead; see trim.
func (p *Publication) inherit(volume string, size uint64) map[uint64]segmentEntry {
	segments := make(map[uint64]segmentEntry)
	if p.parent == nil {
		return segments
	}
	table := p.parent.volumes[volume]
	if table == nil {
		return segments
	}
	count := table.geometry.SegmentCount(size)
	for number, entry := range table.segments {
		if number < count {
			segments[number] = entry
		}
	}
	return segments
}

// isZero reports whether every byte of data is zero: the first is, and each
// is the one before it. Comparing the page with itself shifted by a byte runs
// at the speed of a memory compare rather than of a loop over the bytes, which
// matters on the goroutine that reads every page a publication writes.
func isZero(data []byte) bool {
	return len(data) == 0 || data[0] == 0 && bytes.Equal(data[1:], data[:len(data)-1])
}

// indexObject accumulates one checkpoint's index object: a fixed record, the
// segments the checkpoint changed, the root, and the record again. It is built
// whole in memory, which is what maximumIndexSize bounds, and written in one
// PUT.
type indexObject struct {
	store *Store
	data  []byte
}

func newIndexObject(store *Store) *indexObject {
	return &indexObject{store: store, data: make([]byte, indexRecordSize)}
}

// add puts one encoded segment into the object and reports where it landed,
// which is the address the root carries beside the checkpoint that is writing
// it.
func (o *indexObject) add(ctx context.Context, ref control.Ref, data []byte) (segmentAddress, error) {
	if len(data) > maximumSegmentSize {
		return segmentAddress{}, ErrInvalidRange
	}
	offset := uint64(len(o.data))
	grown, err := o.store.codecs.AppendEncode(ctx, o.data, data)
	if err != nil {
		return segmentAddress{}, err
	}
	o.data = grown
	return segmentAddress{ref: ref, offset: offset, length: uint64(len(o.data)) - offset}, nil
}

// seal appends the root, stamps both records with where it landed, and returns
// the object's bytes. The root is encoded after every segment address is
// settled, because the root is what carries them.
func (o *indexObject) seal(ctx context.Context, index *Index) ([]byte, error) {
	root, err := index.encode()
	if err != nil {
		return nil, err
	}
	if len(root) > o.store.maxRootBytes {
		return nil, ErrInvalidRange
	}
	offset := uint64(len(o.data))
	grown, err := o.store.codecs.AppendEncode(ctx, o.data, root)
	if err != nil {
		return nil, err
	}
	length := uint64(len(grown)) - offset
	o.data = append(grown, make([]byte, indexRecordSize)...)
	if len(o.data) > maximumIndexSize {
		return nil, ErrInvalidRange
	}
	putIndexRecord(o.data, offset, length)
	putIndexRecord(o.data[len(o.data)-indexRecordSize:], offset, length)
	return o.data, nil
}

// protectedCheckpoints expands the sequences Protect named into the
// checkpoints compaction must leave alone: each protected checkpoint and every
// one its index names. The store memoises the expansion, because an index is
// immutable.
func (p *Publication) protectedCheckpoints(ctx context.Context) (map[control.Ref]bool, error) {
	protected := make(map[control.Ref]bool, len(p.protected))
	for ref := range p.protected {
		refs, err := p.store.protectedBy(ctx, ref)
		if err != nil {
			// A protected checkpoint whose index cannot be read protects
			// everything a compaction might otherwise rewrite.
			return nil, errors.Join(err, errors.New("checkpoint: protected checkpoint unreadable"))
		}
		for _, spared := range refs {
			protected[spared] = true
		}
	}
	return protected, nil
}

// move rewrites some of one segment's pages into this checkpoint's parts, in
// the order given. Their members are fetched the way a read fetches a run:
// through the page cache, and in ranged extents of the parts they lie in for
// the ones it does not hold, so a rescue of adjacent pages costs a request per
// extent rather than one per page.
//
// The pages are lent to the writer, which encodes them side by side, and are
// in parts, or no longer read, before they are given back.
func (p *Publication) move(ctx context.Context, writer *partWriter, volume string, geometry Geometry,
	number uint64, held *segment, relatives []uint32) error {
	run := make([]pageRead, len(relatives))
	for at, relative := range relatives {
		run[at] = pageRead{number: geometry.SegmentBase(number) + uint64(relative), at: held.pages[relative]}
	}
	data, release, err := p.store.loadPages(ctx, geometry, volume, run)
	if err != nil {
		return err
	}
	defer release()
	writer.plan(len(run), geometry.PageSize)
	for at, page := range run {
		// The bytes move; the page does not. Carrying the origin forward is
		// what keeps a fork of the older view and this index reporting one
		// identity for one page.
		relative := relatives[at]
		if err := writer.submit(ctx, volume, page.number, memberPage, page.at.origin, data[at],
			func(moved location) { held.pages[relative] = moved }); err != nil {
			writer.halt()
			return err
		}
	}
	if err := writer.drain(ctx); err != nil {
		writer.halt()
		return err
	}
	p.markDirty(volume, number, held)
	return nil
}

// liveBytes reports, per checkpoint, the encoded member bytes this index reads
// from its parts: every page the segments' tables name, and the state. It opens
// nothing, and it counts nothing of the index object — a segment is never a
// member of a part. A segment this checkpoint has not changed answers for
// itself out of the root, which records what its pages read from each
// checkpoint, and one it has changed is in hand already.
func (p *Publication) liveBytes(index *Index) map[control.Ref]uint64 {
	live := make(map[control.Ref]uint64, len(index.checkpoints))
	for name, table := range index.volumes {
		for number, entry := range table.segments {
			if held := p.dirty[name][number]; held != nil {
				for _, at := range held.pages {
					live[at.ref] += at.length
				}
				continue
			}
			for _, use := range entry.reads {
				live[use.ref] += use.bytes
			}
		}
	}
	if !index.state.isZero() {
		live[index.state.ref] += index.state.length
	}
	return live
}

// compact rewrites the parts that have become mostly dead into this
// checkpoint's, so a cold page cannot keep a checkpoint alive for ever. It runs
// after the guest has resumed, reads through the page cache where it can, and
// stops at compactionBudget live bytes; the checkpoints it empties leave the
// index and reclamation deletes them.
//
// It works over the parts alone. A segment is never moved and never
// counted as part liveness: it lives in the index object of the checkpoint that
// wrote it, for as long as any root addresses it, and a compaction that moved
// the pages of a segment writes that segment again anyway, because its entries
// have changed.
func (p *Publication) compact(ctx context.Context, writer *partWriter, index *Index) error {
	if p.parent == nil {
		return nil
	}
	protected, err := p.protectedCheckpoints(ctx)
	if err != nil {
		return err
	}
	// Another VM's parts belong to that VM: a fork rewrites its parent's pages
	// into its own parts only when it writes them, never to tidy the parent up.
	// A page's bytes are billed to the VM whose key holds them, so rewriting a
	// parent's page into a child would also bill the child for it.
	own := func(ref control.Ref) bool {
		return ref.VM == p.ref.VM || sim.Bug(ctx, "checkpoint-compact-another-vm")
	}
	eligible := func(ref control.Ref, entry checkpointCost) bool {
		return own(ref) && ref != p.ref && !protected[ref] &&
			entry.bytes != 0 && entry.emptied == 0
	}
	any := false
	for ref, entry := range index.checkpoints {
		if eligible(ref, entry) {
			any = true
			break
		}
	}
	// Nothing this checkpoint could rewrite is nothing to measure: the segments
	// stay unopened, which is what a VM with one checkpoint of its own costs.
	if !any {
		return nil
	}
	live := p.liveBytes(index)
	type candidate struct {
		ref      control.Ref
		live     uint64
		fraction float64
	}
	var candidates []candidate
	for ref, entry := range index.checkpoints {
		if !eligible(ref, entry) {
			continue
		}
		held := live[ref]
		if held == 0 || held*2 >= entry.bytes {
			// A checkpoint whose parts nothing reads any more needs no
			// compaction: this one has already stopped reading them, so it
			// simply leaves the index.
			continue
		}
		candidates = append(candidates, candidate{ref: ref, live: held,
			fraction: float64(held) / float64(entry.bytes)})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].fraction != candidates[j].fraction {
			return candidates[i].fraction < candidates[j].fraction
		}
		return compareRefs(candidates[i].ref, candidates[j].ref) < 0
	})
	rewriting := make(map[control.Ref]bool)
	budget := uint64(compactionBudget)
	for _, item := range candidates {
		if item.live > budget {
			continue
		}
		budget -= item.live
		rewriting[item.ref] = true
	}
	if len(rewriting) == 0 {
		return nil
	}
	// The pages of the checkpoints being rewritten move into this one's parts
	// while keeping the page identity they were first published under, and
	// the emptied checkpoints become reclaimable one checkpoint later. A
	// campaign that never compacts never tests either.
	sim.Probe(ctx, ProbeCompactionRewrite)
	// Nothing in this index will read from their parts once the rewrite is done,
	// but a reader holding the view this checkpoint replaces still does. The
	// entry stays, marked emptied here, so reclamation leaves them for one
	// checkpoint; the next index drops them and the next sweep deletes them.
	for ref := range rewriting {
		entry := index.checkpoints[ref]
		entry.emptied = p.ref.Sequence
		index.checkpoints[ref] = entry
	}
	// Only the segments whose pages read from a checkpoint being rewritten are
	// opened, and the root says which those are. A segment already changed is in
	// hand.
	for _, name := range index.names {
		table := index.volumes[name]
		for _, number := range slices.Sorted(maps.Keys(table.segments)) {
			entry := table.segments[number]
			touched := p.dirty[name][number] != nil
			for _, use := range entry.reads {
				touched = touched || rewriting[use.ref]
			}
			if !touched {
				continue
			}
			held, err := p.segmentFor(ctx, index, name, number)
			if err != nil {
				return err
			}
			var moving []uint32
			for _, relative := range slices.Sorted(maps.Keys(held.pages)) {
				if rewriting[held.pages[relative].ref] {
					moving = append(moving, relative)
				}
			}
			// The pages are read a run's worth at a time, which is what bounds the
			// decoded bytes held at once, and written in page order, which is
			// what makes a retry write the same parts.
			batch := max(1, int(maximumRunBytes/table.geometry.PageSize))
			for len(moving) > 0 {
				count := min(batch, len(moving))
				if err := p.move(ctx, writer, name, table.geometry, number, held, moving[:count]); err != nil {
					return err
				}
				moving = moving[count:]
			}
		}
	}
	if !index.state.isZero() && rewriting[index.state.ref] {
		data, err := p.store.readMember(ctx, index.state, maximumStateSize)
		if err != nil {
			return err
		}
		// The state member carries its origin the way a page does, so the part
		// says the member was moved rather than written here.
		writer.plan(1, uint64(len(data)))
		if err := writer.submit(ctx, "", 0, memberState, index.state.origin, data,
			func(moved location) { index.state = moved }); err != nil {
			return err
		}
		return writer.drain(ctx)
	}
	return nil
}
