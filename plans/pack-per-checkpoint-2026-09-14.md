# One pack object per checkpoint — 2026-09-14

*History. The store this describes was superseded by
[two planes](two-planes-2026-09-16.md), which separates the metadata plane
from the data and drops the word "pack".*

Decision by the owner: a checkpoint writes its dirty pages as one object, not
one object per page. Per-page objects cost one PUT per dirty 2 MiB page; at a 5
s interval and a hundred dirty pages per checkpoint that is twenty PUTs a second
per VM. Everything per-page objects gave — a fault is one read, reclamation is a
delete, forks share by reference — survives with offsets in the index and range
reads.

## Terms

A **checkpoint** is the operation: pause, save device state, seal the dirty set,
resume, upload. A **checkpoint** is what it leaves in the store at one
sequence. A **page** is a 2 MiB-aligned range of a volume; in the pager it is
a page, in the store it is a member of a pack. There are no deltas.

## Layout

```
vm/<id>/control                 control record: epoch, selected, pins
vm/<id>/ckpt/<seq>/index        the checkpoint's table
vm/<id>/ckpt/<seq>/pack/<n>     pack part n, n from 0
```

A pack part is a concatenation of members followed by a table and a fixed
trailer, so a part describes itself: an index can be rebuilt from packs alone.
Each member is one `internal/blob` envelope (48-byte header, compressed payload)
exactly as a page object is encoded today. Members are the VMM state, when the
checkpoint saved one, followed by the dirty pages in ascending volume-name and
page-number order. The table is a protobuf message listing every member in order
— `{volume, page, offset, length}`, with the state member marked — and the
trailer is the table's offset and length plus a magic, at fixed size at the end
so a reader finds the table with one range read of the tail. Parts fill
sequentially to a target of 64 MiB of encoded member bytes and upload as they
fill, concurrently within the store's slot limit, so a large checkpoint costs a
few PUTs and bounded memory while a typical checkpoint costs one. The index
carries the same offsets so normal reads never touch the table.

The index (`checkpoint/proto`, format version bumped) becomes:

```
Index   { format_version, volumes, packs,
          state_sequence, state_part, state_offset, state_length }   # zero length: no state
Volume  { name, size, pages }
Page    { number, sequence, part, offset, length }   # the encoded member in ckpt/<sequence>/pack/<part>
Pack    { sequence, parts, bytes }                   # every sequence this index names
```

No `parent`: an index is complete on its own, and nothing read the field. No
`ref`: the key names the VM and sequence, and the field was only checked
against it.

`Page.sequence` names the checkpoint whose pack holds the page's current bytes;
an untouched page keeps the entry its parent index had. `packs` lists every
sequence the index names, carried forward from the parent and extended with this
checkpoint's own entry, so compaction and reclamation never read another index.
A retried publication under the same ref must produce byte-identical parts and
index, as today.

## Reads

`Read` resolves each page to its entry and issues a range `Get` of
`length` bytes at `offset` in `ckpt/<sequence>/pack/<part>`, decodes the
envelope, and caches the decoded page. The cache key is the page's
`control.Identity{Ref, Volume, Page}`, which is unchanged, so the pager's
sharing by identity is untouched. `ReadState` is a range read of the state
fields. Adjacent members of one part are contiguous, so a later read-ahead
may fetch several in one range; not required now.

## Reclamation

After checkpoint N is selected, with P the previous index and C the current:

```
dead = (sequences(P) ∪ {P.seq}) − sequences(C) − protected
```

`sequences(I)` is the set of sequences `I.packs` names. For each dead
sequence, list and delete everything under `ckpt/<seq>/`, the index last.
`protected` is every pinned sequence and every sequence the pinned
checkpoint's index names, read once per pinned sequence and memoised
(indexes are immutable). A fork reads through those packs.

This replaces `Store.Reclaim(previous, current)` and `Index.Objects`. It also
closes a leak in the current rule: a page published at sequence 3, still
referenced at 4 and overwritten at 5 was never deleted, because reclamation
only considered objects published under the checkpoint being replaced.

## Compaction

A cold page keeps its pack alive after every other member is dead, so packs
would accumulate dead bytes without bound. Each checkpoint bounds that: while
planning checkpoint N, for every sequence M in the parent's `packs` that is not
protected, live(M) is the sum of `length` over the index's locations in M and
total(M) is `packs[M].bytes`. Sequences with live below half of total are
rewritten, lowest fraction first, up to 64 MiB of live bytes per checkpoint:
their live pages are read (from the page cache or by range read) after the guest
has resumed, re-encoded as members of checkpoint N, and their locations moved to
N. M then leaves `sequences(C)` and the reclamation rule deletes it. Dead bytes
in referenced packs are therefore bounded at twice live bytes. Nothing here runs
under the vCPU pause.

## Unchanged

Checkpoints, forks (root index inherits the parent's locations; the parent pins
the sequence), migration (the handoff's unpublished pages are packed by the
destination's next checkpoint), the control record, the pager, image import.

## Removed

`pageKey`, `stateKey`, per-object uploads in `Publication.upload`, `Object`,
`Index.Objects`, `Store.Reclaim`'s object walk, `has_state`, and whatever the
delta checkpoint leaves of base-plus-delta lookup.

## Proof

Unit: pack encoding is deterministic and members decode from their recorded
locations; a part's table and trailer describe every member and an index rebuilt
from a checkpoint's parts equals the published one; a multi-part checkpoint;
reads of untouched, rewritten and grown pages; reclamation deletes exactly the
dead sequences and spares everything a fork inherits, including the leak case above;
compaction rewrites the right packs and stays under its byte bound; a retried
publication is byte-identical. Simulation and mutation configs updated. Linux
suites in Lima. The GCE demo's five flows, with the per-checkpoint object count
printed by `status` before and after.
