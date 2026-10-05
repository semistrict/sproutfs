---
id: TASK-92.6
title: 'Zircon port step 6: the spill as references in the page list'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 09:19'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.5
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 104000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 6 of the plan. Zircon records a compressed page as a reference in its page list slot and keeps the bytes in compression storage (zircon/kernel/vm/compression.cc, vm/slot_page_storage.cc). A spilled page is the same thing with the spill file as the storage. Two departures apply: a page a pager backs may be spilled when dirty (D2), and a dirty page takes its spill slot before it is dirty, so a spill never needs space (D5). LZ4 is not ported; the storage keeps the page as it is with its CRC32C.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A spilled page is a reference in the page list; the storage follows VmCompression's interface over the spill file, with the file allocated at start, truncated at start and checked on read as today
- [x] #2 compression_smoke_test, compression_zero_test, compression_fail_test and compression_move_reference_test run as Go tests
- [x] #3 vmmemory/reservations_internal_test.go is rewritten against the new storage and keeps every property it tests
- [x] #4 The guards spill-sparse and pager-forget-spill sit in the new storage under the same names and are still killed by the tests guards.json names
- [x] #5 Every test in vmmemory, host, vmmigrate and internal/simtest passes unchanged in both arena modes
- [x] #6 A checkpoint's abandon (WritebackAbandon, from TASK-92.9) gives each page made Dirty again its own reservation, shown by a test
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. zirconvm/compression.go: complete VmCompression in place: the storage interface with a context for I/O, statistics and memory usage, StoreAsIs as the strategy (LZ4 not ported), and D5: Reserve, a store into the page's own reservation, a decompress that keeps it. No timestamp: a page stored as it is fills its slot.
2. zirconvm/spillstorage.go: the spill file as compressed storage in the shape of VmSlotPageStorage: a reference is an allocation's id past the alignment bits; allocations handed out lowest first and reused last-freed first (reservations.go's order); the file truncated and allocated at start (spill-sparse), a CRC32C per allocation checked on read, pager-forget-spill on store, consecutive references written in one write.
3. D5 in VmCowPages: a page holds a reservation while Dirty or AwaitingClean, taken before it becomes Dirty (store, D1 copy, dirty copy of a root's page, DirtyPages all up front), kept through spill and refault, given back when it is cleaned or freed. WritebackAbandon gives each page made Dirty again its own.
4. Tests: compression_smoke/zero/fail/move_reference ported over a sim.Disk spill file at both page sizes; the zirconvm test helper's memory storage replaced by it; reservations_internal_test.go's properties against the spill storage; D5 cases and the abandon AC.
5. Replace in place in vmmemory: reservations.go deleted; the binding's spill slot becomes a reservation (a reference of the spill storage); takeSpill, releaseSpill, evictBatch's writes, the spill read, New and Close go through the storage.
6. Whole suite both arena modes, -race on vmmemory and zirconvm, check-guards, benchmarks old/new alternated, Gremlins on changed zirconvm files, docs, just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TASK-92.9 ported compressor.cc and compression.cc's reference bookkeeping into vmmemory/internal/zirconvm/compression.go ahead of this step, over storage and strategy interfaces, without D5 or the four tests. This step adds the reference storage, D5 and the reservations.

Design: zirconvm.SpillStorage (spillstorage.go, the shape of slot_page_storage.cc) is the spill file as VmCompression's storage. A reference is an allocation id shifted past the alignment bits, one page of the file per allocation; ids are handed out lowest first and reused last-freed first, the order reservations.go had, so state is kept only for what was handed out and seeds replay. It truncates and allocates the file at start (spill-sparse sits there), records a CRC32C per store (pager-forget-spill sits there) and checks it on read (ErrIODataIntegrity, which vmmemory reports as ErrSpillCorrupt), writes consecutive references in one write, and keeps Zircon's metadata and memory usage. MaxSpillPages is 2^29-1, every id a reference names but the temporary one; a larger dirty budget is ErrConfig.
compression.go is whole but for LZ4 (StoreAsIs replaces it), the timestamp (no room after a whole page) and Dump. Its storage interface takes a context, since the storage is a file; Decompress can fail, and then the reference stays.
D5 in VmCowPages: VmPage carries a reservation, taken before the page becomes Dirty (dirtyForStoreLocked, D1's copy, the lookup cursor's dirty copy, DirtyPages all up front so it stays all or nothing), held while Dirty or AwaitingClean, compressed into (the reference is the reservation), decompressed back keeping it (DecompressReserved), given back when the page is cleaned or freed. A moved temporary reference hands the reservation to the spare page; a compression result written into a reservation someone else now holds is unstored, not freed. updateDirtyStateLocked asserts the invariant. Anonymous pages compress as Zircon's do. A spilled AwaitingClean page that WritebackEnd cleans stays a Clean reference holding its bytes, as step 9 left it.
vmmemory: reservations.go deleted; the binding's spillSlot int is now a reservation (a spill storage reference, or none); Host.spill is the storage. reservations_internal_test.go's properties moved to zirconvm/spillstorage_test.go against the storage.
A binding still holds its reservation rather than the page list slot holding a Reference: the old core's bindings must stay in their slots while spilled (faults, alias sets and seals hold them), so a spilled page's reference is in the page list in the region's layer (zirconvm) and in the binding's reservation in the old core until step 11/12 replaces it.
Signature changes for the context the storage needs: ReclaimPage, ReadWriteback, SupplyPages, RequireReadPage, DecompressInRange, ProcessPagesForSupply take ctx; zirconvm tests updated mechanically. fault.go touched only for the reservation type (spill *int -> *reservation, -1 -> noReservation); faultfirst.go, prefetch.go, pagesource.go untouched.

Validation: go test of vmmemory, host, vmmigrate, internal/simtest and vmmachine passes in both arena modes; go test -race of vmmemory (244 s) and zirconvm passes; check-guards kills spill-sparse, pager-forget-spill and both zircon- guards (154 of 154 in just check).
Benchmarks (Mac, old and new vmmemory test binaries alternated, 10 runs each, medians; load average about 12 from other agents, so the ranges are wide): BenchmarkARandom4KiBFault 4.21 us old vs 4.13 us new (-1.7%; ranges 2.98-11.99 vs 3.37-5.76); BenchmarkAForward4KiBFault 475 us vs 460 us (-3.3%; 407-718 vs 397-595); the spill and pressure tests (dirty_budget_test, spill_space_test, spill_power_loss_test, TestASpilledPageFaultsBackWhatTheGuestStored) 0.398 s vs 0.373 s (-6.2%). No change beyond the noise.
Gremlins (--suite full): compression.go, spillstorage.go, page.go: 100 killed, 9 lived, 12 not covered. Lived: assert bounds (5), the budget bounds of NewSpillStorage (killed by a new test), a short write with no error (the sim disk cannot make one). dirty.go, cowpages.go, reclaim.go, lookupcursor.go, supply.go: 589 killed, 108 lived, 113 not covered, 22 timed out. On lines this step changed: one lived (the fallback in reserveFromLocked, now an assert that the up-front count covers the range) and three not covered (DirtyPages over spilled pages, now tested by TestAnAgreementReservesOnlyForSpilledPagesNotDirty, which kills both mutants by hand). The rest are step 9's survivors on lines this step did not touch.

AC #1 checked on this reading: in the region's layer a spilled page is a Reference in its page list slot, and that reference is the spill storage's; in the old core a binding stays in its slot while spilled (faults, alias sets and seals hold it) and carries the reference as its reservation, until steps 11 and 12 move faults and checkpoints onto the region's layer. AC #3: the properties moved with the code to zirconvm/spillstorage_test.go (TestReservationsCostWhatIsTakenNotWhatIsAdmitted, TestReservationsRunOut and TestAReservationGivenBackHoldsNothing), since the storage is internal to zirconvm; the vmmemory file is deleted.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Completed VmCompression (compression.go: the storage interface with a context, statistics, StoreAsIs in place of LZ4, D5's reserve, store into a reservation and decompress that keeps it) and added the spill file as its storage (spillstorage.go, the shape of slot_page_storage.cc: references are allocation ids handed out lowest first, file truncated and allocated at start, CRC32C checked on read, consecutive references in one write, the spill-sparse and pager-forget-spill guards). D5 in VmCowPages: a page takes its reservation before it is Dirty, keeps it through spill and refault, gives it back when cleaned or freed; DirtyPages reserves up front; an abandon gives each page made Dirty again its own reservation. The pager's reservations.go is deleted and its bindings hold reservations that are references of the storage. Verified by the four ported compression cases, D5 and storage cases at 4 KiB and 2 MiB, the whole suite in both arena modes, -race on vmmemory and zirconvm, check-guards, Gremlins with survivors on changed lines tested, benchmarks within noise, and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
