---
id: TASK-14
title: Retry a failed migration receive while the source holds the pages
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
  - correctness
dependencies: []
priority: high
type: feature
ordinal: 14000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs.

**A migration tries the receive once. If the receive fails, the guest loses its writes since its last checkpoint.** The source stops the guest and gives up its volumes before the destination is asked to take the VM. The source keeps the pages no checkpoint holds until it is told that the destination has all of them. When the destination cannot take the VM, the orchestrator records the VM as stopped and reports the failure. Nothing retries the same destination, or another one, while the source still holds those pages (`cmd/sproutfs-orchestrator/orchestrator.go`). The VM reopens from its last checkpoint, and the source's own four-interval hold deadline retires the pages. A drain performs one such migration per VM. So a host that leaves while a destination is briefly unreachable takes the guest's unpublished writes with it. The simulated swizzle campaign shows that a retry is sound, because the source's handoff stays valid for as long as the source holds it. The campaign also shows that the first attempt does fail under a separated link (`internal/simtest/swizzle_test.go`). The nightly seed sweep found this on 2026-09-18. Its seeds 21 and 227 had been passing only because the world reported a VM as on a host that a failed takeover had only attempted.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A failed receive is retried on the same or another destination while the source holds the pages
- [ ] #2 The swizzle campaign passes with retries
<!-- AC:END -->
