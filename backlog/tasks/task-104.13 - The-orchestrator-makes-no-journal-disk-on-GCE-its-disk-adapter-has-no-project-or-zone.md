---
id: TASK-104.13
title: >-
  The orchestrator makes no journal disk on GCE: its disk adapter has no project
  or zone
status: Done
assignee: []
created_date: '2026-10-07 18:56'
updated_date: '2026-10-07 19:33'
labels:
  - journal
  - gce
dependencies: []
references:
  - platform/internal/real/gcedisks.go
  - cmd/sproutfs-orchestrator/main.go
parent_task_id: TASK-104
priority: high
type: bug
ordinal: 138000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found in the step 9 GCE run. The orchestrator builds the Compute Engine adapter with an empty config. Journal disks are named without a project or zone, so List and Create fail on every pass, and durable flush never gets a journal disk. The GCE agent's branch has a fix, 0b7ecae1: it reads the project and zone from the metadata server when they are not set. It is not merged yet.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The GCE adapter fills the project and zone from the metadata server when the config leaves them empty, and a test against the fake API shows it
- [x] #2 With SPROUTFS_DURABLE_FLUSH=gce on GCE, the orchestrator creates and lists journal disks
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Fixed in 13d9b790 (cherry-picked from the step 9 branch): TestGCEDisksTakeTheInstancesProjectAndZoneFromTheMetadataServer; the step 9 GCE run made, listed and deleted journal disks with it.
<!-- SECTION:NOTES:END -->
