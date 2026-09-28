---
id: TASK-65
title: 'Explain the system interactively, running the real code in the browser'
status: Done
assignee:
  - '@claude'
created_date: '2026-09-28 04:31'
updated_date: '2026-09-28 05:35'
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
- [x] #1 A spike reports whether the simulation's non-test code builds for WebAssembly (js/wasm or wasip1) and runs one scenario in a browser, and lists what had to change or cannot build
- [x] #2 One HTML page runs the real code compiled to WebAssembly and animates, from the simulator's own trace, at least a create, a checkpoint, a fork, a live migration and a lost host
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Verified in Ego Lite: the worker runs all 5 chapters in ~790 ms and every one of the 23 steps plays through to the end without a freeze. Ego Lite 'crashes' were partly an ego-browser -e quirk (a < in the script hangs the CLI). Node peak RSS 281 MB with GOMEMLIMIT=96MiB (393 MB without).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
explainer/index.html walks five chapters (a VM is pages, checkpoints, a lost host, a fork, a migration). Each is a simtest test (internal/simtest/explain_test.go) that snapshots the real state per step (World.Snapshot) and brackets its trace events. The page runs the test binary as WebAssembly in a worker (run.js) and animates each step's store and network traffic from the trace. Verified by clicking through every step in Ego Lite; just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
