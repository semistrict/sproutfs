---
id: TASK-50
title: Share a fork's unpublished pages between its local children
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-26 22:14'
updated_date: '2026-09-27 22:07'
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
- [x] #1 A local fan-out of N children uploads each inherited page once, not N times
- [x] #2 After the seal ends no child reads back from the store a page it already held
- [ ] #3 The worst-case run's case 1 shows the upload and read-back bytes before and after
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. A fork point is published once, by the parent, as a checkpoint of its sealed pages (a capture's publication), while its children start at once from the parent's pages as now.
2. A child's root index is built on the point's published checkpoint, so it uploads only what the child wrote; a child's first publication waits for the point's to land.
3. When the point's publication lands, the pages it lent get that checkpoint's identity, so children keep them and read nothing back after the seal ends.
4. If the point cannot be published, children publish what they inherited themselves, as today.
5. Tests: volume/host/sim tests counting uploads (N children upload each inherited page once) and read-backs (none after the seal), then the GCE worst-case run's case 1 before/after.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Implemented: volume.ForkPoint lands a point with no unpublished pages at once on the checkpoint it inherits, and otherwise publishes the point in the background (publishPoint: publish under the sequence the point took, pin it, install it; the seal stays until the last holder). A child's root waits for it (awaitPoint, before any pause) and builds on the published index with no inherited pages; the last holder retires the seal as published, so lent pages become clean under the identity they were lent under. A failed point publication leaves children to publish what they inherited (the old path). host.VolumeConfig.PointPublished feeds the simulation's durable-state model. Proven: volume TestAFanOutUploadsTheParentsUnpublishedPagesOnce (3 children upload no part; mutation-checked), TestAForkPointIsPublishedOnceByTheParent, host TestLocalForkReceivesTheForkPointOverThePages now checks no load after the seal ends (mutation-checked), simulation incl. crash campaigns, probe build. AC3 (GCE worst-case case 1 before/after) pending. TASK-62 takes the child's root off the fork's critical path.
<!-- SECTION:NOTES:END -->
