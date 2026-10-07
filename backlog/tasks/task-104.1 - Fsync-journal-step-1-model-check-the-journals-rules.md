---
id: TASK-104.1
title: 'Fsync journal step 1: model-check the journal''s rules'
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 01:15'
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
- [ ] #1 spec/journal MCCapture checks NoLostFlush, NoRegression and that every page writable without a fault is unjournaled, over stores, flushes, captures with digests, syncs, seals, selections, abandons and a crash with a replay
- [ ] #2 MCCapture is run with digests kept across a seal for pages that were not unjournaled at it; the result is recorded in the plan, and the rule is kept only if TLC passes, otherwise every digest is dropped at every seal
- [ ] #3 spec/journal MCTakeover checks NoLostFlush, NoFencedReplay and OneWriter per disk over three hosts with migration, recovery reads that fence, a fenced host that keeps running, and disk detaches and moves
- [ ] #4 Each TLC run of the default configurations ends within two minutes; deeper ones live under spec/journal/deep for just check-spec-deep
- [ ] #5 Mutants for each rule in the plan (seal keeps the unjournaled set, unmarked protect trap, an unjournaled page keeps its digests across a seal, replay of any epoch, source journal dropped before the post-copy is done, read without fence, reused positions) are each caught by scripts/check-spec.sh
<!-- AC:END -->
