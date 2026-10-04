---
id: TASK-91
title: Start a fork's child without writing to the object store before it runs
status: To Do
assignee: []
created_date: '2026-10-04 22:05'
labels:
  - performance
  - fork
dependencies: []
references:
  - docs/measurements/gce-start-latency-2026-10-04.md
priority: medium
ordinal: 97000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A fork's child reaches running in 342 ms on the parent's host and 375 ms on another host (docs/measurements/gce-start-latency-2026-10-04.md). The store is 78 ms and 107 ms of that at the median, 111 ms and 165 ms at p99: the parent's confirm (a read of its control record, 29 ms), on another host the read of the parent checkpoint's index (29 ms), and the child's control record created if absent (50 ms). The child exists only on its host until its root publishes, so its record could be written behind the running child, as its root is. The confirm is what stops a fenced host from handing out pages no checkpoint holds (host/fork.go, confirmHandoff), so it can move off the path only if a failed confirm still discards the child before anything outside its host can see it.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A fork's child runs before any object-store write of the fork lands, on the parent's host and on another host
- [ ] #2 A fork from a host that a later writer fenced still starts no child that survives, shown by a simulation test
- [ ] #3 A GCE run of cmd/sproutfs-startbench shows the fork cases' time to running and their store share before and after
<!-- AC:END -->
