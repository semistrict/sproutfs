---
id: TASK-49
title: Map a moved page read-only to its owner so a move saves memory
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-26 22:14'
updated_date: '2026-09-26 23:51'
labels:
  - performance
  - security
dependencies: []
priority: high
ordinal: 56000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
In the isolated arena, a published page another region inherits is moved into the shared file and the owner's mapping is revoked (vmmemory/isolation.go, move). The GCE worst-case run of 2026-09-26 (docs/measurements/arena-worst-case-2026-09-26.md) found that the owner's next access to each moved page arrives as a store and makes a private copy: 403 copies for 428 moves. So a move saves no memory (812 MiB saved against 1248 MiB shared), and at 4 KiB the host pod reached 7985 MiB of its 8 GiB. Why the fault arrives as a store is not known.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The owner's first read of a moved page maps the shared copy and makes no private copy
- [x] #2 A pager test and a Lima test count zero copies after a move followed by owner reads
- [x] #3 The worst-case run's saved memory in isolated mode matches shared mode within 5%
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Lima (aarch64, Firecracker, isolated arena, 2 MiB) does not reproduce the copies. New counters split each UFFD fault by what the kernel reported (read / store into a page not in the page tables / store into a write-protected page) and count the copies made for a page the guest did not map. vmmachine TestFirecrackerOwnerRereadsMovedPages before any fix: 35 moves; the owner's reread took 24 faults: 23 read traps, 0 store traps, 1 protect trap; 1 copy-on-write, from the protect trap (a real store into a sealed page), 0 copies of an unmapped page. So on aarch64 the owner's refaults of moved pages are reads and are mapped from the shared copy. arm64 KVM has no asynchronous page faults; x86-64 KVM finishes a fault that waited from a worker that asks for the page writable (docs/vm-memory.md already records this for forks). Suspect (a): on GCE each refault of a revoked moved page arrives as a store trap. Next: a GCE run with the counters to confirm.

Cause, proven on GCE x86_64 (2026-09-26, isolated arena, 2 MiB, first-inheritance case, unfixed build with the new counters, n=2): the owner's reread took 408.5 faults: 404 store traps (UFFD WRITE without WP, on pages not in the page tables), 2.5 read traps, 2 protect traps; 401.5 of its 403.5 copy-on-writes were of a page it did not map. KVM's kvm_try_async_get_page tracepoint fired 517.5 times on the node during that reread. A capture after the reread found 394 of the copies unchanged, so the guest never stored into them. So (a): the move revoked the owner's mapping, each reread was a cold fault, KVM finished it from its async worker, which asks for the page writable (virt/kvm/async_pf.c, FOLL_WRITE), and the pager copied. Hiding async PF from the guest does not help: with no-kvmapf on the guest command line the numbers are the same (401.5 store traps, 397.5 unmapped copies, 522 async faults): KVM still finishes the fault from the worker and halts the vCPU when it cannot tell the guest. Fix: a move replaces the owner's mapping with the read-only shared copy and installs it in the page tables (MAP + CONTINUE), so no cold fault happens; a refused MAP falls back to the revocation.

Fixed build on GCE x86_64 (2026-09-26, 2 MiB, case 2, n=3): isolated owner reread 16 faults, 4 store traps, 0 copies of unmapped pages, 10 copy-on-writes (all protect traps, real stores), 17 KVM async faults, 1.60 s; shared 17 faults, 12 copies, 1.61 s. Saved memory at the end: isolated 1656 MiB, shared 1658 MiB (AC 3, within 0.2%). Lima: vmmachine TestFirecrackerOwnerRereadsMovedPages 0 faults, 0 copies for 35 moves; vmmemory TestAPublishedPageMovesIntoTheSharedFileWhenAnotherRegionInheritsIt 0 faults, 0 copies; refusal falls back to revoking (TestAMoveTheOwnersClientRefusesToMapTakesTheMappingAway). no-kvmapf on the guest (verified in /proc/cmdline) changes nothing: unfixed 401.5 store traps, 397.5 unmapped copies. Passed: go test ./..., just check, Lima pager and Firecracker suites in shared and isolated. Not measured: 4 KiB (the owner's MAP per moved page may reach its client's mapping budget there, which falls back to the revocation and its copies).
<!-- SECTION:NOTES:END -->
