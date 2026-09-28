---
id: TASK-51
title: Batch the isolated arena's moves and revocations
status: To Do
assignee: []
created_date: '2026-09-26 22:14'
updated_date: '2026-09-28 00:31'
labels:
  - performance
  - deferred
dependencies: []
priority: medium
ordinal: 58000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
At 4 KiB pages, inheriting published pages in isolated mode is 5.4x slower for the children and costs 10x the host CPU (GCE worst-case run, 2026-09-26): each of 224,316 moved pages is copied, hashed and revoked one at a time, a round trip to the owner's VMM per page. The restore back onto the original host (2.2x slower start at 2 MiB) is the same cost: the populate moves every page still sitting in the guest's old private file.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Moves and their revocations are batched per window, one round trip to the owner per batch
- [ ] #2 The worst-case run's 4 KiB inherit and the 2 MiB restore-back are within 1.5x of shared
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Deferred by the owner on 2026-09-27.
<!-- SECTION:NOTES:END -->
