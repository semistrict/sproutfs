---
id: TASK-71
title: Set the checkpoint interval per VM
status: Done
assignee:
  - '@claude'
created_date: '2026-09-29 23:10'
updated_date: '2026-09-30 00:10'
labels:
  - embedder
dependencies: []
references:
  - host/host.go
  - host/losswindow.go
  - host/flush.go
  - api/host/host.go
priority: low
type: feature
ordinal: 79000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The checkpoint interval is host-wide, 60 s by default. Some of an embedder's VMs want a tighter bound on lost writes. VMs that want none already have `Ephemeral` (TASK-19).

The loss window and flush bound are derived from the interval, and the pager holds the loss window host-wide (host/host.go:97-125). A per-VM interval changes all three together.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A create or open can set a VM interval, clamped to limits the host configures
- [x] #2 A VM with no interval set behaves as today
- [x] #3 A VM's flush bound follows its own interval, and a VM that asks for no interval is held to no loss window
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. host.MachineTerms{Pull, CheckpointInterval} replaces the pull flag; AddMachineWith replaces AddPullingMachine. The host resolves terms into a cadence: interval, windowed, flush bound.
2. Positive interval clamped to [Config.MinimumCheckpointInterval (1s), host interval]; zero is the host's; negative is none (no turns, no loss window, flushes at once; pressure checkpoints still run).
3. CreateRequest/OpenRequest carry it; a migration handoff carries it; fork children take the host's.
4. Status reports each VM's resolved interval.
5. Tests on a simulated clock; docs in hosting.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Criterion 2 changed: it said the loss window follows the VM's interval. That came from a wrong reading when the task was filed: the loss window is configured on its own, not derived from the interval. A tighter interval needs no tighter window, and a looser one is not allowed, so the window is never shorter than any VM's interval. The window changes only for a VM that asks for none.
Verified: host TestAVMIsCheckpointedOnAnIntervalOfItsOwn (clamping both ways, own interval turns while the host's hour does not), TestAVMThatAsksForNoIntervalIsHeldToNoWindow, TestAVMsFlushBoundFollowsItsInterval, TestAMigrationCarriesTheVMsInterval; every existing host test (default terms behave as before); just check.
Not wired: the orchestrator does not forward the interval, and sproutfsctl has no flag for it.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
VMs can ask for their own checkpoint interval through MachineTerms (CreateRequest/OpenRequest), clamped between the host's minimum and its interval; a negative interval asks for none and exempts the VM from the loss window and flush bound. The interval travels with migrations and is reported in Status. Verified with simulated-clock host tests and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
