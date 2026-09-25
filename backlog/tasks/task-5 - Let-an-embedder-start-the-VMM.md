---
id: TASK-5
title: Let an embedder start the VMM
status: Done
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 5000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. It runs Firecracker under a jailer, in a netns and a cgroup, with network interfaces, a read-only tools.ext4 PMEM, extra drives and its own vsock socket, on every path a VM starts: create, open, fork and receive.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A vmmachine.Starter starts every VMM process on create, open, fork and receive
- [ ] #2 A Starter-run VMM in a chroot, as another user, with a read-only PMEM file boots and restores on Lima
<!-- AC:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Done in 029f546 (Starter), aa91280 in the Firecracker fork (read-only drives and PMEM files may stay out of a managed capture), and 3c59230 (chroot test, adversarial Starters, Memory.Configure fuzz, mutation guards). See docs/hosting.md "Running the VMM".
<!-- SECTION:FINAL_SUMMARY:END -->
