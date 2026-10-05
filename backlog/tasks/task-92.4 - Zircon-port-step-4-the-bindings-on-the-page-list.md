---
id: TASK-92.4
title: 'Zircon port step 4: the bindings on the page list'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 07:12'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.3
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 102000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 4 of the plan. The first in-place swap: the binding blocks and the compressed zero runs (vmmemory/bindings.go, vmmemory/internal/pageranges) become the ported page list, and the runs a seal protects become its dirty runs. The fields Zircon has no place for (mapped, the checkpoint copy a page shares, its origin, cold, written ahead) stay in a binding beside the slot. The existing pager suite is the check, unchanged.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A memory region keeps its per-page state in the ported page list; vmmemory/internal/pageranges is no longer used by vmmemory
- [x] #2 Every test in vmmemory, host, vmmigrate and internal/simtest passes unchanged, in both arena modes, and every guard in scripts/mutation/guards.json is still killed
- [x] #3 vmmemory/sparse_metadata_test.go still holds the metadata bound, and a region that touches one page in 512 holds no more metadata than before, measured by a test
- [x] #4 A seal still issues one protect command per run of dirty pages, shown by the existing seal tests
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Measure BenchmarkARandom4KiBFault and BenchmarkAForward4KiBFault, and the metadata a region touching one page in 512 holds, on the old code.
2. Replace the binding blocks with a zirconvm.PageList[binding] per memory region: a slot holds a page's binding, allocated alone; page indexes convert to byte offsets at the boundary.
3. Replace the compressed zero runs with Untracked zero intervals in the same list; a bound page a run covers keeps it in its binding (inZeroRun), since a slot cannot be both.
4. Replace the sealable runs with the Dirty intervals of a page list of their own (pageRuns), so a seal still reads O(runs).
5. Rewrite eachBinding, markZeros, detach over the page list; drop the unused Dirty state from pageranges, which the Linux connection still uses for mapping generations.
6. Add a metadata test for one page in 512, a refused zero run test and a pageRuns test; update docs/vm-memory.md.
7. Run just check, and the benchmarks again.

8. The page list's google/btree lookups made a random 4 KiB fault about 25% slower (3.2 to 4.0 us on the Mac). Keep the page list's nodes in a map beside the tree in zirconvm/pagelist.go, so find and an exact lowerBound need no tree search.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Design: a slot of the region's page list holds Page(*binding); bindings are allocated one at a time and never leave the list until detach, so their addresses stay stable for alias sets. Compressed zero runs are Untracked zero intervals. A page that has a binding and that a zero run covers keeps the run in binding.inZeroRun, because a slot holds a binding or lies in an interval, never both; this keeps the old lazy semantics exactly (the run's mapping becomes the page's own state when it is next bound). Sealable runs are the Dirty intervals of a second page list (pageRuns), so a seal still reads O(runs). eachBinding walks the list in batches of 256 bindings under the host lock then bindingsMu. binding is reordered to 64 bytes.
pageranges is not deleted: connection_linux.go keeps its client's mapping generations in it, which the page list has no place for. Its unused Dirty state is removed. So AC #1's second clause does not hold as written.
zirconvm/pagelist.go changed (minimal, perf): a map of nodes by offset beside the btree, used by find and by lowerBound when a node is at the first node offset at or after the target; insert, erase and Clear keep it. The 55 page list tests pass unchanged.
Metadata: a region reading one page in 512 holds 389 bytes a touched page, against 21,828 with binding blocks (new test). Benchmarks (Mac, load 8-10, 4 KiB-only binaries, interleaved): BenchmarkARandom4KiBFault median 3.37 us before, 3.24 us after; BenchmarkAForward4KiBFault 395 us before, 392 us after.

Validation: just check exit 0 (gofmt, build and vet for Linux and Darwin, go test ./..., the shared-arena pass of vmmemory, host, vmmigrate, internal/simtest and vmmachine, every guard in guards.json killed, specs). go test -race ./vmmemory/... passes. Seal tests (seal_test.go, revocation_test.go) unchanged and passing. New tests: TestARegionTouchingOnePageIn512HoldsMetadataForThosePagesAlone, TestARefusedZeroRunIsNotRecordedAsMapped (both its halves checked by mutation), TestPageRunsJoinAndSplitAsPagesEnterAndLeave. AC #1 left unchecked: pageranges still serves connection_linux.go's generation map; the owner decides whether to move it or reword the AC.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The memory region's binding blocks and compressed zero runs are now the ported zirconvm page list: a slot holds a page's binding, a zero run is an Untracked zero interval, and the sealable runs are the Dirty intervals of a page list of their own, read in O(runs). A page touched alone costs 389 bytes instead of 21,828. A map of nodes beside zirconvm's btree keeps a random 4 KiB fault at its old cost. Verified with just check, go test -race of vmmemory, three new tests and interleaved benchmarks.
<!-- SECTION:FINAL_SUMMARY:END -->
