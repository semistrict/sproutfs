---
id: TASK-21
title: Test a hostile VMM with a real userfaultfd
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 17:28'
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
- [ ] #1 A fuzz target drives a real registered userfaultfd through seal, retire and settle
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. A proxy in the test process between the Rust client's socket and the pager. It forwards frames both ways, with their descriptors, and misbehaves at scripted moments: withholds a command, acknowledges one it did not forward, acknowledges twice, hangs up, injects frames.
2. A fuzz target (and concrete seed cases run as a test) on an isolated-arena hostile fixture: the client stores and reads through a real registered userfaultfd while the test seals, publishes, settles and retires its RAM, and the proxy misbehaves.
3. After each round the fixture's rules hold: the session ends and never hangs, the neighbour keeps its bytes, the pager and host get back what they held.
4. Run in Lima; fuzz for a while; fix what it finds.
<!-- SECTION:PLAN:END -->
