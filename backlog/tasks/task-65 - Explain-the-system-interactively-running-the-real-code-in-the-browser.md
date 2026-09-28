---
id: TASK-65
title: 'Explain the system interactively, running the real code in the browser'
status: To Do
assignee: []
created_date: '2026-09-28 04:31'
labels:
  - docs
dependencies: []
priority: medium
type: feature
ordinal: 72000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An interactive explainer of the whole system in one HTML page, whose animations are driven by the real code rather than a model of it. The deterministic simulation (platform/sim and internal/simtest) already runs the real host, pager, volume, control-record and migration code over a simulated network, object store and clock, and on the Mac, so it does not need the Linux-only memory code. Compiled to WebAssembly, the same code would run in the page, and the simulator's event trace would drive the animation: creates, checkpoints, forks, migrations, holds and their claims, faults and recoveries. Two steps: a spike that proves the code builds and runs as WebAssembly, then the page.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A spike reports whether the simulation's non-test code builds for WebAssembly (js/wasm or wasip1) and runs one scenario in a browser, and lists what had to change or cannot build
- [ ] #2 One HTML page runs the real code compiled to WebAssembly and animates, from the simulator's own trace, at least a create, a checkpoint, a fork, a live migration and a lost host
<!-- AC:END -->
