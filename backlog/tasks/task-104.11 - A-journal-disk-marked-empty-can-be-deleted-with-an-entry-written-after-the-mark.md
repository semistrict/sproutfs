---
id: TASK-104.11
title: >-
  A journal disk marked empty can be deleted with an entry written after the
  mark
status: To Do
assignee: []
created_date: '2026-10-07 18:56'
labels:
  - journal
  - membership
dependencies: []
references:
  - membership/journals.go
  - host/journaldisks.go
  - spec/shards
parent_task_id: TASK-104
priority: high
type: bug
ordinal: 136000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found by the spec/shards Journals model (spec agent, commit f8e2d1ce on its branch; spec/bugs.md B7). A host reports its own journal disk empty when it runs no VM and holds no live entry. But a VM the orchestrator placed from an older membership can still arrive and flush. If the host or its node then dies, nothing unmarks the disk: Let keeps the mark, the reservation clears, and the disk is deleted with the entry. Reopening the disk when the close finds live entries does not help, because a dead host never closes. A report made before the host read the drain does the same. So does a report that names only the disk: a host holding it under an older assignment as a reader can mark the disk it is now assigned to write.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A host reports its own journal disk empty only once the membership it holds releases the disk
- [ ] #2 From then on the host takes no VM
- [ ] #3 A host's report names the assignment it holds the disk under, and the controller marks or unmarks only on a report of the disk's current assignment
- [ ] #4 spec/shards Journals: the B7 mutants (report before release, place on releasing, mark any assignment) are caught, and MCJournals passes with the fix
- [ ] #5 A membership or host test shows that a late-placed VM's flush on a disk reported empty does not let the disk be deleted
<!-- AC:END -->
