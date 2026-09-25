# Two planes: the store from first principles — 2026-09-16

## Why

The segmented index and the root in the pack were bolted onto a store
designed around one index object and data packs. The page table is
metadata: small, rewritten in pieces every checkpoint, read on every open.
Guest bytes are data: large, immutable, left behind until compaction. The
tree today stores the table inside the data containers, ties a segment's
lifetime to a part's, counts and moves table bytes in compaction, and names
the data with a second word, "pack", for what is exactly one checkpoint's
output. This plan separates the two planes and uses one word.

## Requirements

- One mutable object per VM; everything else immutable, create-if-absent,
  and a retried publication produces identical bytes.
- A checkpoint is complete on its own: no parent to chase at read time.
- Open in one GET. A cold page read is one segment fetch and one page fetch,
  both through the host-wide cache.
- A checkpoint costs O(dirty pages + touched segments); the root's
  O(touched volume / 512 MiB) is the accepted floor.
- A fork costs nothing: the child's root names the parent's objects.
- Reclamation, pins and compaction liveness work from roots alone.
- Data and metadata live in different objects.

## Layout

```
control/<id>                    the one mutable object
vm/<id>/ckpt/<seq>/index        the metadata plane: header, changed segments, root
vm/<id>/ckpt/<seq>/part/<n>     the data plane: members (VMM state, pages), table, trailer
```

There is no "pack". A checkpoint is an index object and its parts.

### The index object

One object per checkpoint. A fixed header names the index format and the
root's offset and length; then the segments this checkpoint changed, one
encoded protobuf each, in volume-name and segment-number order; then the
root. Its create-if-absent PUT is the commit: a checkpoint is published
exactly while its index object exists.

The root (`Index` message, format 7) lists per volume its size and one
`SegmentEntry` per segment that has ever been written: the segment number,
the address of the segment as (checkpoint position, offset, length) inside
that checkpoint's index object, and per checkpoint the segment's pages read
from, the summed member bytes. It lists every checkpoint it reads from:
those whose parts hold a page or the state, with part count, bytes and the
emptied grace as today, and those whose index object holds a segment it
addresses. It names the VMM state's member. It carries no format version of
its own (the header does), no parent and no reference.

Bounds: root at most 2 MiB; a segment at most 1 MiB; the index object at
most 64 MiB, which a fully dirty 4 TiB volume does not reach.

### Segments

A segment is identified by the checkpoint that wrote it, its volume and its
segment number, the way a page is identified by its origin, volume and
number. Its offset and length inside that checkpoint's index object are
only where to fetch it. The cache keys a segment by its identity, and the
root's entry carries both the identity's checkpoint and the location so a
fetch stays one range GET.

Unchanged in content: page entries with number relative to the segment,
checkpoint position, part, offset, length and origin position, over the
segment's own checkpoint list and origin list. A segment lives only in the
index object of the checkpoint that wrote it, at a fixed address, for as
long as any root addresses it. It is never moved, compacted or counted as
part liveness. A segment that was never written has no root entry; a
segment whose last page is zeroed loses its entry.

### Parts

A part holds members of two kinds, VMM state and pages, in the same
envelopes, followed by its table and a 32-byte trailer (format version,
table offset and length, part count in the last part, magic). The member
kind field keeps only those two kinds. Parts are numbered from zero; the root
says how many. The bounded tail stays: the table at most 256 KiB, sealed by
the writer, read as one suffix range by the consistency check. Part format 4.

### Publication

Stream parts as they fill: the VMM state, this checkpoint's dirty pages in
volume-name and page-number order, then compaction's rewrites. Then build the
index object: for each touched segment load the parent's version, apply the
final page locations, drop zero pages, encode, compute the byte sums; write
the segments, then the root; PUT. `Root` for a new VM writes an index object
holding only a root and no parts.

### Reading

`Open` is one GET of `index`, decoded from the header. `Locate` finds the
segment entry in the root, fetches the segment as one range GET on the
addressed checkpoint's index object through `checkpoint.Cache`, keyed by
the segment's identity, memoised per `Index`, then the page member as today.

### Reclamation and compaction

The set difference between consecutive roots runs over everything a root
names: checkpoints read for parts and checkpoints read for segments alike.
A pinned checkpoint's root protects everything it names. `deleteCheckpoint`
deletes `index` first, then the parts. Compaction measures liveness from the
root's byte sums over parts only, rewrites live members of mostly dead
checkpoints into the current parts, and never touches an index object.

### Terminology

"Pack" is gone from identifiers, keys, docs, comments, metrics and tests:
`checkpoint/internal/pack` becomes `checkpoint/internal/part`,
`Pack`/`packs`/`packCost`/`partKey` and the rest are renamed to say
checkpoint or part, `PackTable` becomes `PartTable`, the root's list is
`checkpoints`, and the docs say "a checkpoint's parts".

## Proof

Red first, in `checkpoint`:

1. A published checkpoint's objects are exactly `index` and `part/<n>`; the
   index object holds only the segments it changed; `Open` issues one request.
2. A checkpoint that changes one segment of a volume with eight written
   writes one segment into its index object and its root addresses seven in
   the parent's index object.
3. An index object stays until no root addresses a segment in it: after a
   checkpoint rewrites the last segment still addressed in an old index
   object, the next sweep deletes that object; before, it does not.
4. A publication interrupted before the index PUT is an absent checkpoint;
   a later publication under the same reference lands.
5. Compaction opens no index object it did not write and moves no segment.
6. Opening a format-6 or a root-in-part deployment is refused with the
   version named (existing fixtures).

Then the whole suite, `-race` on `checkpoint` and `volume`,
the fixture tests with new `index-7` and `part-4` fixtures and a regenerated
deployment fixture, `TestScheduled*Reproduces`, the migration chaos seeds
1–16, and 200 seeds of `internal/simtest`'s campaign.

## Docs

`docs/volumes.md` (Objects, Opening, Publication order, Reclamation,
Compaction, Page cache), `docs/metadata.md`, `docs/architecture.md`'s
component table, `docs/testing.md`'s fixtures list; every "pack" in docs/ and
plans/README.md's status lines. The plans that introduced the old shapes stay
as history with a one-line note pointing here.
