---
id: TASK-87
title: >-
  Give every fork child and restored guest a new generation ID and a corrected
  clock
status: Done
assignee:
  - '@claude'
created_date: '2026-10-04 19:21'
updated_date: '2026-10-04 21:15'
labels:
  - vm-memory
  - firecracker
dependencies: []
references:
  - 'https://pinggy.io/amp/blog/edge_functions_isolates_to_microvms/'
  - third_party/firecracker/CHANGELOG.md
priority: high
type: bug
ordinal: 94000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Every child of one fork point resumes from the same VMM state, so its kernel random pool, its userspace RNG seeds and anything it derived from them start identical in every child. A restore from a checkpoint repeats them too. Firecracker in third_party has a VMGenID device and a VMClock device for exactly this, but nothing in the host, vmmachine or the docs makes sure they are enabled, that the guest kernel acts on them, or that the clock jump after a long stop is told to the guest. Netlify names VMGenID and VMClock as the fix for repeated snapshot restores.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Two children of one fork point read different bytes from /dev/urandom right after they resume, shown by a Firecracker test on GCE
- [x] #2 A guest restored after a stop sees the VMGenID change and a wall clock that is right within a stated bound
- [x] #3 docs/vm-memory.md states which devices a VM gets, what the guest kernel must support, and what a guest userspace must do itself
- [x] #4 The host refuses, or warns once per VM, when a VM is started without the device the docs require
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Findings: Firecracker attaches VMGenID and VMClock on every boot (builder.rs attach_vmgenid_device/attach_vmclock_device), both arches; every /snapshot/load makes a new generation ID and bumps VMClock (device_manager/persist.rs). Every fork child, checkpoint restore and migration destination goes through /snapshot/load (vmmachine restore). The pinned guest kernel (CI 6.18.44) has CONFIG_VMGENID and VMCLOCK built in.
2. Decide: a migration also takes a new generation, because a handoff state can be resumed twice.
3. Clock: send clock_realtime on x86_64 restores; the fork pairs the guest clock with the wall clock at save where KVM does not, and moves every vCPU's TSC on with kvmclock (the guest's clocksource is tsc). API revision 2.
4. Host refuses a kernel without the vmgenid driver and boot args with acpi=off/ht (x86_64). arm64 warns at each restore.
5. Test guest entropy command; Firecracker tests on GCE; unit tests and sim.Bug guards; docs and measurement note.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE (n2-standard-8, nested KVM, host clocksource tsc): guest clocksource is tsc, so KVM_CLOCK_REALTIME alone did not move the guest's clock (run 1: children 0.51/0.87 s behind their parent). Moving the TSC on fixed it. Reseed window on x86_64: 2 of 9 restored guests answered first before the reseed (seen 20 ms later); 18 of 18 fork children had reseeded by their first answer. Guests boot 25-46 ms behind the host; restores keep that offset within the console's 20 ms. Regression: fan-out, fork lives (0 of 20 died), kept checkpoint, live migration, DAX capture/restore all pass on GCE. just check exit 0. The fork commit acd85356e is on sproutfs in the worktree's own clone of the submodule and must be fetched from there before merging.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Every restore (fork child, checkpoint open, migration destination) gets a new VMGenID generation from Firecracker; on x86_64 the host now sends clock_realtime and the fork (API revision 2) pairs kvmclock with the wall clock at save and moves kvmclock and every vCPU's TSC on by the time stopped. The host refuses a kernel without CONFIG_VMGENID built in or boot args that hide the device. Verified by new Firecracker tests on GCE (children draw distinct bytes and reseed; a guest opened 10 s after a stop reseeds and keeps its clock offset within 20 ms), the fork's own KVM tests, unit tests with two sim.Bug guards, and just check. Documented in docs/vm-memory.md and docs/measurements/gce-generation-2026-10-04.md.
<!-- SECTION:FINAL_SUMMARY:END -->
