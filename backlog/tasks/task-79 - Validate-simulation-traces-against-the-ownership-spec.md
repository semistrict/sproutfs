---
id: TASK-79
title: Validate simulation traces against the ownership spec
status: To Do
assignee: []
created_date: '2026-10-01 05:37'
labels:
  - formal
  - simulation
dependencies: []
priority: medium
type: feature
ordinal: 86000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
spec/ownership is written from the code by hand, so it can drift from the code without anything failing. The simulation in internal/simtest records traces of what the real code did. Checking those traces against the spec ties the model to the code: a behaviour the code shows and the spec forbids is either a bug or a model gap.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A simulation campaign emits the control-record events that the spec names
- [ ] #2 A checker accepts each trace only if the spec allows it, and runs in CI within a few minutes
- [ ] #3 A trace the spec rejects is triaged, and real defects are recorded in spec/bugs.md
<!-- AC:END -->
