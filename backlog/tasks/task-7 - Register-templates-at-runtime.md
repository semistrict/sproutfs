---
id: TASK-7
title: Register templates at runtime
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 7000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. A builder produces an ext4 image. The embedder imports it on request, and any host can then create VMs from it. This replaces the static SPROUTFS_TEMPLATES file list.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 An API call imports an image into a template named by its digest
- [ ] #2 Any host creates VMs from a template another host imported
- [ ] #3 Two hosts importing the same image at once end with one template
<!-- AC:END -->
