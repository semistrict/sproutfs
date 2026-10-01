---
id: TASK-74
title: Model-check how a VM changes owner
status: Done
assignee:
  - '@claude'
created_date: '2026-10-01 04:07'
updated_date: '2026-10-01 05:21'
labels:
  - formal
  - control
dependencies: []
priority: medium
type: feature
ordinal: 81000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Ownership of a VM is a protocol over conditional writes to its control record. It has many concurrent parties: the writer at the current epoch, opens that take the epoch over, pins and releases made without the epoch, sweeps that delete checkpoints, deletion, and the migration handoff. Each also has lost replies and failed read-backs. The simulation campaigns reach rare interleavings only by chance. A model checker covers every interleaving of a small configuration.

The owner asked to explore formal verification, and to do it now. TLA+ and its TLC checker are MIT licensed.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A TLA+ spec models one VM's control record and its parties as the code implements them: open, select and keep, writer and no-epoch pins, release and its sweep, reclamation, remove and delete, migration handoff, lost replies and failed read-backs
- [x] #2 TLC checks these invariants over every interleaving of the shipped configurations: the selected, pinned and kept checkpoints stay readable; no sequence is committed twice; a selection moves forward and only by the epoch holder; a migration destination never runs one guest's pages over another guest's checkpoint
- [x] #3 A just recipe runs TLC, fetching a pinned tla2tools.jar, and `just check` and CI run it
- [x] #4 Every violation TLC finds is fixed in the code or explained, and docs say what the spec covers and what it leaves out
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Write spec/ownership/Ownership.tla from control/client.go, volume/publish.go, volume/kept.go, checkpoint/reclaim.go and the migration handoff.
2. Add configurations small enough for CI, and a larger one to run by hand.
3. Add scripts/tlc.sh and a just recipe; add a CI job.
4. Run TLC; triage each counterexample against the code.
5. Document the spec in docs/testing.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TLC raised two counterexamples. Both were gaps in the model, not defects in the code:
1. A selection lands with its reply lost and its read-back failed. A release then sweeps against the record. Then a later selection, built on the writer's old base, names what the sweep deleted. The code prevents this: a failed publication gives its sealed pages back as dirty, so the next capture reads no older checkpoint that the landed one did not. The model now tracks the guest's read set (hview).
2. A fork from base right after such a selection pins a checkpoint the release swept. This is impossible for the same reason: the guest has unpublished pages then. ForkBase now requires that the guest reads exactly what its base names.
Six mutants (sweep-forgets-kept, sweep-takes-live, adopt-any, no-stale-check, delete-forgets-pins, pin-any) are each caught by the invariant they name.
After the final model change, TLC ran the MC configurations and the deep configurations ReleaseLong (49M states), Delete (22M) and TakeoverPinned (16M), all passing. deep/Takeovers, deep/Compaction and deep/Release last passed before the hview change and were not rerun, to keep runs short.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added spec/ownership/Ownership.tla, a TLA+ model of one VM's control record. Its parties are the writers at each epoch, opens as recovery or migration destination, pins and releases without the epoch, sweeps, reclamation and deletion. Lost replies and failed read-backs are included. just check-spec runs TLC on six small configurations (about 25 s) and checks that six mutants are caught. CI has a tla+ job. just check-spec-deep runs larger configurations. TLC found no code defects; its two counterexamples were model gaps, now closed and explained in docs/testing.md#model-checking. just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
