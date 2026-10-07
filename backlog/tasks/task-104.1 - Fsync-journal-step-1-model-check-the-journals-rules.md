---
id: TASK-104.1
title: 'Fsync journal step 1: model-check the journal''s rules'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 03:35'
labels:
  - durability
  - spec
dependencies: []
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 125000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 1 of the plan. The journal rests on rules that are easy to get wrong: which pages a capture takes, what a seal and an abandoned seal do to them and to their SHA-256 digests, which position a checkpoint covers, which entries a replay may apply, and how a read fences a host that is still alive. TLC checks them before any code depends on them. The owner decided that digests survive a seal only if MCCapture passes with that rule. Specs are factored by concern so each run ends within two minutes.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 spec/journal MCCapture checks NoLostFlush, NoRegression and that every page writable without a fault is unjournaled, over stores, flushes, captures with digests, syncs, seals, selections, abandons and a crash with a replay
- [x] #2 MCCapture is run with digests kept across a seal for pages that were not unjournaled at it; the result is recorded in the plan, and the rule is kept only if TLC passes, otherwise every digest is dropped at every seal
- [x] #3 spec/journal MCTakeover checks NoLostFlush, NoFencedReplay and OneWriter per disk over three hosts with migration, recovery reads that fence, a fenced host that keeps running, and disk detaches and moves
- [x] #4 Each TLC run of the default configurations ends within two minutes; deeper ones live under spec/journal/deep for just check-spec-deep
- [x] #5 Mutants for each rule in the plan (seal keeps the unjournaled set, unmarked protect trap, an unjournaled page keeps its digests across a seal, replay of any epoch, source journal dropped before the post-copy is done, read without fence, reused positions) are each caught by scripts/check-spec.sh
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. spec/journal with two modules, one per concern: Capture.tla (pager capture, digests, seal, selection, abandon, failed batches, positions, replay) and Takeover.tla (epochs, journals named in the record, migration with post-copy, recovery reads that fence, crashes, detaches and moves).
2. Let scripts/check-spec.sh check a directory of several modules: each configuration names its module in a '\* module: <name>' line.
3. MCCapture with digests kept across a seal for pages that were not unjournaled; a deep configuration with every digest dropped for comparison.
4. MCTakeover with three hosts and all features; deep configurations that grow one dimension each.
5. Mutants for each rule the plan lists, each caught by check-spec.
6. Time every TLC run; keep each within about two minutes.
7. Record the digest answer in the plan's Decided item 2 and here.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Digests across a seal: they survive. A page that was not unjournaled at a seal keeps its digests; one that was loses them. MCCapture (keep) and the deep configurations pass NoLostFlush, NoRegression, WritableIsUnjournaled and DigestsDescribeTheJournal. deep/CaptureDigestsDropped.cfg (drop every digest at every seal) passes too. Keeping the digests of an unjournaled page as well fails NoLostFlush (mutants/unjournaled-keeps-digests.cfg). Recorded in the plan's Decided item 2.

Defect found in the plan, B6 in spec/bugs.md: a failed write or sync left the batch's pages journaled with digests that describe an entry that may not be on the disk, so a later flush that succeeds does not cover the stores the failed one was sent after. Fixed in the plan (Failures table, step 4): a failed batch gives its pages back as unjournaled without digests. The digest rule holds only with this fix. Mutants failed-keeps-pages and failed-keeps-digests.

Model choices: a block's bytes are x or y and every store flips them. With values that only grow, as the plan first proposed, a digest kept too long never matches and the unjournaled-keeps-digests mutant passes. Invariants are judged in every state against the replay a crash there would start. Takeover abstracts blocks and digests (Capture checks them); a running instance seals and selects in one step, a destination in its post-copy may seal and select later; a holder that answers a read gives up any instance of an older epoch, whose in-flight write may still land; crashes and detaches are limited to hosts and disks a replay needs.

scripts/check-spec.sh: a spec directory may hold one module per concern; each configuration then names its module in a '\* module: <name>' line.

Mutants (each caught by scripts/check-spec.sh): seal-drops-unjournaled (the plan's rule that the seal keeps the unjournaled set; a seal that also leaves the pages in the region's set only takes more and passes, checked) by NoLostFlush; trap-not-marked by WritableIsUnjournaled (also NoLostFlush when that invariant is off); unjournaled-keeps-digests by NoLostFlush; reuse-positions by NoRegression (the failed entry lands after the next seal's covered position and replays over newer bytes); replay-any-epoch by NoFencedReplay (a fenced host's late entry on a disk the VM later comes back to); drop-source-early by NoLostFlush; read-no-fence by NoLostFlush; plus failed-keeps-pages and failed-keeps-digests (B6) by NoLostFlush, and multi-attach by OneWriter (the trusted single attach).

TLC times on the Mac, 15 workers. Quick, from just check: MCCapture 3,661,884 states 22s; MCTakeover 5,000,658 states 37s; each mutant 0-2s. Deep, timed alone: CaptureOnePage 8,586,196 states 35s; CaptureThreeBlocks 3,168,478 states 22s; CaptureTwoPages 13,351,706 states 51s; CaptureDigestsDropped 2,967,647 states 18s; TakeoverRecoveries 6,730,020 states 46s; TakeoverMigrations 6,612,666 states 52s; TakeoverPostCopy 4,707,331 states 39s. just check exit 0.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added spec/journal: Capture.tla (MCCapture) and Takeover.tla (MCTakeover), seven deep configurations and eleven mutants, each caught by scripts/check-spec.sh, which now lets a configuration name its module. Answer to the open question: a page that was not unjournaled at a seal keeps its digests; recorded in the plan's Decided item 2. Found B6, a failed batch that left its pages journaled; the plan now gives them back unjournaled without digests. Every TLC run ends within a minute; just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
