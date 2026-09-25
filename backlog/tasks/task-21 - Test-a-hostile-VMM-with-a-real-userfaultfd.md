---
id: TASK-21
title: Test a hostile VMM with a real userfaultfd
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - security
  - testing
dependencies: []
priority: medium
type: task
ordinal: 21000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The hostile-session fuzzing uses fake descriptors, so its sessions end at the first resolve. Seal, retire and settle have not run under hostile timing. It needs a hostile client process, such as a proxy in front of the Rust client.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A fuzz target drives a real registered userfaultfd through seal, retire and settle
<!-- AC:END -->
