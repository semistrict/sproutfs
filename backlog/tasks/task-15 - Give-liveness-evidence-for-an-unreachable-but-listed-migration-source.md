---
id: TASK-15
title: Give liveness evidence for an unreachable but listed migration source
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
  - correctness
dependencies: []
priority: medium
type: feature
ordinal: 15000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs.

**A migration whose source host is unreachable but still listed waits for that host.** A destination asks its source for the pages no checkpoint holds until they arrive. The orchestrator ends the migration only on positive evidence that the source is gone: the Kubernetes API no longer lists the pod, or the pod answers and neither runs the VM nor serves its pages (`cmd/sproutfs-orchestrator/orchestrator.go`). If the source host's process is alive, the migration resolves either way. The source's own handover deadline of four checkpoint intervals gives the pages up, and the source's next answer stops the asking. But a host that is unreachable while its pod is still listed does neither. The migration then stays in flight for as long as the request driving it lives. The rule exists to avoid guessing, so the open item is better evidence, not a timeout. The orchestrator has no liveness signal for a pod it cannot reach, other than the Kubernetes API's own.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A migration whose source is unreachable but listed ends on evidence, not a timeout
<!-- AC:END -->
