---
id: TASK-71
title: Set the checkpoint interval per VM
status: To Do
assignee: []
created_date: '2026-09-29 23:10'
labels:
  - embedder
dependencies: []
references:
  - host/host.go
  - host/losswindow.go
  - host/flush.go
  - api/host/host.go
priority: low
type: feature
ordinal: 79000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The checkpoint interval is host-wide, 60 s by default. Some of an embedder's VMs want a tighter bound on lost writes. VMs that want none already have `Ephemeral` (TASK-19).

The loss window and flush bound are derived from the interval, and the pager holds the loss window host-wide (host/host.go:97-125). A per-VM interval changes all three together.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A create or open can set a VM interval, clamped to limits the host configures
- [ ] #2 A VM's loss window and flush bound follow its own interval
- [ ] #3 A VM with no interval set behaves as today
<!-- AC:END -->
