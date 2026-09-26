package checkpoint

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/semistrict/sproutfs/checkpoint/internal/part"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Source supplies the whole contents of a page a publication is republishing.
// ReadPage must fill dst with the page's contents as of the checkpoint; dst is
// the page's length, which is the volume's own page size except for a tail page
// shorter than one. It must fill all of it: the publication writes one page at
// a time into one buffer it reuses, so bytes a source leaves untouched are the
// previous page's.
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
	sizes     map[string]uint64
	geometry  map[string]Geometry
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
	err   error
}

// Begin starts a checkpoint that inherits parent, which may be nil for a VM
// with no history, and publishes under ref. A fork passes the source VM's index
// as parent and its own ref.
func (s *Store) Begin(parent *Index, ref control.Ref) *Publication {
	p := &Publication{store: s, parent: parent, ref: ref,
		sizes: make(map[string]uint64), geometry: make(map[string]Geometry),
		edits:     make(map[string]map[uint64]bool),
		protected: make(map[control.Ref]bool), dirty: make(map[string]map[uint64]*segment)}
	if parent != nil {
		for _, name := range parent.names {
			p.sizes[name] = parent.volumes[name].size
			p.geometry[name] = parent.volumes[name].geometry
		}
	}
	if !control.ValidID(ref.VM) || ref.Sequence == 0 {
		p.err = ErrInvalidConfig
	}
	return p
}

// Protect names the sequences of this VM a fork pinned, which compaction must
// leave alone: their objects are the fork's as much as this VM's. A pinned
// checkpoint protects every checkpoint its index names, not only
// itself: the fork reads its whole view through them, so rewriting one copies
// bytes reclamation can never free.
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

// Dirty records that a page changed and must be republished whole. Commit reads
// it from the Source; a page that reads as all zeroes then leaves the index
// instead of being written.
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

// maximumVCPUs bounds the processor count a checkpoint records, which is the
// most a Firecracker guest can have.
const maximumVCPUs = 32

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
		index.volumes[name] = &volumeTable{size: size, geometry: geometry,
			segments: p.inherit(name, size)}
		index.names = append(index.names, name)
	}
	slices.Sort(index.names)
	for name := range p.edits {
		if index.volumes[name] == nil {
			return nil, ErrUnknownVolume
		}
	}
	if p.parent != nil {
		for ref, entry := range p.parent.checkpoints {
			index.checkpoints[ref] = entry
		}
		index.state = p.parent.state
		index.vcpus = p.parent.vcpus
	}
	if p.vcpus != 0 {
		index.vcpus = p.vcpus
	}
	// A checkpoint with no state of its own keeps the parent's: only a capture
	// pauses the guest for VMM state, and the VM stays restorable from the last
	// one between captures. A cold boot and a checkpoint of the disks alone
	// are the exceptions and say so.
	if p.dropState {
		index.state = location{}
	}
	writer := &partWriter{store: p.store, ref: p.ref, cancel: cancel}
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
	return index, nil
}

// writeState writes the VMM state as this checkpoint's first member, so its
// extent is the same for a retry of the same publication.
func (p *Publication) writeState(ctx context.Context, writer *partWriter, index *Index) error {
	at, err := writer.add(ctx, "", 0, memberState, p.ref, p.state)
	if err != nil {
		return err
	}
	index.state = at
	return nil
}

