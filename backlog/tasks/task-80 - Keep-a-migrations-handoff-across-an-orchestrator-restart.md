---
id: TASK-80
title: Keep a migration's handoff across an orchestrator restart
status: To Do
assignee: []
created_date: '2026-10-01 05:53'
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
- [ ] #1 A restarted orchestrator can retry the receive of a migration whose handoff the previous one held, for as long as the source holds the pages
- [ ] #2 A test crashes the orchestrator between a failed receive and its retry, and the VM lands with the writes since its last checkpoint
- [ ] #3 spec/postcopy/MCPostCopy.cfg tolerates nothing, and spec/postcopy/mutants/b2-open.cfg is removed; spec/bugs.md records B2 as fixed
<!-- AC:END -->
