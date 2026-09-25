# Segmented index — 2026-09-15

*History. The store this describes was superseded by
[two planes](two-planes-2026-09-16.md), which separates the metadata plane
from the data and drops the word "pack".*

## The problem

An index is complete on its own: it names no parent and lists every page of
every volume. That is the right property, and it costs O(volume) bytes per
checkpoint. `Index.encode` emits one `Page` entry of roughly 16 bytes per
2 MiB page that holds bytes, so a 256 GiB volume that has been written once
pays about 2 MiB of index every 60 s whether one page changed or all of them,
and a 4 TiB volume pays about 40 MiB. Reading is the same: an open, a fork's
child and a migration destination each fetch the whole index before the first
page can be located. `maximumIndexSize` of 256 MiB is the only bound.

## The design

The page table is split into segments, and a segment is a member of the
checkpoint's pack like a page or the VMM state. The index object becomes a
root that locates segments instead of pages. A checkpoint writes only the
segments whose page table changed; every other segment entry of the root
points at the member an earlier checkpoint wrote, exactly as a page entry
does today. The root stays complete on its own: it names every segment and
every pack, and no reader opens another root.

### Segments

- `SegmentPages = 256`: one segment covers 512 MiB of a volume, pages
  `[n·256, (n+1)·256)`. It is a format constant, not a knob.
- A segment is a protobuf message `Segment` holding that range's sparse page
  table. It is complete on its own too: a segment-local pack list and
  segment-local origin list, with each `Page` naming its pack and origin by
  position in those lists and its number relative to the segment's start
  (`uint32`). Around 12 bytes per entry, at most about 3 KiB per segment.
- A segment is written into the pack as a member. `Member` gains
  `bool segment`; `volume` names the volume and `page` carries the segment
  number. Segments have no origin and are never zero-tombstoned.
- A segment whose page table is empty is not written and not named: an
  absent segment reads as zeroes, as an absent page does today.

### The root

`Index` (the object at `vm/<id>/ckpt/<seq>/index`) keeps `format_version`,
`volumes`, `packs`, `origins` (for the state only; segments carry their
own) and the state member. `Volume.pages` is replaced by
`Volume.segments`: `Segment{number, pack, part, offset, length,
repeated uint32 reads}`. `reads` lists, by position in the root's pack list,
every pack that segment's page entries name, so the root alone answers what
this checkpoint reads from. `Index.Packs`, `named`, `markEmptied` and
`retainReferencedPacks` work from `reads` and the state, never from segment
contents.

Root size is O(segments): 15 bytes per 512 MiB of volume, about 120 KiB for
4 TiB. `maximumIndexSize` stays as the bound on the root;
`maximumSegmentSize` of 1 MiB bounds a segment member.

Format bumps: index 5 → 6, pack 1 → 2. Both readers refuse every other
version by name, as now.

### Reading

`Index` holds the decoded root and loads segments on demand. `Locate` gains a
context and may do I/O: `Locate(ctx, volume, offset, length)`. A segment is
read as one range GET of its member through the existing `Cache`, keyed by
the member's location `(pack ref, part, offset)` rather than by a page
identity, so two roots naming the same segment member share one cached copy
and the cache's budget, eviction and coalescing apply unchanged. `Index`
stays immutable and safe for concurrent use; the per-index segment map is a
memo in front of the cache, filled under a mutex.

`volume.source` and every other caller pass their context through. Nothing
outside `checkpoint` sees a segment.

### Publication

`Commit` copies the parent's root. For each volume with edits it groups the
edits by segment, loads the parent's segment for each (an absent one is an
empty table), applies the page adds and tombstones, and after that volume's
pages writes that volume's changed segments as members in ascending segment
order. The root entry for each is the location `packWriter.add` returned;
unchanged segments keep the parent's entry. Member order within a pack is
therefore: state, then per volume in name order, its pages in number order,
then its segments in number order — deterministic, so a retried publication
still produces identical parts.

A volume resized smaller drops the segments past its new size, and the last
segment inside it is rewritten without the pages the size cut off. `Root`
publishes a root with no segments.

### Compaction and reclamation

A segment member's bytes are live bytes of its pack; `packCost.bytes`
already counts every member. Compaction moves live segments like live pages,
verbatim, and the root entry follows. `protectedBy` and `Reclaim` are
unchanged in shape: what a pinned checkpoint's index names is read from the
root's pack list, which `reads` keeps complete.

### Rebuild

A pack now carries the segments its checkpoint wrote, so `Rebuild` decodes
this checkpoint's segment members as authoritative for those segments and
inherits every other segment entry from the inherited root. It refuses a
pack whose page members and tombstones disagree with the segments it holds:
every page member must be located by a segment of the same pack, and every
tombstone's page must be absent from it. `requireSameIndex` compares roots.

### Consistency

`CheckIndex` opens every segment: each page entry's pack must be in the
segment's own list and in the root's list, the segment's `reads` must equal
the set its entries name, and every entry must lie inside its volume.
`volume.CheckDeployment` needs no change beyond what `CheckIndex` returns.

### Fixtures

New `index-6` and `pack-2` fixtures and a regenerated deployment fixture
under `volume/testdata`; `index-5`, `pack-1` and the
`deployment-record-4-index-5-pack-1` twin stay byte-identical as refusal
fixtures, and their refusal text is asserted with the version named.

## Proof

Red first, in `checkpoint`: publish one checkpoint that fills 2048
pages of a 4 GiB volume with compressible, non-zero data (eight segments),
then a second that dirties one page. Assert the second checkpoint's pack
holds exactly one segment member, its root names seven segments in the first
checkpoint's pack and one in its own, and the root object is under 4 KiB.
On the tree as it is, the second index is one entry per page and the
assertion on the root's size fails.

Then: the whole suite, `-race` on `checkpoint` and `volume`,
both format fixture tests, `TestScheduled*Reproduces`, and the migration chaos
campaign's first sixteen seeds, since a destination now locates pages through
segments it fetches lazily.

## Docs

`docs/volumes.md` (Objects, Reads, Compaction, Reclamation), `docs/metadata.md`
(index format 6, pack format 2), `docs/architecture.md`'s component table,
`docs/testing.md`'s format fixtures list, and the index item in
`docs/open-work.md`, which this closes.
