---
id: TASK-19
title: Support volumes that are never checkpointed
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 19000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. Its boxes with an ephemeral upper layer need a writable disk that no checkpoint holds.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A volume can be marked never checkpointed
- [ ] #2 Simulation invariants: it is never published, it is lost with its host, and it never reaches a fork
<!-- AC:END -->
