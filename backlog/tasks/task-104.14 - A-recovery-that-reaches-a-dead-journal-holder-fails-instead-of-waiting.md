---
id: TASK-104.14
title: A recovery that reaches a dead journal holder fails instead of waiting
status: Done
assignee: []
created_date: '2026-10-07 18:56'
updated_date: '2026-10-07 19:34'
labels:
  - journal
  - recovery
dependencies: []
references:
  - host/replay.go
parent_task_id: TASK-104
priority: medium
type: bug
ordinal: 139000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found in the step 9 GCE run. A recovery started soon after a host died still found the dead host named as the journal disk's holder in the membership. JOURNAL_READ to it failed with a connection error ('no connection and hello in time'), so the open failed and the orchestrator gave up. Only ErrJournalPending makes it retry.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A JOURNAL_READ that cannot reach the membership's holder reports volume.ErrJournalPending, before or after the epoch is taken, so the orchestrator retries
- [x] #2 A host test shows an open whose journal holder does not answer fails with ErrJournalPending and succeeds once another member serves the disk
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Fixed on the step 8 agent's branch, edeb0ad9 (TestAnOpenWhoseJournalHolderCannotBeReachedIsPending); lands when that branch is merged.

Fixed by the step 8 branch's edeb0ad9, cherry-picked onto main.
<!-- SECTION:NOTES:END -->
