---
id: TASK-104.3
title: 'Fsync journal step 3: the control record names its journals'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 01:53'
labels:
  - durability
  - metadata
dependencies: []
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 127000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 3 of the plan. A recovery must know which journals may hold entries after the selected checkpoint, and from which position. The control record is the authority, and it is written at every selection and open already, so it carries the list at no extra write.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Control record format 6 carries a list of journals, each naming a disk identity, its generation, the epoch and the covered position; format 5 is refused by name
- [x] #2 Select and SelectKept write the list; an open keeps it; a migration open adds its own journal; a stop and a close write an empty list
- [x] #3 Record tests and format fixtures cover each change; spec/ownership is updated if its state changes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. control: Journal type (disk identity, generation, epoch, covered position) and Record.Journals in proto field 14; format 6; format 5 refused by name; clone, equal, marshal, parse, valid (at most two, disk set, epochs ascending and not past the record's).
2. Handle.Select and SelectKept take the list and write it. Client.OpenAfter keeps it. Client.OpenMigration takes the next epoch and adds the destination's journal, stamped with that epoch, after the others; a third journal is refused.
3. volume: selecting passes no journal, so every selection, a stop's and a close's included, writes an empty list. No new record write.
4. Fixtures: testdata/record-6 written by -update; record-5 joins the superseded list.
5. Tests: control (select, select kept, open keeps, pin and release keep, migration open adds and is reconciled, third refused, invalid lists refused), volume (an open keeps and a close writes empty), host (a stop writes empty).
6. spec/ownership: its state models no journal and no rule there reads one; spec/journal (104.1) owns the list's rules. Leave it unless that turns out wrong.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Control record format 6: Record.Journals (field 14), each a Journal of disk identity ([16]byte, as the membership's rank.Identity; control cannot import rank, which imports control), generation, epoch and covered position. At most two (MaximumJournals), disks with an identity, epochs strictly ascending and none past the record's; a parsed record that breaks this is ErrCorrupt. Format 5 is refused by name (control/testdata/record-5 joined the superseded list; volume's deployment fixtures moved to deployment-record-6-index-9-part-5).
Handle.Select and SelectKept take the list and replace the record's with it; a list the record cannot hold is ErrInvalidConfig before any write. OpenAfter keeps the list (clone). New Client.OpenMigration adds the destination's journal, stamped with the epoch it takes, in the same write that takes the epoch; a third journal is ErrTooManyJournals with the epoch left alone. Pin and Release keep the list.
volume passes no journal at every selection, so the interval's, a stop's and a close's selections all write an empty list. No new record write.
spec/ownership unchanged: its record state has no journal and none of its actions or invariants would read one; the list's rules belong to spec/journal (TASK-104.1).

Validation: go test ./control ./volume ./host (new tests in control/journals_test.go, control/format_test.go, volume/journals_test.go, host/journals_test.go); a mutant that keeps the old list on an empty selection fails the control, volume and host tests; just check exit 0.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Control record format 6 names the journals that may hold a VM's flushed writes newer than its selected checkpoint (disk identity, generation, epoch, covered position; at most two). Select and SelectKept take and write the list; an open, a pin and a release keep it; Client.OpenMigration adds the destination's journal in the write that takes the epoch. volume passes no journal, so every selection, a stop's and a close's included, writes an empty list and behaviour is unchanged. Format 5 is refused by name; fixtures added for format 6 in control and volume. spec/ownership unchanged: its state holds no journal. Verified with new tests in control, volume and host, and just check exit 0.
<!-- SECTION:FINAL_SUMMARY:END -->
