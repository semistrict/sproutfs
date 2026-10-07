---
id: TASK-104.4
title: 'Fsync journal step 4: the pager captures a region''s changed blocks'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 15:48'
labels:
  - durability
  - pager
dependencies:
  - TASK-104.1
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 128000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 4 of the plan. Guest stores to a PMEM disk are CPU stores into mapped memory, so the pager is the only place that can say which pages changed since the last flush. It keeps an unjournaled set, write-protects it at each capture, and finds the changed 4 KiB blocks of 2 MiB pages by a SHA-256 digest per block, 16 KiB per page (owner decision 2).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Every page the guest can store into without a fault is unjournaled; the check runs after every step of the existing pager campaigns
- [ ] #2 MemoryRegion.Capture write-protects the unjournaled runs one command per run, hashes each block once on the settle workers, and returns the blocks whose SHA-256 differs from the digest held; a protect trap on a page no seal holds marks it unjournaled and copies nothing
- [ ] #3 A page with no digests takes them from its resident origin or the zero block, or is captured whole
- [ ] #4 A seal moves the unjournaled pages to the checkpoint and drops their digests; a capture during the seal covers them from the sealed copies; an abandon makes them unjournaled again without digests; other pages keep or drop their digests as step 1 decided
- [ ] #5 A spilled page is captured from the spill; the Linux userfaultfd test covers the new trap on 2 MiB HugeTLB and 4 KiB pages; guards journal-trap-not-marked, journal-seal-keeps-unjournaled and journal-digests-survive-unjournaled-seal are killed by their tests
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Binding mark protected (dirty, write-protected by a capture) and zeroed; writable() excludes protected, so every mapping path keeps the protection.
2. Region journal state: unjournaled runs and per-page SHA-256 block digests; every store path marks the page unjournaled.
3. MemoryRegion.Capture(pages): holds the region, protects writable taken runs, reads each page (sealed copy where shared, own page or spill otherwise) on SettleWorkers, keeps blocks whose digest changed; Captured.Fail gives pages back (B6).
4. Seal: unjournaled pages become the checkpoint's list with digests dropped; publication drops the list and lets clean list pages take digests from the published page; abandon gives the list back unjournaled.
5. Protect trap: unprotectForStore maps the page writable in place, no copy.
6. Tests over the fixture, a capturing disk guest in the prefetch campaign with the writable-is-unjournaled check per step, a real-UFFD test, and guards.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
From TASK-104.2: journal.Commit takes the room the capture's entries may use before the capture runs; the capture must fit that room (split across batches when it does not).

From TASK-104.1 (spec/bugs.md B6): a batch whose write or sync fails must give its pages back as unjournaled and drop their digests; otherwise a later successful flush misses the stores the failed one covered. Digests are kept across a seal for pages not unjournaled at the seal, and dropped for pages that were (MCCapture, DigestsDescribeTheJournal).

Built (vmmemory/journal.go and hooks in bindings, checkpoint, fault, store, cold, peer). A selection gives each page still on the seal's list the digests of the selected bytes (the pager drops them so the next copy takes them from the published page). spec/journal Capture.tla gains Digests = "refresh" for this; MCCapture now checks it (3.94M states, 25 s). Giving refreshed digests to every page, including those stored into under the seal, loses a block: TLC found the trace, so only pages still on the list get them.
Dropped digests are an explicit empty list meaning write the page whole; absent digests mean the page has had no entry since it became the region's own and may take them from its origin or zeros. After a failed capture or a seal the origin is not a valid source, because an entry the replay applies may hold other bytes.
Tests: vmmemory/journal_test.go (changed blocks only, trap without copy, capture under seal, store under seal leaves the list, digests across a seal, failed capture, zero page, spilled page, RAM refused, page stored into under the seal written whole). The prefetch campaign has a capturing disk guest and checks writable-is-unjournaled after every step; with journal-trap-not-marked on it fails at the first protected store.
Guards: journal-trap-not-marked, journal-seal-drops-unjournaled, journal-digests-survive-unjournaled-seal, journal-failed-keeps-digests, all killed by just check.
TestManagedPagerCaptureProtectsAndTrapsOnUFFD (4 KiB and 2 MiB HugeTLB) is written and vets for Linux; it runs in the GCE qualification after step 7.
just check exit 0.
<!-- SECTION:NOTES:END -->
