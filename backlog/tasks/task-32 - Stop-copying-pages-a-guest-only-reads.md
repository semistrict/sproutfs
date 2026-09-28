---
id: TASK-32
title: Stop copying pages a guest only reads
status: Done
assignee: []
created_date: '2026-09-25 18:18'
updated_date: '2026-09-28 02:02'
labels:
  - performance
  - deferred
dependencies: []
priority: medium
type: enhancement
ordinal: 32000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**A page that a guest only reads is copied, and the copy is released at the next checkpoint.** A cold read that has to wait for the pager arrives as a write fault. On x86-64 this happens because KVM's asynchronous page fault worker always requests the page as writable. On aarch64 it happens when the guest first executes a page. The pager answers a write fault with a private page. The part that the [unchanged-page rule](../plans/unchanged-pages-2026-09-19.md) recovers is done. The copy records the page it was made from, and the settle after each checkpoint's pause compares the two. A page that did not change is published nowhere and goes straight back to sharing its origin. The copy itself remains. Between the fault and the next checkpoint, the host holds the page twice. With 4 KiB RAM pages under a 2 MiB read-ahead run, that is one page in 512. For PMEM at 2 MiB, it is a whole page per cold fault until the interval passes. Preventing the copy requires a host kernel that passes the guest's access through, or KVM userfault once it exists. Neither is this project's to start. Fork points are not settled either. A child inherits an unchanged page as an unpublished page, and the child's own next checkpoint settles it.

**The copy is one page per fault and no more. The 2026-09-23 fan-out's counts show this, and the suite now asserts it.** 12,826 copy-on-writes over 13,226 faults is one copy per store-served fault. The pages a store copies beyond the one it faulted on come from the two rules. They are counted separately as `Stats.RuleCopies`, which was missing from the record until now. `TestAForksFirstStoresRevokeNothing` checks this at 4 KiB. It attaches a memory region over a sibling's resident pages. It then stores into a page the populate mapped, into a page the guest has never touched, and into a page inside the window a read brought in. Each store makes exactly one page private and copies nothing for the rules. So what remains of a fork's first pass is the number of faults, not what each fault copies: 13,226 faults at a mean of 1.01 ms. The levers for the fault count are a window larger than the free arena slots a fault can reserve, and a populate of the fork point's hot set instead of whatever pages a sibling happens to hold. Neither is done.
<!-- SECTION:DESCRIPTION:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Resolved by TASK-59 on 2026-09-27. A copy made on a write fault to an unmapped published page is now a cold copy: the session gives it back about 200 ms after it is made (coldCopyAge), and every seal and eviction leaves out one whose bytes are still its origin's, so the page is held twice for that long rather than until the next checkpoint, and is never uploaded. Not making the copy at all still needs a kernel that passes the guest's access through, or KVM userfault, which is not this project's to start. The fork first-pass fault count (13,226 faults at 1.01 ms) is a separate matter and not tracked here.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Superseded by TASK-59: a read-only guest's copies are given back about 200 ms after they are made instead of at the next checkpoint, and no seal uploads them. Verified by TASK-59's pager tests with a real userfaultfd and its Lima and x86 GCE runs, which count zero copies left after cold write faults of pages that were only read.
<!-- SECTION:FINAL_SUMMARY:END -->
