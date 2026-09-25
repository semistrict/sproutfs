---
id: TASK-2.3
title: Mapping protocol version 10
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 23:02'
updated_date: '2026-09-25 23:46'
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
- [x] #1 Frame tests on both sides, Rust unit tests and new vmtest cases pass
- [x] #2 The hostile suite and the Firecracker suite pass in Lima
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

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Protocol version 10 in internal/vmwire, vmmemory/connection_linux.go and rust/sproutfs-vm-memory. ATTACH carries no descriptor and length 0. FILE (14): id file number, length bytes, backing arena kind, flags 1 writable / 0 read-only, one descriptor, not acknowledged; a repeat with a larger length grows the file. DROP_FILE (15): id file number, closes the client's descriptor. MAP flags bit 0 immutable, bits above it the file number; a writable MAP must name file 0. The pager sends the arena as FILE 0, read-write.
Rust client: every frame read with recvmsg (Frame::receive), a BTreeMap file table, check_file (fstatfs kind, fstat length >= stated and device/inode identity, fcntl(F_GETFL) access mode), file 0 mapped MAP_SHARED and read-only files MAP_PRIVATE|MAP_NORESERVE, registered MISSING|MINOR|WP and write-protected before exposure; spans may mix files. READY is refused before file 0.
After rebasing onto TASK-2.2: MapRun gains File, Mapping.Map takes the file number, and every run is built from the page's fileSlot (runAt). The session refuses to send a writable MAP of any file but 0. The simulation's mapping reads the file it is given; single-file fixtures refuse others.
Firecracker: branch arena-files at 12be3bbbd off embedder-devices (not pushed). Seccomp for aarch64 and x86_64: memory thread recvmsg, fstat (and newfstatat on aarch64), fstatfs, fcntl F_GETFL; VMM thread fcntl F_GETFL and mmap MAP_PRIVATE|MAP_NORESERVE (16386) and with MAP_FIXED (16402), no PROT_EXEC.
Validation (Lima aarch64, kernel 7.0): cargo fmt check; clippy -D warnings; cargo test --lib 39 passed; the 19 ignored UFFD protocol tests as root passed; go test ./internal/vmwire; internal/vmtest 23 tests pass including the new read-only file cases and both arena layouts; vmmemory Linux suite 726 PASS including TestAHostileSessionEndsAloneAndLeavesItsNeighbourWhole and 45 FuzzHostileSession seeds; Firecracker suite all five tests (DAX capture/restore/fork/fence, live migration, fan-out, children surviving, refusals). Removing MAP_NORESERVE makes TestAReadOnlyFilePageIsThePagersOwn fail with one pool page reserved.
Unproven: no pager sends a file mid-session yet, so the memory thread's fstat/fstatfs/fcntl rules and the VMM thread's private mmap rules are not exercised under Firecracker's seccomp; the Go session code has no sender for mid-session FILE or DROP_FILE yet (the vmtest fixture sends both through the real client).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Mapping protocol version 10: ATTACH without a descriptor, FILE and DROP_FILE frames, and the file number in MAP, on both ends. The Rust client keeps a checked file table, maps read-only files private with MAP_NORESERVE and write-protected before exposure, and reads every frame with recvmsg. The pager still sends the arena as file 0 and maps every page from it, with the file number taken from each page's fileSlot. Firecracker rebuilt on arena-files (12be3bbbd) with the seccomp change. Proven by Rust unit and root protocol tests, frame tests on both sides, new vmtest cases through the real client, the hostile suite and fuzz seeds, and the whole Firecracker suite in Lima.
<!-- SECTION:FINAL_SUMMARY:END -->
