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
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
)

// A checkpoint may defer its index object (TASK-81). The index object of a
// checkpoint holds the segments of page table it changed, and a checkpoint
// whose dirty pages are scattered rewrites most of them, so at a short
// interval the index costs nearly as much to upload as the pages. The parts
// already say which pages they hold, so a checkpoint between two index objects
// writes none: its last part carries a DeferredIndex, which is its root
// without segments, the base — the newest index object before it — the
// checkpoints between the base and it, which deferred theirs too, and the pages
// it zeroed. That part is put only once every earlier part is durable, and its
// PUT is the commit, so such a checkpoint is one round of PUTs rather than two.
//
// A segment such a checkpoint changed is pending: no object holds it, and the
// index holds it decoded. The next index object writes every pending segment
// beside the ones its own checkpoint changed, so the index object, its root and
// an open of it are what they always were. Config.IndexEvery says how many
// checkpoints may pass between index objects; a VM's first checkpoint, and a
// fork's, always write one, because the chain an open replays never crosses
// into another VM's objects.
//
// An open of a checkpoint with no index object lists its parts, reads the last
// one's table, and then reads the base's index object and every part table of
// the chain at once, and replays the chain forward over the base. So the
// deferred index names the base, the chain, and every checkpoint whose index
// object holds a segment of the base, which a replay starts from; reclamation, pins and
// deletion spare them as they spare anything else a root names; compaction
// leaves them alone, because rewriting their pages frees nothing until the next
// index object stops naming them.

const (
	// deferredTail is how much of the end of a part an open of a deferred
	// index reads for its table. A table this does not hold costs one more
	// read.
	deferredTail = 64 << 10
	// replayConcurrency bounds how many objects an open of a deferred index
	// reads at once.
	replayConcurrency = 16
)

// defers reports whether the checkpoint being published defers its index
// object: the store allows it, the parent is this VM's own, and fewer than
// IndexEvery checkpoints, this one included, follow the parent's base.
func (p *Publication) defers() bool {
	if p.store.indexEvery <= 1 || p.parent == nil || p.parent.ref.VM != p.ref.VM {
		return false
	}
	return len(p.parent.replayChain()) < p.store.indexEvery
}

// replayChain is the checkpoints an open of a checkpoint inheriting this one
// replays up to it: this one's own replays when it deferred its index object,
// and otherwise this one alone, as the base.
func (i *Index) replayChain() []control.Ref {
	if i.deferred() {
		return slices.Clone(i.replays)
	}
	return []control.Ref{i.ref}
}

// chainHolds is what an index inheriting this one names because a replay
// reads the base's segments: this one's own holds when it deferred its index
// object, and otherwise every checkpoint whose index object holds one of this
// one's segments, because it is the base.
func (i *Index) chainHolds() []control.Ref {
	if i.deferred() {
		return slices.Clone(i.holds)
	}
	held := make(map[control.Ref]bool)
	for _, table := range i.volumes {
		for _, entry := range table.segments {
			held[entry.at.ref] = true
		}
	}
	return sortedRefs(held)
}

// deferredIndex is what the checkpoint being published says of itself beside
// the pages its parts name. It is deterministic, so a retry seals the same
// part.
func (p *Publication) deferredIndex(index *Index) *checkpointv1.DeferredIndex {
	root, err := index.rootMessage(false)
	if err != nil {
		return nil
	}
	chain := make([]uint64, 0, len(index.replays))
	for _, ref := range index.replays[1 : len(index.replays)-1] {
		chain = append(chain, ref.Sequence)
	}
	record := checkpointv1.DeferredIndex_builder{Root: root,
		Base: proto.Uint64(index.replays[0].Sequence), Chain: chain}
	for _, name := range slices.Sorted(maps.Keys(p.removed)) {
		record.Removed = append(record.Removed, checkpointv1.RemovedPages_builder{
			Volume: proto.String(name), Pages: p.removed[name]}.Build())
	}
	return record.Build()
}

