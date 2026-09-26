---
id: TASK-49
title: Map a moved page read-only to its owner so a move saves memory
status: To Do
assignee: []
created_date: '2026-09-26 22:14'
labels:
  - performance
  - security
dependencies: []
priority: high
ordinal: 56000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
In the isolated arena, a published page another region inherits is moved into the shared file and the owner's mapping is revoked (vmmemory/isolation.go, move). The GCE worst-case run of 2026-09-26 (docs/measurements/arena-worst-case-2026-09-26.md) found that the owner's next access to each moved page arrives as a store and makes a private copy: 403 copies for 428 moves. So a move saves no memory (812 MiB saved against 1248 MiB shared), and at 4 KiB the host pod reached 7985 MiB of its 8 GiB. Why the fault arrives as a store is not known.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The owner's first read of a moved page maps the shared copy and makes no private copy
- [ ] #2 A pager test and a Lima test count zero copies after a move followed by owner reads
- [ ] #3 The worst-case run's saved memory in isolated mode matches shared mode within 5%
<!-- AC:END -->
