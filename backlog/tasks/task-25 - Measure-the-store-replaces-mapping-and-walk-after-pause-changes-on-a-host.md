---
id: TASK-25
title: Measure the store-replaces-mapping and walk-after-pause changes on a host
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
updated_date: '2026-09-26 01:50'
labels:
  - measurement
  - gce
  - deferred
dependencies: []
priority: medium
type: task
ordinal: 25000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**The two costs that the 2026-09-22 GCE run measured are fixed in the
simulation and unmeasured on a host.**
- A store now replaces the mapping it copied from instead of revoking it
  first. So a copy-on-write, and a store that closes a gap or fills a range,
  is one mapping command with no revocation. In that run, the three-fork
  `cargo test` fan-out spent 1,753 s on 5,450,465 revocations over 5,481,191
  pages, which is about one per page the guests wrote.
- A seal's pause now consists only of its write-protect commands. The walk
  that moves each page into the checkpoint runs after the pause, while the
  guest is already running. In that run, the capture of 2,204,672 sealed
  pages paused for 2.14 s, of which 0.18 s was commands.

Exact counts in `vmmemory` and the campaigns prove both changes. No
run has yet measured what they are worth in seconds on a real host. That
requires re-running the same fan-out and the same capture (`revocations`,
`revoked_pages`, `pause_ns`, `seal_ns`, the new `seal_walk_ns`).

**The revocations that the 2026-09-23 fan-out then recorded did not come from
stores.** Its 12,428 revocations against 12,826 copy-on-writes looked like one
per page the forks wrote, but they came from the retire. A published
checkpoint hands back every page the volume holds no object for. Here those
were the write-ahead pages the guest never stored into, 15,477 of them, and
the retire revoked each one separately. The hand-backs of one retire batch are
now checked and revoked together, with one command per run
(`MemoryRegion.revokeHandedBack`, `TestAForksFirstCheckpointRevokesItsHolesInRunsNotPages`).
Two per-page revocations remain, and neither happens in a fork's first
seconds:
- a page whose identity another resident already holds, which arrives alone;
- `Host.dropSharers`, which an abandoned checkpoint and a retired fork point
  use per sharer per page.
<!-- SECTION:DESCRIPTION:END -->
