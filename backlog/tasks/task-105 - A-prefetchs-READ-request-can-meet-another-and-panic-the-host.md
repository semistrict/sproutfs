---
id: TASK-105
title: A prefetch's READ request can meet another and panic the host
status: In Progress
assignee: []
created_date: '2026-10-07 18:55'
updated_date: '2026-10-07 19:10'
labels:
  - vmmemory
  - prefetch
dependencies: []
references:
  - vmmemory/prefetch.go
  - vmmemory/pagerequests.go
priority: high
type: bug
ordinal: 135000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedder that runs the pager in its own process saw it panic at 50010ead6 with 'vmmemory: a prefetch's request met another' (prefetch.go sendLocked). The panic is in the pager's own goroutine, so the whole process exited and every VM on the host died. Two VMs forked from one public template faulted 1 ms apart on one host; 1 crash in 9 cold runs. splitPrefetch reads the root's outstanding READ ranges (readingIn / AppendOutstanding) with h.mu released, and sends its own requests later under h.mu (sendLocked). A READ of the same root that starts in between goes unseen, the prefetch asks for pages already requested, and sendLocked panics. sendRead (pagerequests.go) has a matching panic, 'a fault's read request met another read', that should be checked for the same exposure. More widely, the fault path uses panic for invariant checks, and an embedder that runs the pager in its own process loses every VM to one.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 splitPrefetch checks for outstanding READs and sends its own requests under one hold of h.mu
- [x] #2 A prefetch whose GetPages finds part of its run already requested gives those pages back to their reservations and sends only the uncovered runs, instead of panicking
- [x] #3 A regression test runs two regions forked from one root that fault overlapping windows at once with prefetching on, and puts a READ of that root between the filter and the send with a seam or a Buggify site; nothing panics and every page lands once
- [x] #4 sendRead's 'met another read' panic is checked for the same race, and fixed or shown unreachable by a test or an argument in its comment
- [ ] #5 An invariant failure in one VM's fault path fails that VM's memory region rather than the process, or the docs say why it cannot
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. In splitPrefetch, filter the prefetch's pages against the roots' outstanding READ requests under the same hold of h.mu that sends them; a covered page's reservation goes back, and a prefetch left with no page is not made.
2. A seam between the plan and the send lets a test start a READ of the same root there; regression test with two regions forked from one root.
3. Check sendRead's matching panic for the same race.
4. Weigh AC#5, a fault-path invariant failing one region rather than the process, separately once 1-3 are in.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
splitPrefetch now reads the roots' outstanding READs under the same hold of h.mu that sends its requests (sendLocked); a page another read covers goes back to its reservation before the send, so GetPages never meets another request and its panic is now an unreachable invariant. TestTwoForksFaultingOneWindowAtOncePrefetchItOnce puts a second fork's prefetch of the same root between the plan and the send through prefetchSendSeam; guard pager-filter-a-prefetch-unlocked restores the old order and reproduces the panic exactly. sendRead's panic is not exposed: it sends only to the region's own source, which the faults of one window reach one at a time under its stripe. AC#5 (one region's invariant failure not ending the process) is still open.
<!-- SECTION:NOTES:END -->
