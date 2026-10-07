---
id: TASK-104.2
title: 'Fsync journal step 2: the journal format and its writer'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 02:25'
labels:
  - durability
  - storage
dependencies: []
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 126000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 2 of the plan. A host needs a write-ahead journal on a network disk that it can append flushed blocks to, read back after a crash, serve to other hosts, and trim. This step builds it as a new package over platform.File, with no caller yet.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Package journal writes and reads the format in the plan: two header slots with lease and tail hint, entries with XXH3-128, a ring with pads, positions that only grow
- [x] #2 A group-commit writer keeps one batch in flight and answers its flushes only after the batch syncs, in position order
- [x] #3 Over platform/sim disks with PowerLossFaults, every entry answered before a power loss reads back, a torn batch ends the read, and a failed write range is padded so nothing after it is lost
- [x] #4 A lease of a newer assignment refuses the disk; trimming by covered positions frees the ring
- [x] #5 Format fixtures under journal/testdata, a fuzz test of the parser, and a sim.Bug guard journal-answer-before-sync killed by its tests
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. journal/format.go: header slot and entry encoding as the plan lays out, XXH3-128, EntryBytes.
2. journal/journal.go: Open (two slots, format a blank disk, refuse another identity or version, refuse a newer lease, take the lease, read back from the tail hint), CheckLease, Close (writes empty when no live entry), Held, Usage.
3. journal/writer.go: Commit with a capture the writer calls as it forms the batch; one batch in flight; up to 8 MiB of room a batch; pads over a failed range, at the ring's end and to 4 KiB; answers after the sync; captures wait for room past the durable tail hint.
4. journal/read.go: Read, the server side of JOURNAL_READ: fences the VM at the reader's epoch, waits for batches placed before, streams the entries after the covered position.
5. journal/trim.go: Trim by covered positions; the tail hint written at most once a second.
6. Tests in synctest over platform/sim disks: power loss, torn batch, padded failed range, positions, lease, read fence, trimming; a Buggify campaign with probes; fixtures under journal/testdata; fuzz of the parsers; guard journal-answer-before-sync in scripts/mutation/guards.json; journal in the determinism rule.
7. Gremlins before and after on journal; just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Built package journal: format.go (two 4 KiB header slots, entries, XXH3-128), journal.go (Open: format a blank disk, refuse another version by name, another identity, a ring that does not fit, a newer lease; take the lease, then read back from the tail hint; CheckLease; Close writes empty when no entry is live; Held, Usage), writer.go (Commit with a Capture the writer calls as it forms the batch; one batch in flight; up to 8 MiB of room a batch; pads over a failed batch, at the ring's end and to 4 KiB; answers after the sync), read.go (Read, the server side of JOURNAL_READ: fences the VM at the reader's epoch, waits for batches placed before, streams entries after the covered position), trim.go (Trim by the epochs and covered positions a control record names).

Decisions and differences from the plan:
- A failed batch is padded over from where the synced prefix ends (written) to the next position given out (next), in the next batch's write. No read back is needed for that; the plan said the journal reopens and reads back.
- The writer may use the ring only up to the tail hint on the disk plus the ring's length, not the tail in memory: reading back starts at the hint, so overwriting a position at or after it would lose everything after. Space a trim frees is reusable only once the hint reaches the header, at most a second later.
- A commit names its room, the most bytes its capture's entries may take, before the capture runs. The writer reserves room plus the largest commit's room plus 4 KiB and two pad heads, which covers a pad at the ring's end and the pad to 4 KiB. Step 4's capture must bound its entries by a room known before it runs.
- The first position of a new journal is the ring's length, so no position is zero and a covered position of zero covers nothing.
- After a crash the next holder gives out positions from the head it read back, as the plan allows (positions are never reused by the process that gave them out). An unanswered entry of the crashed holder may later read back if a new batch ends where it begins; it holds stores made after the last answered flush.
- The VM fence lives here: Read fences, a commit with an entry of an older epoch fails with ErrFenced. Its guard journal-read-without-fence is registered here; step 5 lists a guard of that name and should reuse it.
- The writer reads the header before every tail hint write, so a holder whose lease was taken stops instead of writing over the newer lease.
- docs/testing.md was not changed (out of scope for this agent): its tables of sites, probes and guards need the journal's (step 10).

