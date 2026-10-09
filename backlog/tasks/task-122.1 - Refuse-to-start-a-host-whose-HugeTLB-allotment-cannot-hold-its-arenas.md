---
id: TASK-122.1
title: Refuse to start a host whose HugeTLB allotment cannot hold its arenas
status: Done
assignee:
  - '@claude'
created_date: '2026-10-08 23:40'
updated_date: '2026-10-09 09:48'
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
- [x] #1 A host whose HugeTLB allotment or pool is smaller than its 2 MiB arenas fails at start with an error naming both numbers
- [x] #2 A test proves the refusal
- [x] #3 deploy/ and docs/hosting.md state that the RAM arena is huge pages at the default page
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. host/hugetlb.go: what the 2 MiB arenas need (resident pages × page), what the host may take (least hugetlb.2MB.max of its cgroup and every ancestor; the pool's nr_hugepages), and admit refusing with all three numbers.
2. supervisor_linux.go: check before any pager is built.
3. Tests over a fake /proc and /sys (fstest.MapFS): allotment from container/pod cgroups; refusal past the allotment and past the pool; 4 KiB arenas need nothing.
4. docs/hosting.md, deploy/README.md, deploy/10-host.yaml and the SupervisorConfig comments: arenas at the default 2 MiB are huge pages; the start check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
host/hugetlb.go reads the least hugetlb.2MB.max from the host's cgroup up to the root and the pool's nr_hugepages; supervisor_linux.go checks the RAM, PMEM and ephemeral configs before any pager is built. Verified: go test ./host (TestAHostsHugeTLBAllotmentIsTheLeastOfItsCgroups, TestAHostRefusesArenasItsHugeTLBShareCannotHold over a fake /proc and /sys, including the 2026-10-08 numbers: 16.25 GiB of arenas under a 7 GiB allotment); GOOS=linux go vet ./host.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A host refuses to start when its 2 MiB arenas need more HugeTLB pages than its cgroups allow or the node's pool holds, naming all three numbers (7f8f2de4). Proved by host unit tests over a fake /proc and /sys; deploy/README.md, deploy/10-host.yaml, docs/hosting.md and the SupervisorConfig comments say the arenas at the default 2 MiB page are huge pages and describe the check.
<!-- SECTION:FINAL_SUMMARY:END -->
