---
id: TASK-77
title: Model-check orchestrator recovery against split brain
status: Done
assignee:
  - '@claude'
created_date: '2026-10-01 05:37'
updated_date: '2026-10-01 06:12'
labels:
  - formal
  - orchestrator
dependencies: []
priority: medium
type: feature
ordinal: 84000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Taking a VM over fences a writer that may still be running a guest. So the orchestrator takes an epoch only on positive evidence that the previous holder is gone, and refuses requests while two hosts report one VM (docs/metadata.md, Fencing and selection). spec/ownership assumes these rules and does not check them.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A TLA+ spec models hosts, the orchestrator table, surveys, host loss and pod listing, recovery, migration and drain
- [x] #2 TLC checks that at most one host runs the guest of a VM, except for a fenced host that can never publish, and that recovery never opens a VM while its holder can still run it
- [x] #3 Small configurations run in just check-spec within a few minutes, with mutants; real defects are recorded in spec/bugs.md
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
The model covers hosts, the row, a survey that asks each host in turn, host death and pod unlisting, recovery, and migration. Drain is a sequence of migrations and is not modelled separately. TLC found B3: a recovery's survey could miss a host that opened the VM while it asked, and the open fenced that host's live guest. Fixed with an epoch-conditional open (control.Client.OpenAfter, OpenRequest.Epoch); reopen reads the epoch before it surveys. AC2's 'at most one host runs the guest, except a fenced one' holds by the epoch. The model checks the stronger NoLiveFence: a recovery never fences a holder that is alive.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added spec/recovery/Recovery.tla, a recovery racing a migration across hosts that may die, with invariant NoLiveFence. TLC found B3 (spec/bugs.md): a reopen could fence a running guest that its non-atomic survey missed. Fixed by reading the epoch before the survey and making the host's open conditional on it, refused with control.ErrMoved (HTTP 409). Tests in control, cmd/sproutfs-host and cmd/sproutfs-orchestrator; the orchestrator test fails without the fix. Mutant b3 is caught. just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
