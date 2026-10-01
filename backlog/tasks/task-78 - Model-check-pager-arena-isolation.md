---
id: TASK-78
title: Model-check pager arena isolation
status: To Do
assignee: []
created_date: '2026-10-01 05:37'
labels:
  - formal
  - security
dependencies: []
priority: low
type: feature
ordinal: 85000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The pager hands each VMM files: a private file, its tenant shared file, the public file and fork files. It moves and revokes pages between them (docs/vm-memory.md). A VMM runs untrusted guest code, so the safety property is that no VMM can ever map a page of another tenant, a property the hostile-VMM tests sample but do not exhaust.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A TLA+ spec models the arena files, sharing joins and leaves, moves, revocations and the public file
- [ ] #2 TLC checks that no VMM maps a page of a VM of another tenant, except a public template page
- [ ] #3 Small configurations run in just check-spec within a few minutes, with mutants; real defects are recorded in spec/bugs.md
<!-- AC:END -->
