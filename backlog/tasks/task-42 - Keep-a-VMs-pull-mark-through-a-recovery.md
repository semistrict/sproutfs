---
id: TASK-42
title: Keep a VM's pull mark through a recovery
status: Done
assignee:
  - '@claude'
created_date: '2026-09-26 16:40'
updated_date: '2026-09-26 18:06'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 49000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A VM can be marked to pull its whole memory to local disk when it starts (TASK-37). The orchestrator does not store the mark, so a recovery after a host loss opens the VM without it, and the VM reads the store page by page for the rest of its life on the new host.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The orchestrator records the pull mark with the VM and every open it drives, recovery included, carries it
- [x] #2 A test recovers a marked VM and its new host pulls
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Table: a pull column (added to old files), vmRecord.Pull, kept once set; a survey relearns the mark of a running VM from its host's report (host.VM.Pull).
2. Create and Fork record the mark; reopen (start, recover, the recovery after a lost source) opens with the request's mark or the row's and records it; Migrate adds the row's mark to the handoff every receive carries.
3. Tests: a marked VM recovered after its host is killed opens pulling on the new host; a start without the flag pulls; a migration's receive carries the mark; table keeps and relearns the mark.
4. Docs: orch API comments, hosting.md, deploy/README.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Design: vms.pull column (added to old files), vmRecord.Pull, kept once set (MAX in both upserts). Create and Fork record the request's mark; reopen (start, recover, the recovery after a lost source) opens with request.Pull || row.Pull and records it; Migrate ORs the row's mark into the handoff, so every retried receive carries it. A survey relearns the mark of a running VM from host.VM.Pull, so a lost table keeps running VMs' marks; a stopped VM's mark lives only in the table (deploy/README says so). Refactor: measured(hosts, row) for the RAM a row names; reopen reads its row once.

Validation: go test ./... and just check pass. Orchestrator TestAMarkedVMIsRecoveredPulling (create with pull, kill its host, recover: host-1 open with pull), TestEveryOpenCarriesThePullMark (migration receive and a start without the flag both pull), TestASurveyRelearnsThePullMarkOfARunningVM, and table TestTableKeepsThePullMark. Removing the reopen/migrate/survey lines fails all three orchestrator tests. That an open with Pull makes the host pull is TASK-37's host/pull_test.go (a second host opening a published VM marked to pull faults with zero store reads); no single test drives a real host from the orchestrator.

Rebased onto main: a fork's flight rows carry Pull, so the rewrites and the landing keep it (TestThePullMarkReachesTheHostOnEveryStart checks the child's row; fails without it).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The orchestrator's table records a VM's pull mark (kept once set, relearned from a running VM's host report), and every open it drives carries it: start, recovery, the recovery after a lost migration source, and each receive of a migration (the handoff ORs in the table's mark). Verified by orchestrator and table tests, with the host side covered by TASK-37's pull tests; go test ./..., just check and just soak pass.
<!-- SECTION:FINAL_SUMMARY:END -->
