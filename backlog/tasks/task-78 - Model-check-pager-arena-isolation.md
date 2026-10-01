---
id: TASK-78
title: Model-check pager arena isolation
status: Done
assignee:
  - '@claude'
created_date: '2026-10-01 05:37'
updated_date: '2026-10-01 06:43'
labels:
  - formal
  - security
dependencies: []
priority: low
type: feature
ordinal: 85000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The pager hands each VMM files: a private file, its tenant shared file, the public file and fork files. It moves and revokes pages between them (docs/vm-memory.md). A VMM runs untrusted guest code, so the safety property is that no VMM can ever map a page of another tenant, a property the hostile-VMM tests sample but do not exhaust.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A TLA+ spec models the arena files, sharing joins and leaves, moves, revocations and the public file
- [x] #2 TLC checks that no VMM maps a page of a VM of another tenant, except a public template page
- [x] #3 Small configurations run in just check-spec within a few minutes, with mutants; real defects are recorded in spec/bugs.md
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
No real code defect found. The model covers which file each page goes in, and which descriptors each VMM holds. A compromised VMM keeps every descriptor. Every file is a fresh memfd that is closed when dropped and never reused; LinuxArena.NewFile makes a memfd per file, and dropFileLocked closes it. Mutants no-mayread, fork-across-tenants and pooled-files are caught. A public page moved into a tenant's shared file, the defect the simulation found earlier, is not an isolation breach, so it is not a mutant here.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added spec/arena/Arena.tla: the private, tenant shared, public and fork files; loads by identity under mayRead; moves; and fork points lending only to children of the parent's tenant. VMMs keep every descriptor they are given. The invariant Isolated holds over 20,000 states. Three mutants are caught: a load without mayRead, a fork across tenants, and an arena that reuses closed files. just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
