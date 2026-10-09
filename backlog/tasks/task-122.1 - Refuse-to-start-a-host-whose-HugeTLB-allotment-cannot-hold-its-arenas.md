---
id: TASK-122.1
title: Refuse to start a host whose HugeTLB allotment cannot hold its arenas
status: To Do
assignee: []
created_date: '2026-10-08 23:40'
labels: []
dependencies: []
parent_task_id: TASK-122
priority: high
type: bug
ordinal: 157000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Both pagers' pages are 2 MiB by default (host.DefaultRAMPageSize, DefaultPMEMPageSize), so the RAM, PMEM and ephemeral arenas are all huge pages, taken one slot at a time by fallocate (vmmemory/linux.go, LinuxFile.Zero). A pod whose hugepages-2Mi allotment is smaller than the arenas starts and runs until a guest has written enough; then a fault's fallocate returns ENOSPC, the memory session fails and the VM is lost (seen on GCE 2026-10-08: 'page 4552 fault (write=true): no space left on device', the pod's hugetlb.2MB.events max 1, with a 7 GiB allotment under 16.25 GiB of arenas). The host should find out at start, from its cgroup's hugetlb.2MB.max and the pool, and refuse, as a spill file the disk cannot hold is refused.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A host whose HugeTLB allotment or pool is smaller than its 2 MiB arenas fails at start with an error naming both numbers
- [ ] #2 A test proves the refusal
- [ ] #3 deploy/ and docs/hosting.md state that the RAM arena is huge pages at the default page
<!-- AC:END -->
