---
id: TASK-21
title: Test a hostile VMM with a real userfaultfd
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 17:54'
labels:
  - security
  - testing
dependencies: []
priority: medium
type: task
ordinal: 21000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The hostile-session fuzzing uses fake descriptors, so its sessions end at the first resolve. Seal, retire and settle have not run under hostile timing. It needs a hostile client process, such as a proxy in front of the Rust client.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A fuzz target drives a real registered userfaultfd through seal, retire and settle
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. A proxy in the test process between the Rust client's socket and the pager. It forwards frames both ways, with their descriptors, and misbehaves at scripted moments: withholds a command, acknowledges one it did not forward, acknowledges twice, hangs up, injects frames.
2. A fuzz target (and concrete seed cases run as a test) on an isolated-arena hostile fixture: the client stores and reads through a real registered userfaultfd while the test seals, publishes, settles and retires its RAM, and the proxy misbehaves.
3. After each round the fixture's rules hold: the session ends and never hangs, the neighbour keeps its bytes, the pager and host get back what they held.
4. Run in Lima; fuzz for a while; fix what it finds.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
vmmemory/hostile_client_linux_test.go adds a proxy (hostileProxy) on a real client's RAM control socket. It forwards every frame both ways with its descriptors, so the client's guest faults its RAM in on a real registered userfaultfd. hc.capture drives fill -> wire seal -> Checkpoint().Settle -> publish -> Retire, exercising seal, settle and retire on a live session. The proxy then injects one frame the honest client never sends (unexpected/duplicate ACK, seal id 0, RAM flush, unknown kind, cut-short frame, hang up) and the pager ends the session with its own error while the neighbour keeps its bytes and the pager/host reclaim what the session held. startNativeIn grew a proxy option; nativeProcess grew a reap() so the client's pipe fds are closed before the descriptor-count invariant. Verified in Lima: 7 concrete cases (TestAHostileClientWithARealUFFDIsEndedThroughACapture) PASS in shared and isolated at 4KiB and 2MiB; FuzzHostileClient ran ~45s clean after fixing a proxy fd leak and an unreaped-process fd leak that the fuzzer found.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A proxy in front of the real Rust client forwards a whole session over a real userfaultfd, so the test seals, settles and retires the guest's memory region, then sends the pager a frame the honest client never would. The pager ends the session and leaves its neighbour whole. FuzzHostileClient drives this with a varying number of captures and frames, so seal, retire and settle run under a hostile session, which the made-up descriptors of the existing hostile fuzzing could not reach. Verified in the Lima suite in both arena modes at 4KiB and 2MiB (AC1).
<!-- SECTION:FINAL_SUMMARY:END -->
