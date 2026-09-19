package checkpoint

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
)

// IndexViolation is one thing wrong with what a published root names: the
// object the complaint is about, and what is wrong with it. A part that is
// missing or does not parse is reported against that part; a checkpoint whose
// parts do not add up to what the root recorded is reported against the index
// object, which is what recorded it.
type IndexViolation struct {
	Key platform.ObjectKey
	Err error
}

func (v IndexViolation) Error() string { return v.Key.String() + ": " + v.Err.Error() }

func (v IndexViolation) Unwrap() error { return v.Err }

// CheckIndex reports what one published checkpoint requires of the object store
// and what is wrong with what it finds. It returns every key the root makes
// reachable — the index object and every part of every checkpoint it names, its
// own included and the ones its compaction emptied too — and one violation for
// each part that is missing or does not parse at the layout version this build
// writes, for each checkpoint whose parts do not hold the part count and the
// member bytes the root recorded, and for each segment the root addresses that
// does not read back or whose entries do not read from the checkpoints, and for
// the bytes, the root says they do.
//
// Every segment is opened. Reading one settles the rest of what a segment owes:
// each page entry names a checkpoint the segment and the root both list, and
// lies inside the volume as this checkpoint sizes it, or the segment does not
// decode at all.
//
// A checkpoint with an unreadable part is not also reported for its totals:
// what a part that could not be read holds is unknown, not wrong.
//
// The keys come back whether or not the objects behind them are sound, because
// a deployment-wide check subtracts them from the store's listing to find what
// nothing reaches: an object that is named and broken is one violation, not two.
//
// Every part's trailer and table are read, so this is a whole-store audit and
// never on a serving path.
func (s *Store) CheckIndex(ctx context.Context, index *Index) ([]platform.ObjectKey, []IndexViolation) {
	if index == nil {
		return nil, []IndexViolation{{Err: ErrInvalidConfig}}
	}
	key, err := s.indexKey(index.ref)
	if err != nil {
		return nil, []IndexViolation{{Err: fmt.Errorf("checkpoint %s: %w", index.ref, err)}}
	}
	var keys []platform.ObjectKey
	var violations []IndexViolation
	// unreadable collects the checkpoints a part of which did not read. What
	// such a checkpoint holds is unknown rather than wrong, so its totals are
	// not reported against it a second time.
	unreadable := make(map[control.Ref]bool)
	for _, ref := range index.named() {
		entry := index.checkpoints[ref]
		// Naming a checkpoint spares it whole: its index object holds the
		// segments some root still addresses in it, and its parts hold the pages
		// some root still reads. Both are therefore reached, however long ago
		// that checkpoint stopped being selected.
		named, err := s.indexKey(ref)
		if err != nil {
			violations = append(violations, IndexViolation{Key: key,
				Err: fmt.Errorf("checkpoint %s: %w", ref, err)})
			unreadable[ref] = true
			continue
		}
		keys = append(keys, named)
		body, stated, unread := uint64(0), uint32(0), false
		for number := uint32(0); number < entry.parts; number++ {
			partKey, err := s.partKey(ref, number)
			if err != nil {
				violations = append(violations, IndexViolation{Key: key,
					Err: fmt.Errorf("checkpoint %s part %d: %w", ref, number, err)})
				unread = true
				continue
			}
			keys = append(keys, partKey)
			table, err := s.readPartTable(ctx, ref, number)
			if err != nil {
				violations = append(violations, IndexViolation{Key: partKey,
					Err: fmt.Errorf("the part %s of checkpoint %s names does not read: %w",
						ref, index.ref, err)})
				unread = true
				continue
			}
			body += table.body
			if table.parts != 0 {
				stated = table.parts
			}
		}
		if unread {
			unreadable[ref] = true
			continue
		}
		if stated != entry.parts {
			violations = append(violations, IndexViolation{Key: key,
				Err: fmt.Errorf("checkpoint %s holds %d parts, the root names %d: %w",
					ref, stated, entry.parts, ErrCorrupt)})
		}
		if body != entry.bytes {
			violations = append(violations, IndexViolation{Key: key,
				Err: fmt.Errorf("checkpoint %s holds %d member bytes, the root records %d: %w",
					ref, body, entry.bytes, ErrCorrupt)})
		}
	}
	for _, name := range index.names {
		table := index.volumes[name]
		for _, number := range slices.Sorted(maps.Keys(table.segments)) {
			if unreadable[table.segments[number].at.ref] {
				continue
			}
			held, err := index.segmentAt(ctx, name, number)
			if err != nil {
				violations = append(violations, IndexViolation{Key: key,
					Err: fmt.Errorf("segment %d of %s, which checkpoint %s addresses, does not read: %w",
						number, name, index.ref, err)})
				continue
			}
			if reads := held.reads(); !slices.Equal(reads, table.segments[number].reads) {
				violations = append(violations, IndexViolation{Key: key,
					Err: fmt.Errorf("segment %d of %s reads %v, the root records %v: %w",
						number, name, reads, table.segments[number].reads, ErrCorrupt)})
			}
			for _, relative := range slices.Sorted(maps.Keys(held.pages)) {
				page := table.geometry.SegmentBase(number) + uint64(relative)
				if _, span := table.geometry.PageSpan(table.size, page); span == 0 {
					violations = append(violations, IndexViolation{Key: key,
						Err: fmt.Errorf("segment %d of %s locates page %d, past the volume's %d bytes: %w",
							number, name, page, table.size, ErrCorrupt)})
				}
			}
		}
	}
	return keys, violations
}
