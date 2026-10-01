---
id: TASK-80
title: Keep a migration's handoff across an orchestrator restart
status: Done
assignee:
  - '@claude'
created_date: '2026-10-01 05:53'
updated_date: '2026-10-01 06:03'
labels:
  - migration
  - orchestrator
dependencies: []
priority: high
type: bug
ordinal: 87000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
spec/postcopy found this (spec/bugs.md, B2). The handoff a destination needs to take a migrated VM in lives only in the orchestrator memory. If the orchestrator crashes while a migration is handing over, a receive that fails afterwards can never be tried again, although the source still holds every page no checkpoint has. Once the row ages a survey releases the source and the VM is recovered from its last checkpoint, losing every write since, with no host lost and no hold run out.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A restarted orchestrator can retry the receive of a migration whose handoff the previous one held, for as long as the source holds the pages
- [x] #2 A test crashes the orchestrator between a failed receive and its retry, and the VM lands with the writes since its last checkpoint
- [x] #3 spec/postcopy/MCPostCopy.cfg tolerates nothing, and spec/postcopy/mutants/b2-open.cfg is removed; spec/bugs.md records B2 as fixed
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
The source keeps the handoff in its migrated hold (Host.Handed) and serves it at GET /vms/{id}/handoff. A survey now releases a migrated source only once an answering host runs the VM. One that no host runs or receives, with no in-flight row, is resumed in the background with the source's handoff (orchestrator.resume, via carry, which Migrate shares). If a host is unanswering, the survey does nothing. The migration row refresh from TASK-75 stays: it keeps a survey from resuming a handover that a live orchestrator is retrying. spec/postcopy dropped its Tolerated constant; mutants b1 and b2 put each defect back.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
B2 fixed: a migration's handoff now outlives the orchestrator. The source keeps it while it holds the pages, and a survey resumes any undriven handover instead of releasing its source. Tests: TestASourceKeepsItsHandoffWhileItHoldsThePages (host), TestAHostHandsOutTheHandoffItHolds (cmd/sproutfs-host), and TestASurveyTakesUpAHandoverNothingDrives (orchestrator; fails without the fix). spec/postcopy passes with nothing tolerated, and mutants b1 and b2 are caught. just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
