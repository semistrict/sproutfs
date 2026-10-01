---
id: TASK-79
title: Validate simulation traces against the ownership spec
status: Done
assignee:
  - '@claude'
created_date: '2026-10-01 05:37'
updated_date: '2026-10-01 06:35'
labels:
  - formal
  - simulation
dependencies: []
priority: medium
type: feature
ordinal: 86000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
spec/ownership is written from the code by hand, so it can drift from the code without anything failing. The simulation in internal/simtest records traces of what the real code did. Checking those traces against the spec ties the model to the code: a behaviour the code shows and the spec forbids is either a bug or a model gap.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A simulation campaign emits the control-record events that the spec names
- [x] #2 A checker accepts each trace only if the spec allows it, and runs in CI within a few minutes
- [x] #3 A trace the spec rejects is triaged, and real defects are recorded in spec/bugs.md
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
The checker is in Go (internal/simtest/ownership.go) and checks the spec's properties by name on every store change. It is not a TLC refinement check, because the spec's internal states, such as handles' program counters, are not observable in a trace. Running TLC per trace would also need Java in the Go CI jobs. It runs in every world that ends with CheckSelected, across both arena modes in just check, and the simtest suite takes about 17 s. No trace was rejected, so there was nothing to triage. TestOwnershipCatchesWhatTheSpecForbids shows each check fires, and a run whose check saw no record change fails.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Every simulated world now checks each change to a control record or checkpoint index against spec/ownership's properties: SelectionMoves, SelectedReadable, PinnedReadable and KeptReadable. It uses a new sim.ObjectStore.Observe hook and control.ParseRecord, and World.CheckSelected reports violations. The simtest suite passes with no violation. A unit test shows each check catches its defect, and a vacuity guard fails a check that saw nothing. just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
