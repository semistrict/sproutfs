---
id: TASK-104.6
title: 'Fsync journal step 6: journal disks in the membership and the orchestrator'
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 01:16'
labels:
  - durability
  - cluster
dependencies:
  - TASK-104.5
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 130000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 6 of the plan. Every host in the durable flush mode needs a journal disk of its own, and a lost host journal disk must reach another host so its entries can be read (owner decision 1). Journal disks follow the hosts, so the orchestrator creates and deletes them through the cloud API instead of keeping a pool of claims. The membership and the controller already move shards; journal disks become a second kind of disk that ranks no window.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 platform.NetworkDisks gains List, Create and Delete, in the sim with Buggify sites (create slow, create fails, delete fails) and in the Compute Engine adapter; journal disks are named sproutfs-journal-<random> and labelled with the deployment
- [ ] #2 A membership disk has a kind, a reserved machine and a deleting state; journal disks rank no window; the membership format version is bumped
- [ ] #3 Scale-up: when a node joins, the orchestrator reserves a free and empty journal disk in its zone or creates one, and attaches it while the node boots; the host is assigned the disk reserved for its machine and gets no VM until its journal is served
- [ ] #4 Scale-down: a draining host keeps its journal open until the moved VMs records no longer name it, then marks it empty and closes it; the orchestrator detaches it and the node can go; deploy/ sets a termination grace period that covers a drain and one checkpoint
- [ ] #5 Host loss: the disk is assigned to a survivor in its zone for reading and released once no record names it; a disk free and empty for an hour is deleted
- [ ] #6 Every crash point between create, membership change and delete converges, shown by tests; spec/shards models journal disks and keeps OneServer within two minutes of TLC
- [ ] #7 deploy/ and the plan list the permissions: compute.disks.create, delete, list and setLabels besides the shard permissions, and listing Kubernetes nodes; no PersistentVolumeClaims for journals
<!-- AC:END -->
