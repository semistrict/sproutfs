---
id: TASK-77
title: Model-check orchestrator recovery against split brain
status: To Do
assignee: []
created_date: '2026-10-01 05:37'
labels:
  - formal
  - orchestrator
dependencies: []
priority: medium
type: feature
ordinal: 84000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Taking a VM over fences a writer that may still be running a guest. So the orchestrator takes an epoch only on positive evidence that the previous holder is gone, and refuses requests while two hosts report one VM (docs/metadata.md, Fencing and selection). spec/ownership assumes these rules and does not check them.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A TLA+ spec models hosts, the orchestrator table, surveys, host loss and pod listing, recovery, migration and drain
- [ ] #2 TLC checks that at most one host runs the guest of a VM, except for a fenced host that can never publish, and that recovery never opens a VM while its holder can still run it
- [ ] #3 Small configurations run in just check-spec within a few minutes, with mutants; real defects are recorded in spec/bugs.md
<!-- AC:END -->