// deferredParts lists one checkpoint's parts and reports how many there are:
// one past the highest numbered, which an open of a deferred index reads the
// table of first.
func (s *Store) deferredParts(ctx context.Context, ref control.Ref) (uint32, error) {
	root := s.checkpointPrefix(ref) + "part/"
	prefix, err := platform.NewObjectPrefix(root)
	if err != nil {
		return 0, err
	}
	var parts uint32
	err = platform.ListAll(ctx, s.objects, prefix, func(object platform.ObjectMetadata) error {
		number, err := strconv.ParseUint(strings.TrimPrefix(object.Key.String(), root), 10, 32)
		if err == nil {
			parts = max(parts, uint32(number)+1)
		}
		return nil
	})
	return parts, err
}

// openDeferred opens a checkpoint that deferred its index object. Its last
// part says what it rebuilds from; the base's index object and every part
// table of the chain are then read at once, and the chain is replayed forward
// over the base. A checkpoint whose last part carries no deferred index is
// absent: it never committed, or it is not one this layout wrote.
func (s *Store) openDeferred(ctx context.Context, ref control.Ref) (*Index, error) {
	if !control.ValidID(ref.VM) || ref.Sequence == 0 {
		return nil, ErrInvalidConfig
	}
	parts, err := s.deferredParts(ctx, ref)
	if err != nil {
		return nil, err
	}
	if parts == 0 {
		return nil, fmt.Errorf("%w: %s has no index object and no parts", platform.ErrNotFound, ref)
	}
	lastKey, err := s.partKey(ref, parts-1)
	if err != nil {
		return nil, err
	}
	last, err := s.readTable(ctx, lastKey, deferredTail)
	if err != nil {
		return nil, err
	}
	if last.deferred == nil || last.parts != parts {
		return nil, fmt.Errorf("%w: %s has no index object, and its parts do not commit it", platform.ErrNotFound, ref)
	}
	// From here on the checkpoint is committed, so anything it needs that is
	// gone is a store disagreeing with itself rather than an absent checkpoint.
	index, err := s.rebuild(ctx, ref, parts, last)
	if errors.Is(err, platform.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s replays an object that is gone: %v", ErrCorrupt, ref, err)
	}
	return index, err
}

// rebuild replays a committed checkpoint's chain over its base, given its part
// count and its last part's table.
func (s *Store) rebuild(ctx context.Context, ref control.Ref, parts uint32, last partTable) (*Index, error) {
	record := last.deferred
	chain, err := deferredChain(ref, record)
	if err != nil {
		return nil, err
	}
	// What each checkpoint of the chain has is what this one's root names it
	// with: the chain is named, so every part of it is counted there.
	named, err := decodeRootMessage(s, ref, record.GetRoot())
	if err != nil {
		return nil, err
	}
	type read struct {
		ref    control.Ref
		number uint32
	}
	var reads []read
	tables := make(map[control.Ref][]partTable, len(chain))
	for _, at := range chain {
		count := named.checkpoints[at].parts
		if at == ref {
			count = parts
		}
		if count == 0 {
			return nil, fmt.Errorf("%w: %s replays %s, which has no parts", ErrCorrupt, ref, at)
		}
		tables[at] = make([]partTable, count)
		for number := range count {
			if at == ref && number == parts-1 {
				tables[at][number] = last
				continue
			}
			reads = append(reads, read{ref: at, number: number})
		}
	}
	baseRef := control.Ref{VM: ref.VM, Sequence: record.GetBase()}
	var base *Index
	err = concurrently(ctx, len(reads)+1, replayConcurrency, func(ctx context.Context, at int) error {
		if at == len(reads) {
			opened, err := s.Open(ctx, baseRef)
			base = opened
			return err
		}
		key, err := s.partKey(reads[at].ref, reads[at].number)
		if err != nil {
			return err
		}
		table, err := s.readTable(ctx, key, deferredTail)
		tables[reads[at].ref][reads[at].number] = table
		return err
	})
	if err != nil {
		return nil, err
	}
	if base.deferred() {
		return nil, fmt.Errorf("%w: %s rebuilds from %s, which has no index object", ErrCorrupt, ref, baseRef)
	}
	if err := s.prefetchReplayed(ctx, base, chain, tables); err != nil {
		return nil, err
	}
	index := base
	for _, at := range chain {
		if index, err = s.replay(index, at, tables[at]); err != nil {
			return nil, err
		}
		if len(index.replays) == 0 || index.replays[0] != baseRef {
			return nil, ErrCorrupt
		}
	}
	return index, nil
}