Tests (synctest over platform/sim disks): an answered entry reads back after a power loss with FullCorruption; a torn batch ends the read; a failed write's and a failed sync's range is padded and nothing after it is lost, positions continue past it; a newer lease refuses the disk at open and while open; a commit is answered only after its batch's sync, one batch in flight, commits grouped; a commit whose sync a power loss interrupts is not answered; a read fences and waits for the batch in flight; trimming frees the ring, the tail hint is written a second after the header and not before; the exact layout of pads at the ring's end and to 4 KiB; a batch never writes over the durable tail at the tightest reservation; batch room limit; a commit given up is never captured; fixture testdata/journal-1/device; version refused by name; fuzz of entry and slot parsers; campaign TestAJournalKeepsEveryAnsweredEntryThroughItsFaults over 24 seeds with Buggify on, power losses at seeded points, 4 holders and a reader: every site fired and 7 probes reached. Guards journal-answer-before-sync and journal-read-without-fence are killed (check-guards --repeat 5). journal added to the determinism rule.

Gremlins (scripts/mutate-gremlins.py --package journal --suite full, gremlins at ~/go/bin/gremlins):
- Before the tests for survivors: killed 205, lived 41, not covered 36, timed out 5 (audit: 205 test failures, 41 passed with tests, 5 timeouts; no compile errors counted as kills).
- After: killed 238, lived 5, not covered 36, timed out 5 (audit: 238 test failures). Tests added for survivors: the exact layout of pads (gap 0, 64, 56+4096; an entry 8 bytes over or under the room before the ring's end; one exactly as long; one 64 short), the tightest reservation before the durable tail, the batch room limit and a commit larger than a batch, a cancelled commit never captured, a read after the covered position, reader epoch and generation checks, formatting an uneven disk and the smallest one, a torn header write, the tail hint written one second after the header, read back in windows (3 reads), pad lengths, the largest entry, non-zero name padding, entries the format cannot hold.
- The 5 survivors change nothing a run can see: format.go:134 (a slot is always 4 KiB), journal.go:368 (a full lap stops at the position check either way), read.go:56 (sets the same fence), writer.go:232 (both piece lengths of a failed range over 16 MiB are valid pads), writer.go:102 (a log line).
- Timeouts: Open and writeHeader inverting their error checks, the tail hint's due check at exactly one second, the write loop's condition: each hangs the tests.
- Not covered: constant declarations, switch case expressions (Gremlins attributes no coverage to them; the identity, geometry and lease cases of Open are run by tests), the guard's own branch, and a failed range over 16 MiB.

Validation: go test ./journal (and -race) pass; check-guards kills journal-answer-before-sync and journal-read-without-fence in 5 of 5 runs; fuzz of both parsers ran 20 s and 10 s clean; just check exit 0.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added package journal: the journal disk format (two header slots with lease, tail hint and empty flag; entries and pads with XXH3-128; a ring whose positions only grow), a group-commit writer with one batch in flight that answers commits only after the batch syncs, padding over failed batches, reading back from the tail hint, Read as the server side of JOURNAL_READ with its VM fence, and Trim by covered positions. Nothing outside the package uses it. Verified with synctest tests over platform/sim disks with PowerLossFaults, a 24-seed Buggify campaign reaching every site and probe it registers, the fixture journal/testdata/journal-1/device, fuzz tests of the entry and slot parsers, guards journal-answer-before-sync and journal-read-without-fence, Gremlins 205 killed / 41 lived before and 238 / 5 (equivalent) after, and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
