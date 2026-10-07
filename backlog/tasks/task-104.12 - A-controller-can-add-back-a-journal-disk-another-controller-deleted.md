---
id: TASK-104.12
title: A controller can add back a journal disk another controller deleted
status: In Progress
assignee: []
created_date: '2026-10-07 18:56'
updated_date: '2026-10-07 19:33'
labels:
  - journal
  - membership
dependencies: []
references:
  - membership/shardcontrol.go
  - membership/journals.go
parent_task_id: TASK-104
priority: high
type: bug
ordinal: 137000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found by the spec/shards Journals model (spec/bugs.md B8 on the spec agent's branch, f8e2d1ce). ShardControl.Pass lists the journal disks once. When Store.Update loses the compare-and-set, it runs Next again over the newer generation with the same list. That can re-add a disk another controller deleted and removed in between. The membership then offers a disk that does not exist, and nothing ever removes it.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Steps taken from the cloud's list of journal disks apply only over the generation read before the listing; Next skips an add when the membership is newer
- [x] #2 A test of two controllers shows a deleted disk is never added back
- [x] #3 The spec/shards journal-stale-list mutant is caught and the fixed model passes
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Want.JournalsAfter is the generation the listing followed; Next adds a listed disk only over it (guard membership-add-from-a-stale-list, TestAStaleListingAddsNoJournalDisk).
<!-- SECTION:NOTES:END -->
