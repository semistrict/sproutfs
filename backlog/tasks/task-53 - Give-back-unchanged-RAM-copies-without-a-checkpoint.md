---
id: TASK-53
title: Give back unchanged RAM copies without a checkpoint
status: To Do
assignee: []
created_date: '2026-09-26 23:18'
labels:
  - performance
dependencies: []
priority: high
ordinal: 60000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
RAM is never settled on the interval: OnInterval is true only for PMEM (vmmemory/region.go) and settle runs only inside a publication (volume/publish.go). On x86 a cold read reaches the pager as a write (KVM's async_pf_execute asks for the page writable), and on aarch64 so does a guest's first execution of a page, so a RAM page shared between VMs becomes a private 2 MiB copy that lives as long as the VM unless something checkpoints RAM. The owner decided on 2026-09-26 to give such copies back automatically, independent of checkpoints: per page, write-protect the copy with UFFDIO_WRITEPROTECT (which also drops KVM's mapping through the MMU notifier), compare it with the page it was copied from under that page's lock, and if equal point the guest back at the original and free the copy. No VM pause. A write through a page pinned before the write-protect (O_DIRECT or io_uring DMA into guest RAM, vhost) bypasses the page tables and would be lost, so the pinned writers must be ruled out first; today's seal may have the same exposure.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A written finding lists every writer of guest RAM that can bypass the host page tables (Firecracker's block engines, the embedder's extra drives, anything else), and whether today's seal and settle are exposed to them
- [ ] #2 The cleanup runs on the interval, rate-limited, with no VM pause, and never frees a copy a write could still reach
- [ ] #3 The guest is pointed at the original in place, so the next read makes no new copy even when a cold read arrives as a write
- [ ] #4 Tests prove an unchanged copy is given back, a copy written during the compare is kept, and the Lima suites pass in both arena modes
<!-- AC:END -->
