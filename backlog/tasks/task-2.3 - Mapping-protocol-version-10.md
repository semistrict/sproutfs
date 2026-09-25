---
id: TASK-2.3
title: Mapping protocol version 10
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 23:02'
updated_date: '2026-09-25 23:09'
labels:
  - security
dependencies: []
parent_task_id: TASK-2
priority: high
type: task
ordinal: 39000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 3 of plans/isolated-arena-2026-09-25.md: FILE, DROP_FILE and the file number in MAP; the Rust client's file table and private mappings of read-only files; Firecracker rebuilt with the crate and the seccomp change. The pager still sends one read-write file.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Frame tests on both sides, Rust unit tests and new vmtest cases pass
- [ ] #2 The hostile suite and the Firecracker suite pass in Lima
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. internal/vmwire: Version 10; FILE (14) and DROP_FILE (15) kinds; ATTACH without a descriptor and with zero length; FileFrame, DropFileFrame, MapFlags with the file number above the immutable bit; frame tests.
2. vmmemory/connection_linux.go: send ATTACH then FILE 0 (the arena, read-write); MAP flags through MapFlags on file 0. Go test peers that stand in for a client read ATTACH then FILE 0.
3. Rust client: version 10; one frame reader over recvmsg for every frame, which takes the descriptors a frame carries; a file table checked by kind, size, access mode and inode; file 0 read-write and mapped MAP_SHARED, other files read-only and mapped MAP_PRIVATE with MAP_NORESERVE, registered and write-protected before exposure; MAP naming an absent file, an offset past its length, or a writable MAP of another file refused; DROP_FILE closes the descriptor. Unit tests and ignored protocol tests.
4. internal/vmtest: the fixture sends ATTACH and FILE 0, and can send a read-only file. New cases map read-only files through the real client: physical sharing with the pager's page, stores from a thread and from KVM trapping to the pager, no HugeTLB reservation, refused MAPs, growth and DROP_FILE.
5. Firecracker fork: branch arena-files off embedder-devices; seccomp for recvmsg, fstat, fstatfs and fcntl(F_GETFL) on the memory thread, fcntl(F_GETFL) and private file mmaps on the VMM thread, aarch64 and x86_64; rebuild; bump the gitlink.
6. docs/vm-memory.md protocol section for version 10.
7. Prove: cargo test/clippy/fmt, ignored protocol tests, frame tests, vmtest in Lima, hostile suite and FuzzHostileSession seeds in Lima, Firecracker selection in Lima.
<!-- SECTION:PLAN:END -->
