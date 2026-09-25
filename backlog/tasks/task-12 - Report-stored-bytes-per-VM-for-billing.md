---
id: TASK-12
title: Report stored bytes per VM for billing
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 12000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. Each page is billed to the VM that published it.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Each VM reports the bytes it published that the store still holds
- [ ] #2 A simulation invariant: billed bytes summed over VMs equal the bytes in the store; compaction moves bytes but not the bill; reclamation reduces it
<!-- AC:END -->
