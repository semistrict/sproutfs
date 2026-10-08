---
id: TASK-112
title: Keep a guest fault alive through an object store refusal
status: To Do
assignee: []
created_date: '2026-10-08 14:47'
labels:
  - pager
  - store
dependencies: []
priority: high
type: bug
ordinal: 151000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A guest fault whose page must come from the object store fails when the store refuses the read: checkpoint.Store.readObject passes platform.ErrUnavailable up, volume.Load passes it to the pager, MemoryRegion.Fault returns it, and the connection loop (vmmemory/connection_linux.go, the c.fail after Fault) ends the memory region session, which kills the VM. Nothing between the store and the guest makes the read again except the provider client (the S3 SDK tries three times) and the bounded store, which retries only timeouts. A burst of 503 SlowDown longer than the client tries is enough. Found while writing scripts/faults/platform.json: the checkpoint cluster read campaign, with the store refusing reads at random (sim/object-store/unavailable/get, sim/object-store/body-fails), fails its reads. The fix touches vmmemory, volume or checkpoint.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A guest fault whose load the store refuses, or whose body resets partway, is served once the store answers, and the VM session does not end
- [ ] #2 A regression test fails with a sim.Bug guard that restores the old behaviour, and the guard is in scripts/mutation/guards.json
<!-- AC:END -->
