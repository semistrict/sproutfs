---
id: TASK-63
title: Run each VMM jailed as a user of its own
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-28 01:36'
updated_date: '2026-09-28 01:36'
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
- [ ] #1 vmmachine.Firecracker can run each VMM chrooted into a jail the host prepares (binary, seccomp filter, kernel, its own kvm and userfaultfd nodes) as a user of its own from a configured range, and the host's deployment does so by default
- [ ] #2 A memory session whose peer is root, or not the user its Placement names, is refused
- [ ] #3 The Firecracker suite passes with every VMM jailed on GCE x86_64 in both arena modes, and a test shows two VMMs run as different non-root users
<!-- AC:END -->
