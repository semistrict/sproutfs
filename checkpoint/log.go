package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	checkpointv1 "github.com/semistrict/sproutfs/checkpoint/internal/gen/sproutfs/checkpoint/v1"
	"github.com/semistrict/sproutfs/checkpoint/internal/part"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
)

// The log layout is an experiment (TASK-66) after the backend of Hajkazemi et
// al., "Beating the I/O Bottleneck: A Case for Log-Structured Virtual Disks"
// (EuroSys '22). It stores the same parts as the index layout and differs only
// in where the page table lives.
//
// The index layout writes the segments of page table a checkpoint changed, and
// a root, into an index object whose create-if-absent PUT is the commit. An
// open reads the root, and a segment is fetched the first time a read needs it.
//
// The log layout writes no page table. A part's table already names every page
// the part holds, so a checkpoint adds only a log record — its root without
// segments, its parent, the pages it zeroed and the map it replays from — to
// the table of its last part, whose PUT is then the commit. A VM's first
// checkpoint, a fork's first, and every MapEvery checkpoints after the last
// one also write a map object: an index object holding every segment of the
// page table and the root. An open lists the VM's objects, reads the last map
// at or before the checkpoint and the tables of every part after it, and
// replays the records from the map to the checkpoint along their parent
// chain. So a log index holds its whole page table decoded, and no read
// fetches a segment.
//
// Pull, CheckIndex and Keep are the index layout's alone.

// LogLayout configures the log layout.
type LogLayout struct {
	// MapEvery is how many checkpoints of a VM may follow its last map object:
	// the checkpoint MapEvery after it writes the next. It bounds what an open
	// replays. Zero takes defaultMapEvery.
	MapEvery int
}

const (
	defaultMapEvery = 16
	// logTail is how much of the end of a part an open reads for its table. A
	// table this does not hold costs one more read.
	logTail = 64 << 10
	// replayConcurrency bounds how many part tables an open reads at once.
	replayConcurrency = 16
	// logListPage is the listing page an open asks for, which is what S3
	// returns at most, so an open costs the listing requests a real store would
	// charge.
	logListPage = 1000
)

// mapKey names the map object a log checkpoint writes.
func (s *Store) mapKey(ref control.Ref) (platform.ObjectKey, error) {
	if !control.ValidID(ref.VM) {
		return platform.ObjectKey{}, ErrInvalidConfig
	}
	return platform.NewObjectKey(s.checkpointPrefix(ref) + "map")
}

// mapSequence is the map object the checkpoint being published replays from:
// its own when it writes one, and its parent's otherwise. A VM's first
// checkpoint and a fork's first write one, because replay never crosses to
// another VM's objects, and so does the one MapEvery after the parent's map.
func (p *Publication) mapSequence() uint64 {
	if p.parent == nil || p.parent.ref.VM != p.ref.VM ||
		p.ref.Sequence >= p.parent.mapSequence+uint64(p.store.log.MapEvery) {
		return p.ref.Sequence
	}
	return p.parent.mapSequence
}

// inheritLoaded shares the parent's decoded segments that this index still
// addresses, in either layout. They remain the parent's: a publication changes
// a copy (segmentFor).
func (i *Index) inheritLoaded(parent *Index) {
	if parent == nil {
		return
	}
	parent.mu.Lock()
	defer parent.mu.Unlock()
	for key, held := range parent.loaded {
		if table := i.volumes[key.volume]; table != nil {
			if _, addressed := table.segments[key.number]; addressed {
				i.loaded[key] = held
			}
		}
	}
}

// replayed readdresses every segment of a log index at the index itself: no
// object holds a segment of a log checkpoint but a map object, so an entry says
// only that replay rebuilt it, and names no other checkpoint.
func (i *Index) replayed() {
	for _, table := range i.volumes {
		for number, entry := range table.segments {
			entry.at = segmentAddress{ref: i.ref}
			table.segments[number] = entry
		}
	}
}

