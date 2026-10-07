---
id: TASK-104.5
title: 'Fsync journal step 5: the host answers flushes from its journal'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 17:00'
labels:
  - durability
  - host
dependencies:
  - TASK-104.2
  - TASK-104.3
  - TASK-104.4
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 129000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 5 of the plan. Durable flush is an optional mode, SPROUTFS_DURABLE_FLUSH, off by default (owner decision 3). Off, the flush bound works as today. On, a flush waits for its entry on the journal disk, and a flush that cannot be journaled fails with EIO; there is no fallback to a checkpoint. The host opens its journal disk like a shard, captures and batches flushes, and records the covered position at each seal.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 With the mode off, the flush path and its tests are unchanged and no journal disk is opened
- [x] #2 With the mode on, a flush is answered only after the batch holding its entry has synced; a flush that cannot be journaled (no journal served, a failed write or sync, a detached disk) fails with EIO, and the next batch pads the failed range
- [x] #3 A full ring is back-pressure: past three quarters the host asks for checkpoints out of turn, a VM over half the ring waits for its own, and a full ring holds captures until trimming frees space
- [x] #4 The seal records the VM last captured position and the selection writes it as the covered position
- [x] #5 JOURNAL_READ is served by the disk holder, which first fences the VM at the reader epoch and gives the VM up if it runs it
- [x] #6 Status and metrics report durable_flush (whether the mode is on), flush latency, hashing time, live bytes and failed flushes; guards journal-covered-after-seal, journal-read-without-fence, journal-full-answers and journal-failed-write-answers are killed by their tests
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. journal: Hooks on a commit (Placed with its positions, Failed when it does not land), run on the writer before the next batch; Identity().
2. volume: Terms.Cover (JournalCover) whose Journals the selection writes and whose Selected the record names afterwards.
3. host: JournalConfig.DurableFlush; SetJournal; per-VM capture numbering under the VM journal lock; a flush captures its region's unjournaled and in-flight pages as one commit; EIO on failure, pages given back through the Failed hook.
4. Covered position: the pause takes the VM journal lock after sealing and records the capture count; the selection waits for those captures to be placed and names their highest position; Selected trims.
5. Trimming loop over the records of the VMs the journal holds; back-pressure (half ring per VM, three quarters asks checkpoints, also before a commit that would not fit).
6. JOURNAL_READ in the peer protocol; the holder fences, reads and gives the VM up.
7. Status and metrics; tests and guards.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
From TASK-104.2: the guard journal-read-without-fence already exists in package journal (Read fences); reuse it, do not add a second. Commit takes the room a capture may use before the capture runs, so the pager capture (104.4) must fit a room given in advance. Trimmed ring space is reusable only once the tail hint is on disk (once a second), so a full ring can wait up to a second longer.

Built: journal Hooks (CommitHooked), volume.Terms.Cover, host/journal.go (flush path, cover, trimming, back-pressure, ReadJournal), peer JOURNAL_READ (peer/journal.go, JournalRead/JournalEntries), hostapi.Journal and its metrics.
Design point the plan left open: the covered position. The pause records how many captures of the VM had begun, taking the VM journal lock after its disks are sealed and before the guest runs; a capture holds that lock for its whole region capture. Captures during the pause read sealed bytes, so either side of the boundary is safe. The selection waits until those captures are placed (journal.Hooks.Placed) and uses their highest position.
A flush covers its region's unjournaled pages and the pages of captures still in flight, so a flush that finds nothing new still covers an earlier commit that fails.
Back-pressure gap found by the half-ring guard: a commit the ring has no room for waited forever when nothing asked for a checkpoint; the host now asks before a commit that would pass three quarters, as well as after.
JOURNAL_READ gives the VM up before it answers (synchronously), so the reader knows the old instance stopped.
SPROUTFS_DURABLE_FLUSH is not read yet: turned on with no journal disk served, every flush would fail. Step 6 wires it with the journal disks.
Tests: host/journal_test.go (answered after the entry, no journal fails, failed sync fails and is taken again, selection names the covered position and trims, half ring waits for its checkpoint, JOURNAL_READ fences/answers/gives up/refuses other generations and disks), host/journal_internal_test.go (covered position), journal TestACommitsHooksRunBeforeTheNextBatch, metrics golden.
Guards: journal-failed-write-answers, journal-failed-write-keeps-pages, journal-covered-after-seal, journal-ignore-half-ring, journal-read-keeps-vm (host), journal-read-without-fence (journal, from 104.2).

Done in 71f5cfab. The full-ring guard is journal-answer-before-sync in journal/; a full ring is back-pressure in the journal itself. Later changes: a flush is journaled only once the record names this host's journal at the VM's epoch (860ee6f7).
<!-- SECTION:NOTES:END -->
