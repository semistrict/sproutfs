---
id: TASK-92.3
title: 'Zircon port step 3: the page list'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 06:14'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.1
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 101000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 3 of the plan. Zircon keeps a VMO's pages in a page list whose slots hold a page, a zero marker, a reference to compressed storage, or one end of a zero interval with its own dirty state (zircon/kernel/vm/vm_page_list.cc, vm/include/vm/vm_page_list.h). The pager keeps the same facts in binding blocks, compressed zero runs and sealable runs kept in step by hand. This step ports the page list alone, with its tests; nothing uses it yet.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 vmmemory/internal/zirconvm holds the page list, its slots, zero intervals with their dirty states, cursors and splice lists, ported from vm_page_list.cc and vm_page_list.h with byte offsets and the pager page size in place of kPageSize
- [x] #2 All 55 cases of vm/unittests/vmpl_unittest.cc run as Go tests in synctest bubbles, each naming its Zircon case in a comment, at a 4 KiB and a 2 MiB page
- [x] #3 just check passes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Port vm_page_list.h and vm_page_list.cc into vmmemory/internal/zirconvm: pageormarker.go (VmPageOrMarker, its zero range bits, VmPageOrMarkerRef), pagelist.go (VmPageListNode, VMPLCursor, VmPageList with BatchInserter, intervals, splitting, populating, overwriting and clipping), splicelist.go (VmPageSpliceList). Offsets stay bytes; each list carries its page size in place of kPageSize.
2. Language-forced changes, each commented at the line: the page is held as *P of a type parameter rather than a pmm index; the node tree is github.com/google/btree (Apache-2.0), stepping by search where Zircon steps an iterator; walk callbacks return nil/ErrStop/error for ZX_ERR_NEXT/ZX_ERR_STOP/other; branches that only handle a failed node allocation are dropped; an interval sentinel carries its list's page shift for the AwaitingClean length; the splice list frees through a Freer given to it; CreateFromPageList (PhysicalPageProvider only) is not ported.
3. Port all 55 vmpl_unittest.cc cases with sentence names and the Zircon name in a comment, each run in a synctest bubble at 4 KiB and 2 MiB pages.
4. Run Gremlins on the package, add tests for meaningful survivors.
5. just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Ported vm_page_list.h/.cc into pageormarker.go (VmPageOrMarker with its zero range bits, VmPageOrMarkerRef), pagelist.go (VmPageListNode, VMPLCursor, VmPageList with BatchInserter, walks, intervals, split, populate, return, overwrite, clip, merge) and splicelist.go (VmPageSpliceList). All 55 vmpl_unittest.cc cases are Go tests named in sentences with the Zircon name in a comment, each in a synctest bubble at 4 KiB and 2 MiB (110 subtests). Language-forced changes, each commented at the line: page held as *P of a type parameter, not a pmm index; node tree is github.com/google/btree v1.1.3 (Apache-2.0), stepping by search; nil/ErrStop/error for ZX_ERR_NEXT/STOP/other; branches only handling failed node allocation dropped (MergeRangeOnto and AddPagesFrom return nothing); an interval sentinel carries its list's page shift for the AwaitingClean length; splice list frees through a Freer and its destructor is Free(); MergeRangeOnto asserts both lists share a page size; HeapAllocationBytes counts page list nodes and their entries; CreateFromPageList (PhysicalPageProvider only) not ported; Pop on an unfinalized list asserts (all asserts panic). Gremlins: Zircon's cases alone 343 killed / 56 lived / 77 not covered; 20 extra cases of our own (marked as not Zircon's) brought it to 420 / 26 / 30. The 26 survivors are equivalent (assert bounds valid input cannot reach, or equal-case boundaries giving the same result); the 30 not covered are bit-layout constants, ErrNoMemory branches unreachable inside an interval, and the list's own failing self-checks. scripts/mutate-simulation.py's snapshot now copies LICENSE files, which the header test reads. just check exited 0.

Correction: the cases of our own number 24, not 20 (79 tests in pagelist_test.go, 55 of them Zircon's).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Ported Zircon's page list (vm_page_list.h/.cc at fuchsia 90e54e09) into vmmemory/internal/zirconvm with byte offsets over the pager's page size, and all 55 vmpl_unittest.cc cases as sentence-named Go tests in synctest bubbles at 4 KiB and 2 MiB, plus 24 cases of our own for parts Zircon reaches only through VmCowPages. Mutation testing with Gremlins: 420 killed, 26 equivalent survivors, 30 unreachable. Verified with go test (also -race) and just check (exit 0).
<!-- SECTION:FINAL_SUMMARY:END -->