// segmentFor reports the working copy of one segment of the index being built,
// decoding the one the root addresses the first time it is touched. The copy
// belongs to that index, so a reader of the published one finds the table this
// publication left rather than fetching it again.
func (p *Publication) segmentFor(ctx context.Context, index *Index, volume string, number uint64) (*segment, error) {
	return index.segmentAt(ctx, volume, number)
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
			at, err := object.add(ctx, p.ref, held)
			if err != nil {
				return err
			}
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
// One page is read at a time, into one buffer a volume's pages share: a page is
// encoded into the part it belongs to before the next is read, so the dirty
// set's size costs nothing here. The buffer is one page of the volume being
// written, because two volumes of one VM may have different page sizes.
func (p *Publication) writeEdits(ctx context.Context, writer *partWriter, index *Index, source Source) error {
	for _, name := range index.names {
		table := index.volumes[name]
		geometry := table.geometry
		var buffer []byte
		for _, number := range slices.Sorted(maps.Keys(p.edits[name])) {
			_, span := geometry.PageSpan(table.size, number)
			if span == 0 {
				return ErrInvalidRange
			}
			if source == nil {
				return ErrInvalidConfig
			}
			if buffer == nil {
				buffer = make([]byte, geometry.PageSize)
			}
			data := buffer[:span]
			if err := source.ReadPage(ctx, name, number, data); err != nil {
				return err
			}
			held, err := p.segmentFor(ctx, index, name, geometry.SegmentOf(number))
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
				p.markDirty(name, geometry.SegmentOf(number), held)
				continue
			}
			at, err := writer.add(ctx, name, number, memberPage, p.ref, data)
			if err != nil {
				return err
			}
			held.pages[relative] = at
			p.markDirty(name, geometry.SegmentOf(number), held)
		}
	}
	return nil
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

// isZero reports whether every byte of data is zero.
func isZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
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

// add encodes one segment into the object and reports where it landed, which is
// the address the root carries beside the checkpoint that is writing it.
func (o *indexObject) add(ctx context.Context, ref control.Ref, held *segment) (segmentAddress, error) {
	data, err := encodeSegment(held)
	if err != nil {
		return segmentAddress{}, err
	}
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

// partWriter fills parts and uploads each one as it is sealed, so a large
// checkpoint costs a few PUTs and holds a bounded number of parts in memory.
type partWriter struct {
	store  *Store
	ref    control.Ref
	cancel context.CancelFunc
	part   *part.Builder
	// admitted is whether this writer holds a slot of the store's host-wide
	// builder budget, which it takes before it writes anything.
	admitted bool
	next     uint32
	bytes    uint64
	wait     sync.WaitGroup
	once     sync.Once
	failure  error
}

// admit takes this writer's slot of the store's builder budget, once. A
// publication that writes nothing never takes one.
func (w *partWriter) admit(ctx context.Context) error {
	if w.admitted {
		return nil
	}
	if err := w.store.acquireBuilder(ctx); err != nil {
		return err
	}
	w.admitted = true
	return nil
}

// discharge gives the builder slot back, once, when the writer is done with it.
func (w *partWriter) discharge() {
	if w.admitted {
		w.admitted = false
		w.store.releaseBuilder()
	}
}

// memberKind says what one member holds: a page of a volume's contents or the
// VMM state. Those are the two kinds a part holds.
type memberKind int

const (
	memberPage memberKind = iota
	memberState
)

// add encodes one member into the current part, sealing and uploading that part
// when it fills. The caller's bytes are read here and not kept, so it may reuse
// the buffer it read them into for the next page. origin is the checkpoint the
// page was first published under: this one for a page the publication wrote,
// and the page's existing origin for one compaction moved.
func (w *partWriter) add(ctx context.Context, volume string, page uint64, kind memberKind, origin control.Ref, data []byte) (location, error) {
	member := part.Member{Volume: volume, Page: page, State: kind == memberState}
	if origin != w.ref {
		member.OriginVM, member.OriginSequence = origin.VM, origin.Sequence
	}
	builder, err := w.builderFor(ctx, member)
	if err != nil {
		return location{}, err
	}
	offset, length, err := builder.Add(ctx, member, data)
	if err != nil {
		return location{}, err
	}
	at := location{ref: w.ref, origin: origin, part: w.next, offset: offset, length: length}
	w.bytes += at.length
	return at, nil
}

// partBytes is the encoded member size a part fills to before it is sealed and
// uploaded. It is the store's configured size, except where a campaign has
// buggified it down to a single page: a deployment whose pages are large
// relative to the part size writes a part per page, and a multi-part checkpoint
// is the shape a one-part checkpoint never reaches — a member table per part, a
// part count carried by the last one, and a root that must name which part each
// page is in.
func (w *partWriter) partBytes(ctx context.Context) int {
	if sim.Buggify(ctx, "checkpoint/one-page-parts", 1) {
		return 1
	}
	return w.store.partBytes
}

// builderFor returns the part builder member goes into, sealing the part in
// hand first when member no longer fits it — in body bytes or in table. A full
// part is sealed when the next member arrives rather than as soon as it fills,
// so the part this publication holds last is always the one finish closes: that
// is the part carrying the checkpoint's part count.
func (w *partWriter) builderFor(ctx context.Context, member part.Member) (*part.Builder, error) {
	if err := w.admit(ctx); err != nil {
		return nil, err
	}
	if w.part != nil && w.part.Full(member, w.partBytes(ctx), maximumTableSize) {
		if err := w.flush(ctx); err != nil {
			return nil, err
		}
	}
	if w.part == nil {
		w.part = part.NewBuilder(w.store.codecs)
	}
	return w.part, nil
}

// flush seals the part in hand as one the checkpoint goes on past and starts its
// upload, which runs under one slot of the store's shared budget so parts in
// flight are bounded. Only the last part carries the part count, and finish
// writes that one.
func (w *partWriter) flush(ctx context.Context) error {
	if w.part == nil {
		return nil
	}
	data, err := w.part.Seal(0)
	if err != nil {
		return err
	}
	if len(data) > maximumPartSize {
		return ErrInvalidRange
	}
	key, err := w.store.partKey(w.ref, w.next)
	if err != nil {
		return err
	}
	w.part, w.next = nil, w.next+1
	if err := w.store.acquire(ctx); err != nil {
		return err
	}
	w.wait.Add(1)
	go func() {
		defer w.wait.Done()
		defer w.store.release()
		if err := w.put(ctx, key, data); err != nil {
			w.record(err)
		}
	}()
	return nil
}

// put writes one part create-if-absent. A part a retry of this publication
// finds already there is its own, byte for byte, because a publication writes
// its members in one order and encodes them one way; a part holding anything
// else is another publication under this reference, which is a conflict.
func (w *partWriter) put(ctx context.Context, key platform.ObjectKey, data []byte) error {
	return w.store.putObject(ctx, key, data, digestOf(data), func(existing []byte) error {
		if !equalParts(existing, data) {
			return ErrConflict
		}
		return nil
	})
}

// finish seals and uploads the part in hand, which is the one carrying the
// checkpoint's part count, and waits for every earlier upload. When it returns
// every part is durable, which is what the index object may then
// name. An upload that failed is what the caller is told about, not the
// cancellation it caused in whatever was still running.
func (w *partWriter) finish(ctx context.Context) error {
	defer w.discharge()
	if w.part != nil {
		sealed, err := w.part.Seal(w.next + 1)
		if err != nil {
			return err
		}
		if len(sealed) > maximumPartSize {
			return ErrInvalidRange
		}
		key, err := w.store.partKey(w.ref, w.next)
		if err != nil {
			return err
		}
		w.part, w.next = nil, w.next+1
		if err := w.store.acquire(ctx); err != nil {
			return err
		}
		err = w.put(ctx, key, sealed)
		w.store.release()
		if err != nil {
			return err
		}
	}
	w.wait.Wait()
	return w.failure
}

// abandon stops the uploads a failed publication started and reports why it
// failed: an upload's own error where there was one, and otherwise what the
// caller ran into. Everything the uploads wrote is unreferenced.
func (w *partWriter) abandon(cause error) error {
	w.cancel()
	w.wait.Wait()
	w.discharge()
	if w.failure != nil {
		return w.failure
	}
	return cause
}

func (w *partWriter) record(err error) { w.once.Do(func() { w.failure = err; w.cancel() }) }

// equalParts compares a part already in the store with the one this publication
// built. Parts are raw bytes rather than an envelope, so equality is exact.
func equalParts(existing, data []byte) bool { return string(existing) == string(data) }

// protectedCheckpoints expands the pinned sequences Protect named into the
// checkpoints compaction must leave alone: each pinned checkpoint and every one
// its index names. The store memoises the expansion, because an index is
// immutable.
func (p *Publication) protectedCheckpoints(ctx context.Context) (map[control.Ref]bool, error) {
	protected := make(map[control.Ref]bool, len(p.protected))
	for ref := range p.protected {
		refs, err := p.store.protectedBy(ctx, ref)
		if err != nil {
			// A pinned checkpoint whose index cannot be read protects
			// everything a compaction might otherwise rewrite.
			return nil, errors.Join(err, errors.New("checkpoint: pinned checkpoint unreadable"))
		}
		for _, spared := range refs {
			protected[spared] = true
		}
	}
	return protected, nil
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
			for _, relative := range slices.Sorted(maps.Keys(held.pages)) {
				at := held.pages[relative]
				if !rewriting[at.ref] {
					continue
				}
				page := table.geometry.SegmentBase(number) + uint64(relative)
				data, release, err := p.store.loadPage(ctx, table.geometry, name, page, at)
				if err != nil {
					return err
				}
				// The bytes move; the page does not. Carrying the origin forward
				// is what keeps a fork of the older view and this index reporting
				// one identity for one page.
				moved, err := writer.add(ctx, name, page, memberPage, at.origin, data)
				release()
				if err != nil {
					return err
				}
				held.pages[relative] = moved
				p.markDirty(name, number, held)
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
		moved, err := writer.add(ctx, "", 0, memberState, index.state.origin, data)
		if err != nil {
			return err
		}
		index.state = moved
	}
	return nil
}
