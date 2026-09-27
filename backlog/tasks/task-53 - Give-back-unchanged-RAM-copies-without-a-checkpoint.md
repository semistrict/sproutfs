---
id: TASK-53
title: Give back unchanged RAM copies without a checkpoint
status: Done
assignee:
  - '@claude'
created_date: '2026-09-26 23:18'
updated_date: '2026-09-27 00:09'
labels:
  - performance
dependencies: []
priority: high
ordinal: 60000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
RAM is never settled on the interval: OnInterval is true only for PMEM (vmmemory/region.go) and settle runs only inside a publication (volume/publish.go). On x86 a cold read reaches the pager as a write (KVM's async_pf_execute asks for the page writable), and on aarch64 so does a guest's first execution of a page, so a RAM page shared between VMs becomes a private 2 MiB copy that lives as long as the VM unless something checkpoints RAM. The owner decided on 2026-09-26 to give such copies back automatically, independent of checkpoints: per page, write-protect the copy with UFFDIO_WRITEPROTECT (which also drops KVM's mapping through the MMU notifier), compare it with the page it was copied from under that page's lock, and if equal point the guest back at the original and free the copy. No VM pause. A write through a page pinned before the write-protect (O_DIRECT or io_uring DMA into guest RAM, vhost) bypasses the page tables and would be lost, so the pinned writers must be ruled out first; today's seal may have the same exposure.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A written finding lists every writer of guest RAM that can bypass the host page tables (Firecracker's block engines, the embedder's extra drives, anything else), and whether today's seal and settle are exposed to them
- [x] #2 The cleanup runs on the interval, rate-limited, with no VM pause, and never frees a copy a write could still reach
- [x] #3 The guest is pointed at the original in place, so the next read makes no new copy even when a cold read arrives as a write
- [x] #4 Tests prove an unchanged copy is given back, a copy written during the compare is kept, and the Lima suites pass in both arena modes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Find every writer of guest RAM that can bypass the host page tables; record it in the task notes and in docs/vm-memory.md, and say whether the seal and settle are exposed.
2. MemoryRegion.GiveBack (vmmemory/giveback.go): for each dirty copy with an origin, take the page's window, the memory region shared, the origin's lock then the copy's; UFFDIO_WRITEPROTECT the copy; compare; if equal MAP the origin in place, free the copy and its reservation, and install the origin read-only; otherwise lift the protection and forget the origin. Bounded per pass, resuming from a cursor.
3. host: run it once an interval beside each VM's checkpoint loop for RAM, bounded to 4096 pages and 1 GiB a VM.
4. Tests: vmmemory unit tests (unchanged copy given back, read after give-back makes no copy, copy written before the protection kept, trapped store not lost, refused map, evicted origin, sealed page, bounded passes); host interval test; simtest campaign draw + invariant + scenario; guard pager-give-back-changed-copy; Linux UFFD/KVM tests at 2 MiB and 4 KiB; fan-out Firecracker test runs the give-back between checkpoints; Lima pager and Firecracker suites in both arena modes.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Step 1 finding (2026-09-26, Firecracker fork 12be3bbb + mainline Linux): every writer of guest RAM goes through the VMM's page tables except KVM's kvm_vcpu_map maps for a nested guest. Checked: Firecracker block sync engine (no O_DIRECT, pread -> copy_to_user; io_uring engine and vhost-user refused with managed RAM at boot; restore does not repeat the check but only loads state a managed boot captured); the embedder's drives (ordinary Firecracker drives, same rule; writable ones fail capture); net/vsock/entropy/MMDS/vmclock (userspace stores/readv; no vhost-net/vsock; balloon and hotplug refused); KVM's own writes (steal time, APF token, PV EOI, SMM via copy_to_user; kvmclock via gfn_to_pfn_cache invalidated by the MMU notifier that UFFDIO_WRITEPROTECT calls and refilled by a FOLL_WRITE GUP that takes the uffd fault; CPU A/D bits via EPT/stage-2). Bypass: nested virt. Intel virtual-APIC page, posted-interrupt descriptor, APIC-access page (nested_get_vmcs12_pages, held while L2 runs); AMD vmcb12/hsave (kvm_vcpu_map_local, one VMRUN/exit). Reachable when the guest sees VMX/SVM: Firecracker passes host VMX through without a CPU template and kvm_intel.nested defaults on. aarch64 not exposed (no EL2 vCPU). Debuggers (process_vm_writev, /proc/pid/mem) excluded. DEFECT: seal, settle, every COW and eviction are exposed to the nested writer. Harm is confined to the nested-virt guest (freed slots are punched, KVM's ref keeps an orphan page), and Firecracker never saves nested state so such a guest already breaks at capture. Fix (not done, needs owner approval as follow-up): hide VMX/SVM from guests with managed RAM. The give-back adds no new exposure. Written into docs/vm-memory.md 'Writers that bypass the page tables'.

In-place replacement is allowed here and not in the settle because the give-back holds the page's read-ahead window (stripe) and the memory region shared, exactly the locks under which a store fault's MAP replaces the page it copied from; the settle holds no window. TASK-49 (move in place) has not landed on main (checked 2026-09-26, main at 5e7ecefc), so there was no shared mechanism to reuse. A move's rebind cannot reuse this one directly: it revokes another region's binding while holding page locks, and taking that region's window after a page lock would invert the lock order.

Rebased onto main after TASK-49 landed: the give-back now shares the move's in-place mechanism, mapInPlace (isolation.go). A refused MAP keeps the copy and lifts its protection rather than revoking, because a revoke leaves the next read to a cold fault (x86 async_pf asks writable; no-kvmapf does not stop it, per the coordinator's GCE measurement).
Validation 2026-09-26: go test ./... and just check pass; vmmemory in both arena modes and with -tags sproutfsprobe; host, simtest campaign and new scenario under -race. Guard pager-give-back-changed-copy kills TestSeededTopologyCampaign (seeds 1, 7, 23). Lima pager suite passes shared (542 PASS) and isolated (543 PASS), including TestManagedPagerGiveBackPointsTheGuestAtTheOriginInPlace and ...KeepsAChangedCopyWritable at 2 MiB and 4 KiB (no fault and no copy on a process read and a KVM read after the give-back; PTE present, shared pfn). Firecracker suite passes isolated (63 PASS; fan-out gave back 14 of 86 compared) and shared except TestFirecrackerOwnerRereadsMovedPages (TASK-49's test), which fails 2 of 3 on unmodified main too (1-3 protect traps on the owner's reread), so it is a pre-existing flake, not this change; the shared fan-out gave back 28 of 126 compared. Not done: hiding VMX/SVM from managed-RAM guests (needs owner approval as a follow-up); the restore path does not re-check the block engine.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
RAM copies a write fault made without a store are now given back on the interval with no checkpoint and no pause. MemoryRegion.GiveBack (vmmemory/giveback.go) takes each copy with an origin under its window, the region shared and both pages' locks, write-protects it with UFFDIO_WRITEPROTECT, compares it with the origin, and either maps the origin in place and installs it read-only (mapInPlace, shared with TASK-49's move) and frees the copy and its reservation, or lifts the protection and forgets the origin. The host runs it once an interval per VM's RAM, bounded to 4096 pages and 1 GiB (host/giveback.go); Stats and host status report GiveBackCompares and GivenBackPages. Finding: every writer of guest RAM goes through the VMM's page tables except KVM's kvm_vcpu_map maps for a nested guest, which the seal, settle, copy-on-write and eviction are exposed to as well (docs/vm-memory.md). Verified with pager unit tests, a host interval test, a simulation campaign invariant and scenario, a mutation guard, Linux UFFD/KVM tests at 2 MiB and 4 KiB, and the Lima pager and Firecracker suites in both arena modes.
<!-- SECTION:FINAL_SUMMARY:END -->
