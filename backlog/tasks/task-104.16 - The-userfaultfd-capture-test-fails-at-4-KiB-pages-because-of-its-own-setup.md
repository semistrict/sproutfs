---
id: TASK-104.16
title: The userfaultfd capture test fails at 4 KiB pages because of its own setup
status: To Do
assignee: []
created_date: '2026-10-07 18:56'
labels:
  - vmmemory
  - test
dependencies: []
references:
  - vmmemory/kernel_linux_test.go
parent_task_id: TASK-104
priority: medium
type: bug
ordinal: 141000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found in the step 9 GCE run, as root on a c3 VM. TestManagedPagerCaptureProtectsAndTrapsOnUFFD passes at 2 MiB and fails at 4 KiB with 'invalid managed-memory-region'. The capture never runs. With no backings given, startNative makes each backing pages × 2 MiB (vmmemory/kernel_linux_test.go:550), while the Rust client declares pages × page, 8 KiB here (rust/sproutfs-vm-memory/examples/support/client_linux.rs:53). The pager refuses the mismatch at attach (vmmemory/connection_linux.go:211-212). This is TASK-104.4's AC#5.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 startNative sizes each backing by the test's page, and the test passes at 4 KiB and 2 MiB as root on a GCE Linux VM
<!-- AC:END -->
