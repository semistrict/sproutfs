---
id: TASK-60
title: Find why the read-only mapping test sometimes sees no REMAP event
status: To Do
assignee: []
created_date: '2026-09-27 18:35'
labels:
  - flaky
dependencies: []
priority: low
ordinal: 67000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
internal/vmtest/readonly_linux_test.go TestReadOnlyPrivateMappingTrapsEveryFault/4KiB failed once on Lima aarch64 (2026-09-27, shared arena, full vm-memory suite): 'moving the run into place sent 0 REMAP events, want 1'. It then passed 20 of 20 alone. The test uses its own raw userfaultfd VMM, not the pager's sessions, so the REMAP event is probably read after the count is checked.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The cause is found and the test waits for the REMAP event rather than racing it, or the product defect it shows is fixed
- [ ] #2 The test passes 500 times in a row on Lima
<!-- AC:END -->
