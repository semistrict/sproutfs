---
id: TASK-42
title: Keep a VM's pull mark through a recovery
status: To Do
assignee: []
created_date: '2026-09-26 16:40'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 49000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A VM can be marked to pull its whole memory to local disk when it starts (TASK-37). The orchestrator does not store the mark, so a recovery after a host loss opens the VM without it, and the VM reads the store page by page for the rest of its life on the new host.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The orchestrator records the pull mark with the VM and every open it drives, recovery included, carries it
- [ ] #2 A test recovers a marked VM and its new host pulls
<!-- AC:END -->
