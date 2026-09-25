# The root in the pack — 2026-09-15

*History. The store this describes was superseded by
[two planes](two-planes-2026-09-16.md), which separates the metadata plane
from the data and drops the word "pack".*

## The problem

A checkpoint is its pack parts plus one index object, the root, at
`vm/<id>/ckpt/<seq>/index`. Since the segmented index the root is a small
table of pointers (about 15 bytes per 512 MiB of volume), yet it is still an
object of its own kind with its own key, its own PUT as the commit point, its
own format version and fixtures, and a "published" state the control record
and the reader both track. `Rebuild` and `Recover` exist to reconstruct that
object from the parts when it is lost, and the tombstones in every part table
exist so that reconstruction can agree with the segments; neither has a
production caller.

## The design

The root is the last member of the checkpoint's last part. A checkpoint is
parts only.

### Layout

- Keys: `vm/<id>/ckpt/<seq>/pack/0` … `pack/<n-2>` and
  `vm/<id>/ckpt/<seq>/pack/last`. A location's part number `n-1` names
  `last`; `partKey` is the one place that mapping lives. `pack/last` is what
  a reader opens; its trailer says how many parts there are, as it does
  today.
- The last part's members end with the root, a member of kind `root`, after
  that part's pages and segments. `Member` gains the kind; the trailer gains
  the root's offset and length so a suffix read finds it without decoding
  the table first. The root message is what the index object is today; it
  no longer carries a format version of its own, the pack's covers it.
- The root is bounded at 2 MiB (`maximumRootSize`), which is about 140,000
  segments or 70 TiB of volume per VM; the bound is documented as the limit
  it is. A suffix read of `maximumRootSize + maximumTableSize + TrailerSize`
  therefore yields root, table and trailer in one GET, and a whole smaller
  object otherwise.
- Tombstones go: a page zeroed by a checkpoint is simply absent from the
  segment that checkpoint rewrote. `Member.zero`, `packWriter.tombstone` and
  the tombstone table entries are removed.
- `Rebuild`, `Recover`, `adoptSegments` and `agreesWithPack` go, with their
  tests: a checkpoint whose last part is absent never committed and has
  nothing to recover; its earlier parts are what an interrupted publication
  leaves, which reclamation already handles by set difference.
- Pack format 2 → 3. There is no separate index format any more; the index
  fixtures are superseded and stay byte-identical, and opening a deployment
  that has `index` objects and no `pack/last` is refused with the format
  named.

### Publication

`Commit` uploads parts `0 … n-2` as it fills them, bounded as today, and
seals the last part after compaction and the changed segments with the root
as its final member. The last part's create-if-absent PUT is the commit: a
retry of the same publication produces the same bytes and compares by
digest, as `putIndex` does today; a different root under the same reference
is `ErrConflict`. `Root` publishes a checkpoint of one part holding only the
root. Nothing else about ordering changes.

### Reading

`Open(ref)` is one suffix GET of `pack/last`; it decodes the trailer, the
root and the table from the bytes in hand and refuses a root the trailer
places outside them. `readPartTable` is unchanged in shape. Everything that
asks "is the index published" now asks whether `pack/last` exists; the
control record's published flag keeps its meaning and its name.

### Reclamation and consistency

`deleteCheckpoint` deletes `pack/last` first, so that a checkpoint stops
being openable before its earlier parts go, then the rest. `CheckIndex`
opens `pack/last` like any reader. `DeleteVM` and `Reclaim` are unchanged in
shape.

## Proof

Red first, in `checkpoint`:

1. A published checkpoint's objects are exactly its parts: no key under its
   prefix but `pack/<n>` and `pack/last`, and `Open` issues exactly one
   object-store request (count through the simulated store's trace).
2. A publication interrupted before `pack/last` leaves a checkpoint `Open`
   reports as absent, and a later publication under the same reference lands.
3. A zeroed page is absent from the rewritten segment and reads as zeroes
   after a reopen, with no tombstone in any table.
4. Opening a format-6 deployment (the existing fixture) is refused with the
   version named.

Then the whole suite, `-race` on `checkpoint` and `volume`,
the format fixture tests with a new `pack-3` fixture and a regenerated
deployment fixture, `TestScheduled*Reproduces`, the migration chaos seeds
1–16 and 200 seeds of `internal/simtest`'s campaign.

## Docs

`docs/volumes.md` (Objects, Opening, Publication order, Reclamation),
`docs/metadata.md`, `docs/architecture.md`'s component table and
identities section, `docs/testing.md`'s format fixtures list.
