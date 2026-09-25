---
id: TASK-23
title: Simulate post-copy while the source keeps storing and checkpointing
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - testing
  - correctness
dependencies: []
priority: medium
type: task
ordinal: 23000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**No simulation reaches the post-copy paths where that defect was.** Two separate things were wrong there, and neither simulated campaign caught either of them. First, the pager's test double stripped the identity permanently, as the product did, so the tests modelled the retire's mistake instead of catching it. Second, with an earlier fix to `readIn` disabled, a hundred soak seeds still passed, because in every campaign the source's unpublished set only shrinks. The missing piece is a scenario in which the source keeps storing and checkpointing while a destination post-copies from it, and the destination then publishes and retires what it received. Until that scenario exists, this class of defect can only be reached on a real kernel.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A simulated scenario has the source storing and checkpointing while a destination post-copies, publishes and retires
<!-- AC:END -->