// commitLog finishes a publication of the log layout once its pages are in
// parts: the segments it changed are settled, the last part is sealed with the
// log record, and a map object is written when one is due.
func (p *Publication) commitLog(ctx context.Context, writer *partWriter, index *Index) (*Index, error) {
	for _, name := range index.names {
		table := index.volumes[name]
		for number, held := range p.dirty[name] {
			if len(held.pages) == 0 {
				delete(table.segments, number)
				index.mu.Lock()
				delete(index.loaded, segmentKey{volume: name, number: number})
				index.mu.Unlock()
				continue
			}
			table.segments[number] = segmentEntry{reads: held.reads()}
		}
	}
	index.replayed()
	err := writer.finish(ctx, func(parts uint32, bytes uint64) *checkpointv1.LogRecord {
		index.checkpoints[p.ref] = checkpointCost{parts: parts, bytes: bytes}
		index.retainReferencedCheckpoints()
		return p.logRecord(index)
	})
	if err != nil {
		return nil, writer.abandon(err)
	}
	if index.mapSequence == p.ref.Sequence {
		if err := p.store.putMap(ctx, index); err != nil {
			return nil, err
		}
	}
	return index, nil
}

// logRecord is what the checkpoint being published says about itself beside
// the pages its parts name. It is deterministic, so a retry seals the same part.
func (p *Publication) logRecord(index *Index) *checkpointv1.LogRecord {
	record := checkpointv1.LogRecord_builder{Root: index.rootMessage(false),
		Map: proto.Uint64(index.mapSequence)}
	if p.parent != nil {
		record.Parent = refMessage(p.parent.ref)
	}
	for _, name := range slices.Sorted(maps.Keys(p.removed)) {
		record.Removed = append(record.Removed, checkpointv1.RemovedPages_builder{
			Volume: proto.String(name), Pages: p.removed[name]}.Build())
	}
	return record.Build()
}