// deferredChain is the checkpoints an open of ref replays, in order, ref last:
// the chain its deferred index names, which must lie strictly between the base
// and ref, ascending.
func deferredChain(ref control.Ref, record *checkpointv1.DeferredIndex) ([]control.Ref, error) {
	previous := record.GetBase()
	if previous == 0 || previous >= ref.Sequence {
		return nil, fmt.Errorf("%w: %s rebuilds from checkpoint %d", ErrCorrupt, ref, previous)
	}
	chain := make([]control.Ref, 0, len(record.GetChain())+1)
	for _, sequence := range append(slices.Clone(record.GetChain()), ref.Sequence) {
		if sequence <= previous {
			return nil, fmt.Errorf("%w: %s replays its chain out of order", ErrCorrupt, ref)
		}
		previous = sequence
		chain = append(chain, control.Ref{VM: ref.VM, Sequence: sequence})
	}
	if len(chain) > maximumIndexEvery {
		return nil, fmt.Errorf("%w: %s replays %d checkpoints", ErrCorrupt, ref, len(chain))
	}
	return chain, nil
}

// prefetchReplayed fetches at once every segment of the base that a replay of
// chain will change, so the replay itself reads nothing. The rest stay
// addressed in index objects and are fetched when a read needs them, as for
// any open.
func (s *Store) prefetchReplayed(ctx context.Context, base *Index, chain []control.Ref,
	tables map[control.Ref][]partTable) error {
	touched := make(map[segmentKey]bool)
	add := func(volume string, page uint64) {
		table := base.volumes[volume]
		if table == nil {
			return
		}
		number := table.geometry.SegmentOf(page)
		if _, addressed := table.segments[number]; addressed {
			touched[segmentKey{volume: volume, number: number}] = true
		}
	}
	sizes := make(map[string]uint64, len(base.volumes))
	for name, table := range base.volumes {
		sizes[name] = table.size
	}
	for _, ref := range chain {
		held := tables[ref]
		for _, table := range held {
			for _, member := range table.members {
				if !member.State {
					add(member.Volume, member.Page)
				}
			}
		}
		record := held[len(held)-1].deferred
		if record == nil {
			continue
		}
		for _, removed := range record.GetRemoved() {
			for _, page := range removed.GetPages() {
				add(removed.GetVolume(), page)
			}
		}
		// A volume that shrank trims the segment straddling its new end.
		for _, volume := range record.GetRoot().GetVolumes() {
			name, size := volume.GetName(), volume.GetSize()
			if was := base.volumes[name]; was != nil && size != 0 && size < sizes[name] {
				add(name, was.geometry.PageCount(size)-1)
			}
			sizes[name] = size
		}
	}
	// The segments one index object holds are read in one ranged GET, which a
	// scattered chain makes the difference between a request per segment and
	// one per object.
	holders := make(map[control.Ref][]segmentKey)
	for key := range touched {
		at := base.volumes[key.volume].segments[key.number].at
		holders[at.ref] = append(holders[at.ref], key)
	}
	refs := sortedRefs(holders)
	if len(refs) == 0 {
		return nil
	}
	return concurrently(ctx, len(refs), replayConcurrency, func(ctx context.Context, at int) error {
		return s.loadSegments(ctx, base, refs[at], holders[refs[at]])
	})
}

// loadSegments decodes into base the segments keys names, all of which the
// index object of ref holds, out of one read of the range spanning them.
func (s *Store) loadSegments(ctx context.Context, base *Index, ref control.Ref, keys []segmentKey) error {
	address := func(key segmentKey) segmentAddress { return base.volumes[key.volume].segments[key.number].at }
	if len(keys) == 1 {
		_, err := base.segmentAt(ctx, keys[0].volume, keys[0].number)
		return err
	}
	start, end := uint64(maximumIndexSize), uint64(0)
	for _, key := range keys {
		at := address(key)
		start, end = min(start, at.offset), max(end, at.offset+at.length)
	}
	object, err := s.indexKey(ref)
	if err != nil {
		return err
	}
	data, err := s.readRange(ctx, object, start, end-start, maximumIndexSize)
	if err != nil {
		return err
	}
	for _, key := range keys {
		at := address(key)
		encoded, err := s.codecs.Decode(ctx, data[at.offset-start:][:at.length], maximumSegmentSize)
		if err != nil {
			return errors.Join(ErrCorrupt, err)
		}
		held, err := base.decodeSegment(key.volume, encoded)
		if err != nil {
			return err
		}
		base.mu.Lock()
		if base.loaded[key] == nil {
			base.loaded[key] = held
		}
		base.mu.Unlock()
	}
	return nil
}

