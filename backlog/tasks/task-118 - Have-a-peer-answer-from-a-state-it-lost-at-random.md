---
id: TASK-118
title: 'Have a peer answer from a state it lost, at random'
status: To Do
assignee: []
created_date: '2026-10-08 15:23'
labels:
  - testing
dependencies: []
priority: medium
ordinal: 153000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A request one host makes of another (peer.Peer) can come back not served, stale, not me, no journal, or another generation: the source released a VM, the cache is under another membership, the holder moved its journal disk. The campaigns reach these answers only where their schedules change that state, never at a moment of a fault's choosing, so a request that meets one anywhere else is untested; a peer that restarted or moved on answers them at any moment. scripts/faults/peer.json defers these entries here.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A server-side sim.Buggify site per answer gives it at random, as a peer that restarted or moved on would
- [ ] #2 The campaigns that fire them accept what such a peer costs: a source that lost pages no checkpoint has is a lost source
- [ ] #3 The deferred entries in scripts/faults/peer.json name those sites and campaigns, and check-faults passes
<!-- AC:END -->
