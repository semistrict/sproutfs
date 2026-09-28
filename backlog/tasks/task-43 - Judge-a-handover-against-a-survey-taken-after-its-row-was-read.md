---
id: TASK-43
title: Judge a handover against a survey taken after its row was read
status: Done
assignee: []
created_date: '2026-09-26 17:48'
updated_date: '2026-09-28 03:31'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 50000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The orchestrator's release() judges a table row against a survey of the hosts taken before it read that row. A child whose receive lands in between is given up after it was already taken in. On a real host that is harmless, because giving up a taken-in child's hold works like releasing it, but the rule reasons from evidence older than what it judges. Found while doing TASK-39.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 release() never judges a row written after the survey it uses began
- [ ] #2 An orchestrator test lands a receive between the survey and the row read, and the child's hold is released rather than given up
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Closed by the owner on 2026-09-27 as not worth fixing. Reading the row before the survey would open a worse race: a fork started between the two reads has a hold its host reports but no row in the snapshot, and the survey would give that hold up mid-fork. The race as it stands is harmless: giving up a child that already landed releases its parent as a release would, and since TASK-44 the host answers claimed and the child keeps running.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Not fixed: harmless as it stands, and the obvious fix (reading the row first) would give up the hold of a fork in progress.
<!-- SECTION:FINAL_SUMMARY:END -->
