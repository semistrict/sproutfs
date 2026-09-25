---
id: TASK-9
title: Capture a running VM into a new VM that never boots
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 9000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The new VM publishes its root and is never started, so it can be forked or opened later.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 An API call captures a running VM into a new VM that publishes its root and does not boot
- [ ] #2 The source VM keeps running
<!-- AC:END -->
