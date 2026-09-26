---
id: TASK-50
title: Share a fork's unpublished pages between its local children
status: To Do
assignee: []
created_date: '2026-09-26 22:14'
labels:
  - performance
dependencies: []
priority: high
ordinal: 57000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The GCE worst-case run of 2026-09-26 found, in both arena modes, that a local fork of unpublished pages shares nothing lasting: each child uploads everything it inherited (1114 MiB for 3 children of a parent that wrote 357 MiB), and when the parent's seal ends each child reads it all back from the store (about 1 GiB). Pages a fork point lends should be published once, by the parent or by one child, and inherited by identity by the rest.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A local fan-out of N children uploads each inherited page once, not N times
- [ ] #2 After the seal ends no child reads back from the store a page it already held
- [ ] #3 The worst-case run's case 1 shows the upload and read-back bytes before and after
<!-- AC:END -->
