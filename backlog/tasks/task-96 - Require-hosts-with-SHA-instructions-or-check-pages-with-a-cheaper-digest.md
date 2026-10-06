---
id: TASK-96
title: 'Require hosts with SHA instructions, or check pages with a cheaper digest'
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
labels:
  - decision
  - performance
dependencies: []
priority: medium
ordinal: 116000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
SHA-256 is about 6 ms of a 2 MiB fault on Cascade Lake, which lacks SHA instructions, and 47% of a publication's CPU. Ice Lake and newer have them. Either the deployment requires such hosts, or pages are checked with a cheaper digest, which changes the stored format.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The owner picks one
- [ ] #2 The choice is enforced (deploy manifests or host start refuses) or implemented with a format change assessed for compatibility
<!-- AC:END -->
