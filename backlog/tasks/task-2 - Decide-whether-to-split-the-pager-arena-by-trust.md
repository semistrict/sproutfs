---
id: TASK-2
title: Split the pager arena so a VMM reaches only its own VM's memory
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-25 23:08'
labels:
  - security
dependencies: []
references:
  - vmmemory/connection_linux.go
  - rust/sproutfs-vm-memory
priority: high
type: bug
ordinal: 2000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**A compromised VMM can read and write every page of its pager's arena.** An embedder jails the VMM because it may be compromised. The memory session's protocol holds against one: `vmmemory/hostile_linux_test.go` plays a VMM that breaks every rule of it, beside a well-behaved process on the same pager, and the other process keeps its bytes, its faults and its share of the pager. But the protocol is not all a VMM is given. ATTACH hands it the arena's memfd, and the memfd is sealed only against shrinking, growing and further seals. So the VMM can map any offset of the arena writable, or punch it out. The arena holds every VM's private pages and every published page the host's VMs share. A VMM that answers a REVOKE without removing its mapping keeps the same reach, through the mapping instead of the descriptor, once the pager gives that slot to another VM. Closing this needs the arena split by trust. A VMM would be given its own private pages in a file of its own, and shared pages read-only in a file that holds only pages its VM may read. Until then, one compromised VMM reaches the memory of every VM on its host.

This undoes what the jailer is for, so it blocks running untrusted tenants. The fix is a large redesign: each VM's private pages in a file only its VMM receives, and published pages in a file the VMM gets read-only, one per tenant. It touches arena accounting, slots, eviction, spill and the attach protocol on the Go and Rust sides. Write a plan in plans/ first.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The owner has decided whether and when to do it
- [x] #2 A plan for the split is in plans/
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Plan: plans/isolated-arena-2026-09-25.md. Waiting on the owner's five decisions at its end.

2026-09-25: owner said build it behind a switch (SPROUTFS_ARENA=shared|isolated), accepting the recommendations; BLAKE3 for the digest; one GCE run at the end measures both modes.
<!-- SECTION:NOTES:END -->
