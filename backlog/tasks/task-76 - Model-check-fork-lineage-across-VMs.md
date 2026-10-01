---
id: TASK-76
title: Model-check fork lineage across VMs
status: Done
assignee:
  - '@claude'
created_date: '2026-10-01 05:37'
updated_date: '2026-10-01 06:28'
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
- [x] #1 A TLA+ spec models several VMs: forks with pins, fork points published behind their children, child roots, parent deletion and sweeps
- [x] #2 TLC checks that no descendant ever reads a deleted checkpoint
- [x] #3 The spec includes a candidate collector design for TASK-24, and TLC checks it against the same invariant
- [x] #4 Small configurations run in just check-spec within a few minutes, with mutants; real defects are recorded in spec/bugs.md
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
No real code defect found: the code today keeps every checkpoint a descendant reads (MCLineage, 38,972 states, MaxCkpts 1). MaxCkpts 2 ran past three minutes and is left out to keep runs short. The candidate collector is checked. Two wrong designs are kept as mutants: naive-collector and holders-judged-late. The constraints the passing design meets are recorded on TASK-24. Code mutants sweep-other-vms and delete-forgets-pins are caught.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added spec/lineage/Lineage.tla: three VMs in a lineage, with forks that pin and then create the child, child roots naming the parent's checkpoints, per-VM sweeps, and deletes. The invariant NoDanglingRead holds for the code today. It also models a candidate pin collector for TASK-24. TLC showed that a naive collector, and one that judges holders late, delete what a fork in flight reads; the design constraints are on TASK-24. Four mutants are caught. just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
