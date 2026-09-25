---
id: TASK-8
title: Create a VM from a stopped VM's published checkpoint
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 8000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The host API needs to expose volume.Manager.Inherit, and the pin has to work when no host runs the parent.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 An API call creates a VM from another VM's published checkpoint
- [ ] #2 It works when no host runs the parent, and the checkpoint is pinned
- [ ] #3 It refuses a checkpoint of another tenant
<!-- AC:END -->