// putMap writes a log checkpoint's map object: an index object holding every
// segment of its page table and its root. An open of this checkpoint, or of any
// later one up to the next map, starts replaying from it. It is written after
// every part is durable, so when it lands the checkpoint is complete.
func (s *Store) putMap(ctx context.Context, index *Index) error {
	object := newIndexObject(s)
	for _, name := range index.names {
		table := index.volumes[name]
		for _, number := range slices.Sorted(maps.Keys(table.segments)) {
			held := index.loaded[segmentKey{volume: name, number: number}]
			if held == nil {
				return ErrCorrupt
			}
			at, err := object.add(ctx, index.ref, held)
			if err != nil {
				return err
			}
			entry := table.segments[number]
			entry.at = at
			table.segments[number] = entry
		}
	}
	data, err := object.seal(ctx, index)
	if err != nil {
		return err
	}
	key, err := s.mapKey(index.ref)
	if err != nil {
		return err
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	return s.putObject(ctx, key, data, digestOf(data), func(existing []byte) error {
		if !equalParts(existing, data) {
			return ErrConflict
		}
		return nil
	})
}

// readMap reads a map object whole and decodes every segment it holds, which
// is the log index of the checkpoint that wrote it.
func (s *Store) readMap(ctx context.Context, ref control.Ref) (*Index, error) {
	key, err := s.mapKey(ref)
	if err != nil {
		return nil, err
	}
	data, _, err := platform.ReadObject(ctx, s.objects, key, 2*indexRecordSize, maximumIndexSize, ErrCorrupt)
	if err != nil {
		return nil, err
	}
	offset, length, err := decodeIndexRecord(data[len(data)-indexRecordSize:], uint64(len(data)))
	if err != nil {
		return nil, err
	}
	root, err := s.codecs.Decode(ctx, data[offset:offset+length], maximumRootSize)
	if err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	index, err := decodeRoot(s, ref, root)
	if err != nil {
		return nil, err
	}
	end := uint64(len(data)) - indexRecordSize
	for _, name := range index.names {
		for number, entry := range index.volumes[name].segments {
			if entry.at.ref != ref || entry.at.offset > end || entry.at.length > end-entry.at.offset {
				return nil, ErrCorrupt
			}
			encoded, err := s.codecs.Decode(ctx, data[entry.at.offset:][:entry.at.length], maximumSegmentSize)
			if err != nil {
				return nil, errors.Join(ErrCorrupt, err)
			}
			held, err := index.decodeSegment(name, encoded)
			if err != nil {
				return nil, err
			}
			index.loaded[segmentKey{volume: name, number: number}] = held
		}
	}
	index.mapSequence = ref.Sequence
	return index, nil
}

// logObjects is what a listing shows of one log checkpoint: the keys of its
// parts, by number, and of its map object if it wrote one.
type logObjects struct {
	parts     []platform.ObjectKey
	mapObject platform.ObjectKey
}

// listLog lists one VM's checkpoint objects by sequence, a page of logListPage
// keys at a time. A part numbered past one the listing did not show is left
// for replay to refuse: it is a checkpoint missing a part.
func (s *Store) listLog(ctx context.Context, vm string) (map[uint64]*logObjects, error) {
	root := s.vmPrefix(vm) + "ckpt/"
	prefix, err := platform.NewObjectPrefix(root)
	if err != nil {
		return nil, err
	}
	listing := make(map[uint64]*logObjects)
	token := ""
	for {
		page, err := s.objects.List(ctx, platform.ListRequest{Prefix: prefix, ContinuationToken: token, Limit: logListPage})
		if err != nil {
			return nil, err
		}
		for _, object := range page.Objects {
			fields := strings.Split(strings.TrimPrefix(object.Key.String(), root), "/")
			sequence, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil || sequence == 0 {
				continue
			}
			held := listing[sequence]
			if held == nil {
				held = &logObjects{}
				listing[sequence] = held
			}
			switch {
			case len(fields) == 2 && fields[1] == "map":
				held.mapObject = object.Key
			case len(fields) == 3 && fields[1] == "part":
				number, err := strconv.ParseUint(fields[2], 10, 32)
				if err != nil {
					continue
				}
				if int(number) >= len(held.parts) {
					held.parts = append(held.parts, make([]platform.ObjectKey, int(number)+1-len(held.parts))...)
				}
				held.parts[number] = object.Key
			}
		}
		if page.NextContinuationToken == "" {
			return listing, nil
		}
		token = page.NextContinuationToken
	}
}

// logTable is what one part of a log checkpoint says about itself: its
// members, the log record if it is the last part, and the part count its
// trailer carries, which is set in the last part alone.
type logTable struct {
	members []part.Member
	record  *checkpointv1.LogRecord
	parts   uint32
}

// readLogTable reads one part's table: the end of the part, and the rest of
// the table in a second read if the table is longer than that.
func (s *Store) readLogTable(ctx context.Context, key platform.ObjectKey) (logTable, error) {
	tail, size, err := s.readSuffix(ctx, key, logTail)
	if err != nil {
		return logTable{}, err
	}
	if size < part.TrailerSize || size > maximumPartSize {
		return logTable{}, ErrCorrupt
	}
	trailer, err := part.DecodeTrailer(tail[uint64(len(tail))-part.TrailerSize:], size)
	if err != nil {
		return logTable{}, errors.Join(ErrCorrupt, err)
	}
	// A writer puts the table right before the trailer, which is what lets the
	// tail read locate it.
	if trailer.TableOffset+trailer.TableLength != size-part.TrailerSize {
		return logTable{}, ErrCorrupt
	}
	base := size - uint64(len(tail))
	var table []byte
	if trailer.TableOffset >= base {
		table = tail[trailer.TableOffset-base:][:trailer.TableLength]
	} else {
		head, err := s.readRange(ctx, key, trailer.TableOffset, base-trailer.TableOffset, maximumPartSize)
		if err != nil {
			return logTable{}, err
		}
		table = append(head, tail[:trailer.TableLength-(base-trailer.TableOffset)]...)
	}
	members, record, err := part.DecodeLogTable(table, trailer.TableOffset)
	if err != nil {
		return logTable{}, errors.Join(ErrCorrupt, err)
	}
	return logTable{members: members, record: record, parts: trailer.Parts}, nil
}

// openLog opens a checkpoint of the log layout. One listing finds the last map
// object at or before it and every part after that map; the map and those
// parts' tables are then read at once, and the records are replayed along the
// checkpoint's parent chain, which leaves out any checkpoint that was written
// but never selected. A map that turns out not to be the one the checkpoint's
// record names costs a second round.
func (s *Store) openLog(ctx context.Context, ref control.Ref) (*Index, error) {
	if !control.ValidID(ref.VM) || ref.Sequence == 0 {
		return nil, ErrInvalidConfig
	}
	listing, err := s.listLog(ctx, ref.VM)
	if err != nil {
		return nil, err
	}
	var mapped uint64
	for sequence, held := range listing {
		if sequence <= ref.Sequence && !held.mapObject.IsZero() && sequence > mapped {
			mapped = sequence
		}
	}
	for {
		if mapped == 0 {
			return nil, fmt.Errorf("%w: %s has no map object at or before it", platform.ErrNotFound, ref)
		}
		index, tables, err := s.readLogWindow(ctx, ref, mapped, listing)
		if err != nil {
			return nil, err
		}
		if mapped == ref.Sequence {
			return index, nil
		}
		last := tables[ref.Sequence]
		if len(last) == 0 || last[len(last)-1].record == nil {
			return nil, fmt.Errorf("%w: %s has no log record", platform.ErrNotFound, ref)
		}
		if named := last[len(last)-1].record.GetMap(); named != mapped {
			if named == 0 || named > ref.Sequence || listing[named] == nil || listing[named].mapObject.IsZero() {
				return nil, ErrCorrupt
			}
			mapped = named
			continue
		}
		return s.replayChain(index, ref, tables)
	}
}

// readLogWindow reads a map object and the tables of every part of every
// checkpoint after it up to ref, all at once.
func (s *Store) readLogWindow(ctx context.Context, ref control.Ref, mapped uint64,
	listing map[uint64]*logObjects) (*Index, map[uint64][]logTable, error) {
	type read struct {
		sequence uint64
		number   int
		key      platform.ObjectKey
	}
	var reads []read
	tables := make(map[uint64][]logTable)
	for sequence, held := range listing {
		if sequence <= mapped || sequence > ref.Sequence || len(held.parts) == 0 {
			continue
		}
		tables[sequence] = make([]logTable, len(held.parts))
		for number, key := range held.parts {
			if key.IsZero() {
				return nil, nil, fmt.Errorf("%w: %s/%d lacks part %d", ErrCorrupt, ref.VM, sequence, number)
			}
			reads = append(reads, read{sequence: sequence, number: number, key: key})
		}
	}
	var index *Index
	err := concurrently(ctx, len(reads)+1, replayConcurrency, func(ctx context.Context, at int) error {
		if at == len(reads) {
			loaded, err := s.readMap(ctx, control.Ref{VM: ref.VM, Sequence: mapped})
			index = loaded
			return err
		}
		table, err := s.readLogTable(ctx, reads[at].key)
		tables[reads[at].sequence][reads[at].number] = table
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return index, tables, nil
}

// replayChain walks from ref back to the map through each record's parent, and
// then replays those checkpoints forward over the map's index.
func (s *Store) replayChain(index *Index, ref control.Ref, tables map[uint64][]logTable) (*Index, error) {
	var chain []uint64
	for at := ref.Sequence; at != index.ref.Sequence; {
		held := tables[at]
		if len(held) == 0 || held[len(held)-1].record == nil {
			return nil, fmt.Errorf("%w: %s/%d is in the chain and has no log record", ErrCorrupt, ref.VM, at)
		}
		record := held[len(held)-1].record
		parent := record.GetParent()
		if record.GetMap() != index.ref.Sequence || parent.GetVm() != ref.VM ||
			parent.GetSequence() >= at || parent.GetSequence() < index.ref.Sequence {
			return nil, fmt.Errorf("%w: %s/%d does not lead back to its map", ErrCorrupt, ref.VM, at)
		}
		chain = append(chain, at)
		at = parent.GetSequence()
	}
	slices.Reverse(chain)
	for _, sequence := range chain {
		next, err := s.replay(index, control.Ref{VM: ref.VM, Sequence: sequence}, tables[sequence])
		if err != nil {
			return nil, err
		}
		index = next
	}
	return index, nil
}

// replay applies one log checkpoint to the index of the checkpoint it inherits
// and returns its own. The parent's segments are shared, and the ones this
// checkpoint changed are copied first.
func (s *Store) replay(parent *Index, ref control.Ref, tables []logTable) (*Index, error) {
	last := tables[len(tables)-1]
	if last.record == nil || last.parts != uint32(len(tables)) {
		return nil, ErrCorrupt
	}
	for _, earlier := range tables[:len(tables)-1] {
		if earlier.record != nil || earlier.parts != 0 {
			return nil, ErrCorrupt
		}
	}
	root := last.record.GetRoot()
	for _, volume := range root.GetVolumes() {
		if len(volume.GetSegments()) != 0 {
			return nil, ErrCorrupt
		}
	}
	index, err := decodeRootMessage(s, ref, root)
	if err != nil {
		return nil, err
	}
	if index.checkpoints[ref].parts != uint32(len(tables)) {
		return nil, ErrCorrupt
	}
	index.mapSequence = parent.mapSequence
	for name, table := range index.volumes {
		was := parent.volumes[name]
		if was == nil {
			continue
		}
		if was.geometry != table.geometry || was.ephemeral != table.ephemeral {
			return nil, ErrCorrupt
		}
		count := table.geometry.SegmentCount(table.size)
		for number, entry := range was.segments {
			if number >= count {
				continue
			}
			key := segmentKey{volume: name, number: number}
			if parent.loaded[key] == nil {
				return nil, ErrCorrupt
			}
			table.segments[number] = entry
			index.loaded[key] = parent.loaded[key]
		}
	}
	owned := make(map[segmentKey]bool)
	touch := func(volume string, number uint64) *segment {
		key := segmentKey{volume: volume, number: number}
		if !owned[key] {
			held := index.loaded[key]
			if held == nil {
				held = newSegment()
			} else {
				held = held.clone()
			}
			index.loaded[key] = held
			owned[key] = true
		}
		return index.loaded[key]
	}
	// A volume that shrank loses the pages past its end in the one segment that
	// straddles it, as the publication's trim did; the segments wholly past it
	// were not inherited.
	for name, table := range index.volumes {
		was := parent.volumes[name]
		if was == nil || was.size <= table.size || table.size == 0 {
			continue
		}
		pages := table.geometry.PageCount(table.size)
		number := table.geometry.SegmentOf(pages - 1)
		if _, addressed := table.segments[number]; !addressed {
			continue
		}
		held := touch(name, number)
		for relative := range held.pages {
			if table.geometry.SegmentBase(number)+uint64(relative) >= pages {
				delete(held.pages, relative)
			}
		}
	}
	for number, table := range tables {
		for _, member := range table.members {
			if member.State {
				continue
			}
			volume := index.volumes[member.Volume]
			if volume == nil || volume.ephemeral || member.Page >= volume.geometry.PageCount(volume.size) {
				return nil, ErrCorrupt
			}
			at := location{ref: ref, origin: ref, part: uint32(number), offset: member.Offset, length: member.Length}
			if member.OriginVM != "" {
				at.origin = control.Ref{VM: member.OriginVM, Sequence: member.OriginSequence}
			}
			if err := index.checkLocation(at); err != nil {
				return nil, err
			}
			geometry := volume.geometry
			touch(member.Volume, geometry.SegmentOf(member.Page)).pages[geometry.OffsetIn(member.Page)] = at
		}
	}
	for _, removed := range last.record.GetRemoved() {
		volume := index.volumes[removed.GetVolume()]
		if volume == nil {
			return nil, ErrCorrupt
		}
		for _, page := range removed.GetPages() {
			geometry := volume.geometry
			delete(touch(removed.GetVolume(), geometry.SegmentOf(page)).pages, geometry.OffsetIn(page))
		}
	}
	for key := range owned {
		table := index.volumes[key.volume]
		held := index.loaded[key]
		if len(held.pages) == 0 {
			delete(table.segments, key.number)
			delete(index.loaded, key)
			continue
		}
		table.segments[key.number] = segmentEntry{reads: held.reads()}
	}
	index.replayed()
	// Everything the replayed table reads from must be a checkpoint the record
	// names, or the record and the parts disagree.
	for read := range index.readCheckpoints() {
		if _, named := index.checkpoints[read]; !named {
			return nil, fmt.Errorf("%w: %s reads %s, which its record does not name", ErrCorrupt, ref, read)
		}
	}
	return index, nil
}

// reclaimLog is Reclaim for the log layout. A checkpoint after the current map
// object is replayed by every open, so nothing of it may go even once nothing
// reads its pages: only a new map object frees anything. When one lands, every
// checkpoint of this VM before it that neither index names is deleted, and so
// is every map object before it. A protected checkpoint spares everything up to
// it, because an open of it replays from a map before it.
func (s *Store) reclaimLog(ctx context.Context, previous, current *Index, protected []uint64) error {
	if current.mapSequence <= previous.mapSequence {
		return nil
	}
	spared := make(map[control.Ref]bool)
	for _, ref := range append(previous.named(), current.named()...) {
		spared[ref] = true
	}
	var pinned uint64
	for _, sequence := range protected {
		pinned = max(pinned, sequence)
	}
	listing, err := s.listLog(ctx, current.ref.VM)
	if err != nil {
		return err
	}
	var errs []error
	for _, sequence := range slices.Sorted(maps.Keys(listing)) {
		if sequence >= current.mapSequence || sequence <= pinned {
			continue
		}
		held := listing[sequence]
		if !held.mapObject.IsZero() {
			errs = append(errs, s.deleteObject(ctx, held.mapObject))
		}
		if spared[control.Ref{VM: current.ref.VM, Sequence: sequence}] {
			continue
		}
		for _, key := range held.parts {
			if !key.IsZero() {
				errs = append(errs, s.deleteObject(ctx, key))
			}
		}
	}
	return errors.Join(errs...)
}
