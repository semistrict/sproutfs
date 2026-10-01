---
id: TASK-76
title: Model-check fork lineage across VMs
status: To Do
assignee: []
created_date: '2026-10-01 05:37'
labels:
  - formal
  - fork
dependencies: []
priority: medium
type: feature
ordinal: 83000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A fork child reads through its parent checkpoints, and a grandchild names its grandparent checkpoints directly, which no record shows (docs/metadata.md). Pins are permanent because no single party can tell whether a pin is still needed. spec/ownership models one VM, so it cannot check that a descendant never reads a deleted checkpoint. The pin collector (TASK-24) is the planned way to release pins, and it must be correct before it is built.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A TLA+ spec models several VMs: forks with pins, fork points published behind their children, child roots, parent deletion and sweeps
- [ ] #2 TLC checks that no descendant ever reads a deleted checkpoint
- [ ] #3 The spec includes a candidate collector design for TASK-24, and TLC checks it against the same invariant
- [ ] #4 Small configurations run in just check-spec within a few minutes, with mutants; real defects are recorded in spec/bugs.md
<!-- AC:END -->
