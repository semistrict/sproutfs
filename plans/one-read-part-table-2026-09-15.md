# A part's table in one read — 2026-09-15

*History. The store this describes was superseded by
[two planes](two-planes-2026-09-16.md), which separates the metadata plane
from the data and drops the word "pack".*

## The problem

Reading a pack part's table costs three round trips: a `Head` for the
object's size, a range `Get` of the 32-byte trailer at the end, and a range
`Get` of the table the trailer locates. `Rebuild`, `Recover` and `CheckIndex`
pay it per part, and any future reader that works from the parts alone (an
index written only every N checkpoints was discussed and not adopted) would
pay it on every open. The table is small — one entry per member — so the
round trips are the whole cost.

## The design

A part's tail is bounded, and a reader fetches it as one suffix range.

- `platform.ByteRange` gains a suffix form: `Suffix int64`, the number of
  trailing bytes to read, exclusive with `Offset`/`Length`. A suffix longer
  than the object returns the whole object, as HTTP suffix ranges do. The
  simulated store and the GCS adapter (`NewRangeReader` with a negative
  offset) implement it; the sim's recording and trace carry it.
- `maximumTableSize` drops from 16 MiB to 256 KiB, and it bounds what a
  writer produces, not only what a reader accepts: `packWriter` tracks the
  encoded size of the table it is building and seals the part when the next
  member's entry would carry it past the bound, exactly as it seals at
  `partTargetBytes`. A tombstone is a table entry with no bytes, so a
  checkpoint zeroing a great many pages now spreads its tombstones over
  several small parts instead of one unbounded table.
- `readPartTable` does one `Get` with `Suffix: maximumTableSize +
  TrailerSize`, decodes the trailer from the last 32 bytes of what came
  back, checks that the table the trailer locates lies inside the bytes it
  holds, and decodes it from there. The part's size is what the trailer's
  offsets are checked against; it is the suffix's length plus the table's
  offset when the object was longer than the suffix, or the bytes returned.
- `pack.DecodeTrailer` keeps its checks; the reader supplies the size it
  derived.

The pack layout does not change — the same members, table and trailer in the
same order — so the format version stays 2 and the format-2 fixture is
unchanged. What changes is a bound a writer must respect, and that is tested
against the writer.

## Proof

Red first, in `checkpoint`:

1. A publication whose tombstones alone would exceed the 256 KiB table
   bound produces more than one part, and every part's table decodes from a
   suffix read of `maximumTableSize + TrailerSize` bytes.
2. Reading a part's table issues exactly one object-store request (count
   through the simulated store's trace): no `Head`, no second `Get`.
3. The GCS adapter's suffix range is exercised by its existing fake or unit
   test; the simulated store's suffix read is tested for a suffix longer
   than the object.

Then the whole suite, `-race` on `checkpoint`, the format fixture
tests, `TestScheduled*Reproduces` and the migration chaos seeds 1–16.

## Docs

`docs/volumes.md` (Objects: the bounded tail and the one-read table),
`docs/metadata.md`, and the `Rebuild` paragraph in `docs/testing.md` if it
names the round trips.
