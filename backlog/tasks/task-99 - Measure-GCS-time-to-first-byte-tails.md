---
id: TASK-99
title: Measure GCS time-to-first-byte tails
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
labels:
  - measurement
dependencies: []
priority: low
ordinal: 119000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The store requests' 10 s first-byte and stall limits (platform/bounded) were set without measuring GCS's own tails on GCE. A budgeted first-byte hedge of ranged GETs may or may not be worth adding.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A dated report gives GCS first-byte p50/p99/p99.9 for ranged GETs from GCE in the bucket's region
- [ ] #2 The limits are confirmed or changed, and a hedge is added or ruled out
<!-- AC:END -->
