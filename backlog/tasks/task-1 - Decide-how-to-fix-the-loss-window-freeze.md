---
id: TASK-1
title: Decide how to fix the loss-window freeze
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - needs-owner
  - correctness
dependencies: []
references:
  - vmmachine/neighbours_linux_test.go
  - vmmemory/pressure.go
  - host/interval.go
priority: high
type: bug
ordinal: 1000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**A store the loss window holds can stop its guest for an eighth of the interval, and for good while publications fail.** `TestAGuestPastItsLossWindowIsCheckpointedOutOfTurn` shows it on the aarch64 Lima instance. It runs two guests with the interval an hour away and a two-second window. After 162 rounds, the well-behaved guest's `write` took more than 30 seconds.

A store past the window waits in the pager for a checkpoint of its VM. When no checkpoint is in flight, the host takes one out of turn, and the checkpoint starts with a pause. The waiting store holds its vCPU inside a userfault. On this kernel the pause's signal does not bring the vCPU out. So Firecracker gives up on the pause after 30 seconds (`Failed to message the vCPUs`), the capture fails, and the loop backs off by an eighth of the interval. A backoff answers no request out of turn, so the guest stops for that long. While the store is down, every attempt finds a vCPU held and fails the same way, so the guest never runs again. Goroutine dumps showed both guests' faults in `takeSpill` and both loops in `waitForCheckpoint` with requests off.

A full dirty budget meets the same pause, but it rarely waits with nothing in flight. The pager asks at three quarters of the budget, so the checkpoint is sealed while the guest still runs, and a later store waits only for a publication. The window has no such early request. A fix has to keep every held store behind a checkpoint that is already sealed. Two ways are open: the pager asks for the window's checkpoint before the window runs out, and admits a store only while a requested checkpoint has not sealed yet; or the VMM counts a vCPU held in a userfault as paused. The first loosens the bound while the store is down, and the second changes the VMM. The choice is the owner's.

Recommendation: the pager-side fix. Ask for the window's checkpoint before the window runs out, so its pause and seal happen while the guest runs, and retry the upload of that same sealed checkpoint when it fails instead of handing its pages back. Then no pause is ever needed while a store is held, and the window stays enforced while the store is down. The VMM-side fix probably cannot work: a vCPU inside a userfault is inside the kernel mid-fault, so its state cannot be saved consistently. Also check whether x86_64 kernels behave the same.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The owner has chosen a fix
- [ ] #2 TestAGuestPastItsLossWindowIsCheckpointedOutOfTurn passes on Lima
- [ ] #3 A guest held by the window while the object store is down is stopped deliberately, not frozen
<!-- AC:END -->
