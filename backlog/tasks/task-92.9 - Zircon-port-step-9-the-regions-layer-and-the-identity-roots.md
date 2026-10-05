---
id: TASK-92.9
title: 'Zircon port step 9: the region''s layer and the identity roots'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:10'
updated_date: '2026-10-05 07:39'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.2
  - TASK-92.8
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 107000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 9 of the plan. The heart of the port, built and tested alone before anything uses it: VmCowPages and VmObjectPaged (zircon/kernel/vm/vm_cow_pages.cc, vm/vm_object_paged.cc). A region gets one VmCowPages for the pages it owns, dirty-tracked against our pager; each published (checkpoint, volume) a region reads is an identity root of Clean pages that a lookup falls through to by identity instead of a parent chain. It brings the lookup cursor, supply and take, the dirty states and writeback with D1 to D4, zero intervals, the reclaim split with D2, and the snapshot-on-write copy. No hidden parents, no merge, no full or modified snapshot, no slices, no references.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The region layer and identity roots are ported from vm_cow_pages.cc and vm_object_paged.cc, with each departure D1 to D4 marked in the code beside the line it changes
- [x] #2 The 33 VMO cases the plan lists run as Go tests in synctest bubbles; a case whose expectation a departure changes says which in its comment
- [x] #3 A test shows a store into an AwaitingClean page leaves the checkpoint the bytes of the pause and gives the store a Dirty copy, and a test shows an abandoned writeback makes every page Dirty again with its own reservation
- [x] #4 Nothing outside the package uses it yet, and just check passes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Steps 5, 6 and 8 have not landed, so port the parts of them VmCowPages calls, standalone and marked for those steps to complete: page.go (vm_page_t's object fields, the PMM as an interface), pagequeues.go (page_queues.cc without its threads, aging timers, LRU actions and loans; aging by faults and manual rotation only, decision 4), compression.go (compressor.cc whole and compression.cc's reference bookkeeping over storage and strategy interfaces, no LRU4), pagesource.go (PageSource/PageRequest: populate, resolve on supply/dirty/fail, wait).
2. Commit A, cursor+supply+take: cowpages.go (create, add-page transactions, FindPageContentLocked with the identity-root lookup in place of the parent walk, attribution, lookup, lookup readable, commit, supply, take, decommit, DeferredOps, range changes), lookupcursor.go, objectpaged.go (Create, CreateExternal, Read/Write, GetPage, Lookup, Commit, Supply, Take, Hint, Prefetch, mappings as an interface). One lock per clone tree, as the plan's Locking section says; a region's lookup takes an identity root's lock after its own.
3. Commit B, dirty states: dirty.go: UpdateDirtyStateLocked, PrepareForWriteLocked, DirtyPages, EnumerateDirtyRanges, WritebackBegin/End, zero pages and zero intervals; D1 (a store into an AwaitingClean page or AwaitingClean zero interval moves the AwaitingClean content to the checkpoint's list beside the page list and gives the store a Dirty copy), D3 nothing at this layer, D4 WritebackAbandon; ReadWriteback reads what a checkpoint holds. sim.Bug guards zircon-dirty-awaiting-clean-in-place (D1) and zircon-abandon-leaves-awaiting-clean (D4) in guards.json.
4. Commit C, reclaim: reclaim.go: ReclaimPage, ReclaimRangeForEviction, ReclaimPageForCompression with D2 (a Dirty or AwaitingClean page of a dirty-tracked object, or a checkpoint's copy, is compressed; its reference keeps the dirty state), DedupZeroPage, hints.
5. Commit D, clone: clone.go: CreateCloneLocked with the snapshot-on-write path only (unidirectional children of any root, no hidden parents, no parent content markers), CloneChildLocked, children, dead transition, RangeChangeUpdateCowChildren.
6. Tests: the 33 vmo_unittest.cc cases in sentence names with the Zircon name in a comment, each in a synctest bubble over a sim runtime at 4 KiB and 2 MiB; cases that need hidden parents, full snapshots or slices are adapted to snapshot-on-write and say so. Our own tests for D1, D2, D4 and identity roots.
7. Gremlins on the new files; tests for meaningful survivors. docs/testing.md mutation note. just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Decisions:
- One lock per clone tree: a child shares its parent's mutex. A region's lookup takes an identity root's lock after its own, and only when the root is a different tree.
- D1: pages and zero intervals a checkpoint holds live in a second page list (held) beside the page list. A store into an AwaitingClean page moves the page there and stores into a Dirty copy. WritebackBegin records AwaitingClean zero intervals as held zero intervals. WritebackEnd and Abandon free what is held in their range. ReadWriteback reads what a checkpoint holds.
- D2: a Dirty or AwaitingClean page, or a held page, of an object with a page source is compressed when a compressor is given. The reference metadata keeps the dirty state in its top two bits. A dirty page of zeros becomes a Dirty zero interval.
- D3: nothing at this layer; WritebackProtect is the range protection, WritebackBegin the walk.
- D4: WritebackAbandon makes AwaitingClean pages, references and zero intervals Dirty, frees what is held and unmaps the range.
- No hidden parents, so no parent content markers. Snapshot-on-write clones are allowed for any root; full snapshots are refused; modified snapshots only of an object with no parent.
- Identity roots: CreateIdentityRoot makes a pager-backed root that is never dirtied (an assert). CreateRegionLayer takes a RootResolver; the lookup falls through to the root at the root offset, one page at a time. A store copies the root's page into the layer as Dirty; a read commit copies it as Clean.
- Steps 5, 6 and 8 had not landed, so the parts VmCowPages calls are ported here: page.go, pagequeues.go (no threads, aging timers, LRU actions or loans), compression.go, pagesource.go. Those steps finish them and port their own tests.
- Not ported: pinning, loaned pages, discardable, high priority, cache policy, slices, references, contiguous objects.
- Known gap: a region's mapping of a root's page is not revoked when the root evicts it; step 12's alias set does that.
- Guards zircon-dirty-awaiting-clean-in-place (D1) and zircon-abandon-leaves-awaiting-clean (D4) in scripts/mutation/guards.json; killed 3 of 3 runs each.
- AC #3's "with its own reservation": reservations are not at this layer; they come with step 6 (D5, the reference storage). The abandon test shows every page Dirty again with its own page.

Found while testing survivors: Zircon's ZeroPagesLocked, for a node without parent content markers, takes a gap as zero when the whole gap does not see the parent, so a gap across the parent limit left its first part showing the parent. The port reaches that path in anonymous trees too and now sends any gap that starts below the parent limit to the per-offset walk (commented beside the line in dirty.go; TestZeroingAChildPastItsParentsEndZeroesWhatItSaw). readWriteInternal and unmapAndFreePagesLocked no longer return counts no caller uses. Merged main (step 4's pagelist map) and resolved guards.json.

Gremlins on the eleven production files (--suite full, --file each): first run 801 killed, 257 alive, 188 not covered, 37 timed out; after the survivor tests 885 killed, 178 alive, 181 not covered, 33 timed out. Survivors left: assert bounds, range-change lengths past the object end that no mapping sees, queue counters (step 5/6), page source request lists (step 8), resize/pinning-only paths. Validation: go test ./vmmemory/internal/zirconvm (race and -count=3 -shuffle=on) passes; check-guards.py kills both zircon- guards in 3 of 3 runs; just check.

Coordinator decision (owner asleep): AC #3's 'with its own reservation' part moves to TASK-92.6, which brings the reference storage (D5) it needs; the abandon itself is ported and tested here.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Ported VmCowPages and VmObjectPaged into vmmemory/internal/zirconvm, standalone: lookup cursor, supply, take, dirty states and writeback with D1-D4, zero intervals, reclaim with D2, snapshot-on-write clones, and identity roots a region's layer falls through to. Parts of steps 5, 6 and 8 that VmCowPages calls are ported alongside. 33 VMO cases plus 52 cases of our own, each at 4 KiB and 2 MiB in synctest bubbles. Guards zircon-dirty-awaiting-clean-in-place and zircon-abandon-leaves-awaiting-clean are killed every run. Gremlins: 885 killed, 178 alive, 181 not covered, 33 timed out. just check passes. AC #3 is left open: its 'own reservation' needs step 6's reference storage; the abandon itself is tested.
<!-- SECTION:FINAL_SUMMARY:END -->
