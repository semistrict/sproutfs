---
id: TASK-97
title: Review the decisions taken on the Zircon port while the owner slept
status: Done
assignee: []
created_date: '2026-10-05 17:20'
updated_date: '2026-10-06 15:47'
labels:
  - decision
  - vm-memory
dependencies: []
references:
  - plans/zircon-pager-port-2026-10-05.md
priority: high
ordinal: 117000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
On the night of 2026-10-05 the coordinator took these on the plan's recommendations: D1 split a store into a page a checkpoint holds; D2 spill dirty pages; Zircon byte offsets; aging from faults only; the switch-over gate; sentence test names. It also narrowed TASK-92.4 AC 1 (pageranges stays for the connection's mapping generation), checked TASK-92.5 AC 1 against decision 4, and moved TASK-92.9 AC 3's reservation part to TASK-92.6. All are in TASK-92's notes.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The owner confirms or reverses each, and reversals get tasks
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
2026-10-06 owner answers: D1 (a store into a page a checkpoint holds gets a copy) kept; D2 (dirty pages may spill) kept; Zircon's byte offsets kept in zirconvm; aging from faults only kept; the three narrowed ACs (92.4 pageranges stays, 92.5 fault-driven aging, 92.9 reservation moved to 92.6) accepted; the step-13 gate was waived earlier the same day.
<!-- SECTION:NOTES:END -->
