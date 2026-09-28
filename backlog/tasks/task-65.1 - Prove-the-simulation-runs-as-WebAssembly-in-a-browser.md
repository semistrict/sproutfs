---
id: TASK-65.1
title: Prove the simulation runs as WebAssembly in a browser
status: Done
assignee:
  - '@claude'
created_date: '2026-09-28 04:31'
updated_date: '2026-09-28 04:47'
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
- [x] #1 One scenario (for example a fork and a migration of two VMs over two hosts) runs to completion in a browser from the real code, and its trace reaches JavaScript
- [x] #2 The spike's findings, including what had to change and the module's size, are in the task notes
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Spike findings (2026-09-28):
- The simulation builds for js/wasm with no source change: GOOS=js GOARCH=wasm go test -c ./internal/simtest (just explainer). The module is 17.9 MB, 4.6 MB gzipped (measure again when served).
- The fast, deterministic clock is testing/synctest, which only a test can enter, so the page runs the compiled test binary with -test.run naming one scenario; the scenario is a real test (TestExplainTheLifeOfAVM) that asserts its outcome. Under Node it runs the existing scenarios unchanged, 0.2 s each.
- The trace: the scenario records step markers (sim.Trace.Record) beside the simulator's own object-store, network, disk and process events, and with SPROUTFS_EXPLAIN=1 prints them as JSON lines ('EXPLAIN {...}'); the page reads them from the Go runtime's stdout through wasm_exec.js's fs.writeSync. 114 events for store, checkpoint, fork, migrate, lose-host, restart-host.
- One browser-only fix: wasm_exec.js's fs stand-in answers a write's callback synchronously, which re-enters Go on a goroutine inside the synctest bubble ('panic: goroutine is already bubbled'). The page answers on a microtask instead (a timer is throttled in a hidden tab).
- In Chrome the scenario passes in about 0.56 s of browser time.
- What the full explainer needs beyond this: the low-level trace has no page-level events (faults, holds, claims, seals) and no VM-to-host placement, so the page needs world-level events (placement, holds, checkpoints by sequence) recorded the same way.

Correction: the module is 4.2 MB gzipped (gzip -9, measured), not 4.6 MB.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The real simulation compiles to WebAssembly unchanged and runs a scenario in a browser as its own test binary under synctest, and the scenario's trace reaches JavaScript through stdout. Verified in Chrome (explainer/index.html via just explainer): the scenario passes in about 0.56 s with 114 events grouped by step.
<!-- SECTION:FINAL_SUMMARY:END -->
