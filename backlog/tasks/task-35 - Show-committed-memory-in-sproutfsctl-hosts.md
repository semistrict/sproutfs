---
id: TASK-35
title: Show committed memory in sproutfsctl hosts
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
updated_date: '2026-09-26 01:50'
labels:
  - chore
  - deferred
dependencies: []
priority: low
type: enhancement
ordinal: 35000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**`sproutfsctl hosts` shows RESIDENT, not COMMITTED.** Placement now depends on the guest RAM a host has promised (`Pager.CommittedBytes` on `/status` and `/metrics`). So an operator cannot see from the table why a placement was refused. The column was not changed, because `scripts/lib/demo-run.sh` and `demo-workload.sh` parse the table by position.
<!-- SECTION:DESCRIPTION:END -->
