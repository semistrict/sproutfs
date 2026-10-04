---
id: TASK-88
title: 'Measure how long a start, a restore and a fork take until the guest runs'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-04 19:21'
updated_date: '2026-10-04 22:05'
labels:
  - measurement
dependencies: []
references:
  - 'https://pinggy.io/amp/blog/edge_functions_isolates_to_microvms/'
priority: medium
type: task
ordinal: 95000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
No report records the time from a start, a restore or a fork request to the guest's first instruction, or where that time goes. A start and a fork write to the object store on their path (the control record's create and the epoch's advance), which on GCS or S3 is tens of milliseconds; Netlify's microVMs cold start in about 9 ms with no remote write. Before deciding whether a VM that is never recorded or published is worth adding, we need the numbers.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A dated report in docs/measurements gives p50/p99 from request to first guest instruction for a cold start, a restore from a checkpoint, a fork on the same host and a fork to another host, on GCE
- [x] #2 The report splits each into its steps (control record writes, root read, VMM restore, first faults) and names the largest
- [x] #3 The report says what a fork that writes nothing to the store would save, and the backlog has a task for it if the saving is worth it
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Record each start's steps on the host: a per-request timeline carried in the context (host), the object-store calls it made (platform.MeteredObjectStore into a per-context trace), the VMM start phases, the resume, and one log line per start ('host: a VM runs') with offsets in ms.
2. Count a memory region's guest faults (vmmemory) and log the first second of faults after the guest runs.
3. Add cmd/sproutfs-startbench: drive cold start, restore, fork on the same host and fork to another host through the host API, time the request and the guest agent's answer, time GCS conditional writes directly, and summarise with the host's log lines into p50/p99 per step.
4. Add scripts/bench-start-gce.sh (two Ice Lake nodes, k3s, deploy/) modelled on bench-app-restore-gce.sh.
5. Run on GCE with hundreds of repetitions per case; delete everything and verify.
6. Write docs/measurements/gce-start-latency-<date>.md; create a task for a store-free fork if the saving is worth it.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Ran scripts/bench-start-gce.sh on two n2-highmem-4 Ice Lake nodes, 400 starts per case, all succeeded; raw results in docs/measurements/gce-start-latency-2026-10-04/ (ignored). To running p50/p99: cold 476/514, restore 450/7337 (448/575 without the 56 throttled epoch writes), fork-local 342/380, fork-remote 375/439 ms. Largest step: root publication for cold (184 ms), VMM state load for the rest (226-242 ms, of which populate 170-190 ms). Store share 23-45 %. GCS direct: create-if-absent 46.5/65.7, compare-and-set 52.5/76.3 ms. A store-free fork saves 78 ms local and 107 ms remote at p50: TASK-89. Nodes, disks and objects deleted and verified. check-guards' migration-take-a-claim-after-close guard survived twice under load on this Mac (vmmigrate unchanged) and passed alone.

The store-free fork task is TASK-91; TASK-89 above was renumbered at merge because TASK-89 and TASK-90 were taken by archived tasks.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Each create, open, fork and receive now logs its steps, object-store calls and attaches as 'host: a VM runs' / 'host: a VM forked', and its first faults a second later (host/timeline.go, platform/objecttrace.go, vmmemory/guestfaults.go). cmd/sproutfs-startbench and scripts/bench-start-gce.sh drive the four cases and summarise them. The report docs/measurements/gce-start-latency-2026-10-04.md gives p50/p99 per case and step from 1,600 GCE starts; TASK-89 tracks the store-free fork. Verified with unit and synctest tests of every new piece, just check, and the GCE run.
<!-- SECTION:FINAL_SUMMARY:END -->
