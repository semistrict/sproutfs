---
id: TASK-65.1
title: Prove the simulation runs as WebAssembly in a browser
status: To Do
assignee: []
created_date: '2026-09-28 04:31'
labels:
  - docs
dependencies: []
parent_task_id: TASK-65
priority: medium
type: spike
ordinal: 73000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Build the simulation's non-test code (platform/sim, the world in internal/simtest, and what it imports) for WebAssembly and run one scenario in a browser. Find out: which dependencies do not build; how the simulated clock and goroutines behave without testing/synctest; how large the module is; and how the trace can be read from JavaScript.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 One scenario (for example a fork and a migration of two VMs over two hosts) runs to completion in a browser from the real code, and its trace reaches JavaScript
- [ ] #2 The spike's findings, including what had to change and the module's size, are in the task notes
<!-- AC:END -->