// replay applies one checkpoint that deferred its index object to the index of
// the checkpoint it inherits, and returns its own. The parent's decoded
// segments are shared, and the ones this checkpoint changed are copied first
// and left pending, as its publication left them.
func (s *Store) replay(parent *Index, ref control.Ref, tables []partTable) (*Index, error) {
	last := tables[len(tables)-1]
	if last.deferred == nil || last.parts != uint32(len(tables)) {
		return nil, fmt.Errorf("%w: %s is in a chain and does not commit itself", ErrCorrupt, ref)
	}
	for _, earlier := range tables[:len(tables)-1] {
		if earlier.deferred != nil || earlier.parts != 0 {
			return nil, ErrCorrupt
		}
	}
	record := last.deferred
	chain := parent.replayChain()
	if record.GetBase() != chain[0].Sequence || len(record.GetChain()) != len(chain)-1 {
		return nil, fmt.Errorf("%w: %s does not rebuild from its parent's base", ErrCorrupt, ref)
	}
	for at, sequence := range record.GetChain() {
		if chain[at+1].Sequence != sequence {
			return nil, fmt.Errorf("%w: %s does not replay its parent's chain", ErrCorrupt, ref)
		}
	}
	root := record.GetRoot()
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
	index.replays = append(chain, ref)
	index.holds = parent.chainHolds()
	parent.mu.Lock()
	for name, table := range index.volumes {
		was := parent.volumes[name]
		if was == nil {
			continue
		}
		if was.geometry != table.geometry || was.ephemeral != table.ephemeral {
			parent.mu.Unlock()
			return nil, ErrCorrupt
		}
		count := table.geometry.SegmentCount(table.size)
		for number, entry := range was.segments {
			if number >= count {
				continue
			}
			table.segments[number] = entry
			if held := parent.loaded[segmentKey{volume: name, number: number}]; held != nil {
				index.loaded[segmentKey{volume: name, number: number}] = held
			}
		}
	}
	parent.mu.Unlock()
	owned := make(map[segmentKey]bool)
	var missing error
	touch := func(volume string, number uint64) *segment {
		key := segmentKey{volume: volume, number: number}
		if !owned[key] {
			held := index.loaded[key]
			switch {
			case held != nil:
				held = held.clone()
			case !addressed(index, key):
				held = newSegment()
			default:
				// The prefetch fetched every segment a replay changes.
				missing = ErrCorrupt
				held = newSegment()
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
	for _, removed := range record.GetRemoved() {
		volume := index.volumes[removed.GetVolume()]
		if volume == nil || volume.ephemeral {
			return nil, ErrCorrupt
		}
		for _, page := range removed.GetPages() {
			if page >= volume.geometry.PageCount(volume.size) {
				return nil, ErrCorrupt
			}
			geometry := volume.geometry
			delete(touch(removed.GetVolume(), geometry.SegmentOf(page)).pages, geometry.OffsetIn(page))
		}
	}
	if missing != nil {
		return nil, missing
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
	// Everything the rebuilt table reads from must be a checkpoint the record
	// names, or the record and the parts disagree.
	for read := range index.readCheckpoints() {
		if _, named := index.checkpoints[read]; !named {
			return nil, fmt.Errorf("%w: %s reads %s, which its deferred index does not name", ErrCorrupt, ref, read)
		}
	}
	return index, nil
}

// addressed reports whether an index has an entry for a segment at all.
func addressed(index *Index, key segmentKey) bool {
	table := index.volumes[key.volume]
	if table == nil {
		return false
	}
	_, found := table.segments[key.number]
	return found
}
