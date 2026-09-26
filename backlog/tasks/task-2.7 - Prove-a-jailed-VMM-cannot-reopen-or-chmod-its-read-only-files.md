---
id: TASK-2.7
title: Prove a jailed VMM cannot reopen or chmod its read-only files
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-26 15:26'
updated_date: '2026-09-26 17:54'
labels:
  - security
dependencies: []
parent_task_id: TASK-2
priority: medium
ordinal: 46000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The reach test plays a VMM in the pager's own user, so it cannot show what only a jailed VMM is held to. A VMM in the embedder's jailer runs as another user. Through /proc/self/fd it could try to reopen a read-only descriptor for writing, or fchmod it, and either would undo the isolated arena's read-only shared files. The plan (plans/isolated-arena-2026-09-25.md, the reach test section) calls for a helper process that runs as another user.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A helper process running as another user holds the files a session gives it, and each attempt to reopen a read-only file for writing through /proc/self/fd fails
- [x] #2 Each fchmod of a file it was given fails
- [x] #3 It runs in the Lima suite in isolated mode
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Move the copy-the-test-binary-for-another-user helper out of internal/vmtest into a small shared test package, and use it from vmtest.
2. Add a jailed helper role to the vmmemory test binary: run as nobody, holding the files it is started with, it tries to reopen each through /proc/self/fd for writing and fchmod each, and reports the errno of each attempt.
3. In TestAHostileVMMReachesNoOtherVMsBytes, hand the reacher's files to the helper and require EACCES for every read-only file's reopen and EPERM for every fchmod.
4. Run it in Lima in both suite modes (the test itself is isolated).
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
internal/testjail holds the copy-the-test-binary-and-run-as-another-user helper (formerly in internal/vmtest). vmmemory/jail_linux_test.go adds a TestJailedVMM role: run as nobody holding the session's files, it reopens each through /proc/self/fd O_RDWR and fchmods each, reporting the errno of both. TestAHostileVMMReachesNoOtherVMsBytes now hands the reacher's files to it and requires EACCES for each read-only reopen, EPERM for each fchmod, and mode still 0600. Verified in Lima isolated: PASS at 4KiB and 2MiB. Mutation check: running the helper as the pager's own user (owner nil) makes the assertions fail (reopen and fchmod succeed), so the test is not vacuous.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A helper process run as nobody holds the files a session gave the reach test's hostile VMM. Its reopening of each read-only file for writing through /proc/self/fd fails EACCES, its fchmod of each fails EPERM, and every file stays mode 0600. The copy-and-run-as-another-user helper moved to internal/testjail and internal/vmtest now uses it. Verified by TestAHostileVMMReachesNoOtherVMsBytes in the Lima suite in isolated mode at 4KiB and 2MiB (AC1, AC2, AC3).
<!-- SECTION:FINAL_SUMMARY:END -->
