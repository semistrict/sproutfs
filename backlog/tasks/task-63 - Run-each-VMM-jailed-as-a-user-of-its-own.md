---
id: TASK-63
title: Run each VMM jailed as a user of its own
status: Done
assignee:
  - '@claude'
created_date: '2026-09-28 01:36'
updated_date: '2026-09-28 03:59'
labels:
  - security
dependencies: []
priority: high
ordinal: 70000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The isolated arena (now the default) protects a VM's memory only from a VMM that runs as a user other than the host's, not root, and without CAP_DAC_OVERRIDE, CAP_FOWNER or CAP_SYS_PTRACE (plans/isolated-arena-2026-09-25.md, 'What the embedder must do'). vmmachine.Firecracker, which the deployment's host uses, runs Firecracker as root and unjailed, so the demo deployment isolates nothing.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 vmmachine.Firecracker can run each VMM chrooted into a jail the host prepares (binary, seccomp filter, kernel, its own kvm and userfaultfd nodes) as a user of its own from a configured range, and the host's deployment does so by default
- [x] #2 A memory session whose peer is root, or not the user its Placement names, is refused
- [x] #3 The Firecracker suite passes with every VMM jailed on GCE x86_64 in both arena modes, and a test shows two VMMs run as different non-root users
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE x86_64, both arenas, every VMM jailed: TestJailedVMMsRunAsUsersOfTheirOwn and TestAdversarialStarters (root peer refused) pass. Four other tests failed: socket paths under the jail passed the 108-byte limit (the per-VMM directory is now named by its start number), and TestAFailedReleaseIsReportedOnce's helper ran as root under a placement naming 65534 (it now drops to that user). A narrowed jailed rerun of those tests is running.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Each VMM runs chrooted into a jail the host builds, as a user of its own from a range, and the deployment does so by default; a memory session from root or the wrong user is refused. Verified on GCE x86_64 in both arenas with every VMM jailed: the full suite, then the four tests the jail broke (fixed: shorter socket paths, the adversarial fake drops to its user) rerun with TestJailedVMMsRunAsUsersOfTheirOwn and TestAdversarialStarters, all passing. Recorded in docs/measurements/firecracker-x86_64-2026-09-27.md.
<!-- SECTION:FINAL_SUMMARY:END -->
