---
id: TASK-20
title: Bound the fault work one VMM can cause
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - security
dependencies: []
priority: medium
type: bug
ordinal: 20000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A hostile VMM can re-fault its own memory without limit, which costs the pager CPU that other VMs need. Found by the hostile-VMM fuzzing of 2026-09-25.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A session that faults beyond its share is slowed or ended, and a neighbour's faults keep their latency
<!-- AC:END -->
